//go:build linux

package epoll

// celeris#1381, the part of the change the black-box tests of idle_wait_1381
// cannot reach: the predicate that decides a loop holds nothing, one row per
// thing a loop can hold or be owed; the loop that has no eventfd; and the state
// of the sweep timer (slow while the loop holds nothing, back to 25 ms with
// any connection, slow again when the last one has gone).

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/deferlinger"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/wakefd"
)

// quiescentLoop1381 is a bare Loop that holds nothing: the base of the table. It
// needs the three descriptors only as numbers (the predicate tests them for
// >= 0) and a real eventfd for the handle.
func quiescentLoop1381(t *testing.T) *Loop {
	t.Helper()
	efd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
	if err != nil {
		t.Fatalf("eventfd: %v", err)
	}
	l := &Loop{listenFD: 3, timerFD: 4, wakeFD: wakefd.New(efd), acceptPaused: new(atomic.Bool)}
	t.Cleanup(l.wakeFD.Close)
	return l
}

// TestIdleQuiescentIsFalseForAnythingTheLoopHolds1381 pins what "no live
// connection" is: the loop that holds nothing is quiescent, and each row below
// takes it out of it. A row is one thing a loop can hold, or be owed, or lack.
func TestIdleQuiescentIsFalseForAnythingTheLoopHolds1381(t *testing.T) {
	ctx := context.Background()
	if !quiescentLoop1381(t).idleQuiescent(ctx) {
		t.Fatal("PREMISE: a loop that holds nothing is not quiescent")
	}
	for _, tc := range []struct {
		name string
		mut  func(l *Loop)
	}{
		{"no listener (paused, standby, stopping)", func(l *Loop) { l.listenFD = -1 }},
		{"no sweep timerfd", func(l *Loop) { l.timerFD = -1 }},
		{"no wake eventfd", func(l *Loop) { l.wakeFD = wakefd.New(-1) }},
		{"listen backlog owed", func(l *Loop) { l.listenHot = true }},
		{"a connection counted", func(l *Loop) { l.connCount = 1 }},
		{"a live connection (HTTP/1, HTTP/2 and detached conns all are)", func(l *Loop) { l.liveConns = []*connState{{}} }},
		{"an HTTP/2 connection", func(l *Loop) { l.h2Conns = []int{7} }},
		{"a detached connection", func(l *Loop) { l.detachedCount = 1 }},
		{"a dirty connection", func(l *Loop) { l.dirtyHead = &connState{} }},
		{"a driver read turn owed", func(l *Loop) { l.driverReadQ = []*driverConn{{}} }},
		{"a pause linger running", func(l *Loop) { l.lingerUntil = deferlinger.After(time.Second) }},
		{"a transplant drain set", func(l *Loop) { l.transplant.Store(&transplantState{}) }},
		{"the sweep still on a drain's epoch", func(l *Loop) { l.sweepTS = &transplantState{} }},
		{"residue published and not yet retracted", func(l *Loop) { l.sweepPub[0] = 1 }},
		{"a hand-off to the other engine in flight", func(l *Loop) { l.transplantInFlight = 1 }},
		{"HTTP/2 pool handlers being waited out", func(l *Loop) { l.h2DrainStart = time.Now() }},
		{"a driver connection registered", func(l *Loop) { l.hasDriverConns.Store(true) }},
		{"the detach queue not empty", func(l *Loop) { l.detachQPending.Store(1) }},
		{"the adopt queue not empty", func(l *Loop) { l.adoptQPending.Store(1) }},
		{"a transplant ask queued", func(l *Loop) { l.xferAskPending.Store(1) }},
		{"accept paused", func(l *Loop) { l.acceptPaused.Store(true) }},
		{"a request count not flushed", func(l *Loop) { l.reqBatch = 1 }},
		{"received bytes not flushed", func(l *Loop) { l.bytesReadBatch = 1 }},
		{"sent bytes not flushed", func(l *Loop) { l.bytesWrittenBatch = 1 }},
		{"a test has disarmed the timer by hand", func(l *Loop) { l.idleWaitOff.Store(true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := quiescentLoop1381(t)
			tc.mut(l)
			if l.idleQuiescent(ctx) {
				t.Errorf("a loop with %q is quiescent: it would take the long wait", tc.name)
			}
		})
	}
	t.Run("the loop's context is cancelled", func(t *testing.T) {
		c, cancel := context.WithCancel(ctx)
		cancel()
		if quiescentLoop1381(t).idleQuiescent(c) {
			t.Error("a loop whose context is cancelled is quiescent")
		}
	})
}

// TestIdleWaitTier1381 pins the wait: the last tier is 4 ms for a loop that
// has not taken the slow timer, and the idle wait (100 ms) for one that has; a
// dirty list or a missing eventfd with an HTTP/2 conn still win.
func TestIdleWaitTier1381(t *testing.T) {
	l := &Loop{consecutiveEmpty: 500, wakeFD: wakefd.New(-1)}
	if got := l.adaptiveTimeoutMs(1); got != 4 {
		t.Errorf("last tier, timer at the floor: %d ms, want 4", got)
	}
	l.timerSlow = true
	if got := l.adaptiveTimeoutMs(1); got != idleWaitMs {
		t.Errorf("last tier, timer slow: %d ms, want %d", got, idleWaitMs)
	}
	l.consecutiveEmpty = 50
	if got := l.adaptiveTimeoutMs(1); got != 2 {
		t.Errorf("middle tier: %d ms, want 2 (the slow timer belongs to the last tier)", got)
	}
	l.consecutiveEmpty = 500
	l.dirtyHead = &connState{}
	if got := l.adaptiveTimeoutMs(1); got != 0 {
		t.Errorf("dirty list: %d ms, want 0", got)
	}
	if idleWaitMs != 100 {
		t.Errorf("idleWaitMs is %d, want 100: the sweep timer's slow period is the same constant", idleWaitMs)
	}
}

// dial1381 opens a keep-alive connection, has one request answered on it, and
// returns it open.
func dial1381(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := fmt.Fprint(c, "GET / HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("response: %v", err)
	}
	_ = resp.Body.Close()
	return c
}

// stateProbe1381 records, from the loop's own thread at each of its sweeps,
// whether the loop's timer is slow and whether it holds a connection.
type stateProbe1381 struct {
	mu    sync.Mutex
	slow  map[*Loop]bool
	live  map[*Loop]bool
	ticks map[*Loop]int
}

func newStateProbe1381(t *testing.T) *stateProbe1381 {
	t.Helper()
	p := &stateProbe1381{slow: map[*Loop]bool{}, live: map[*Loop]bool{}, ticks: map[*Loop]int{}}
	h := func(l *Loop) {
		p.mu.Lock()
		p.slow[l] = l.timerSlow
		p.live[l] = len(l.liveConns) > 0
		p.ticks[l]++
		p.mu.Unlock()
	}
	testHookCheckTimeouts.Store(&h)
	t.Cleanup(func() { testHookCheckTimeouts.Store(nil) })
	return p
}

func (p *stateProbe1381) state(l *Loop) (slow, live bool, ticks int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.slow[l], p.live[l], p.ticks[l]
}

// waitState1381 polls the probe until cond holds for the loop, or fails after
// 20 s: the loops sweep themselves, so a condition on what a sweep saw is
// reached by waiting for an event.
func waitState1381(t *testing.T, p *stateProbe1381, what string, loops []*Loop, cond func(slow, live bool) bool) {
	t.Helper()
	for dl := time.Now().Add(20 * time.Second); time.Now().Before(dl); time.Sleep(2 * time.Millisecond) {
		ok := false
		for _, l := range loops {
			if s, lv, _ := p.state(l); cond(s, lv) {
				ok = true
				break
			}
		}
		if ok {
			return
		}
	}
	t.Fatalf("celeris1381: %s: not reached in 20 s", what)
}

// TestSweepTimerFollowsWhatTheLoopHolds1381: an idle loop takes the slow timer;
// the connection that arrives gives its loop the 25 ms timer back; the loop
// takes the slow one again once the connection has closed (no count was left
// behind). The loop that holds the connection is the one whose sweeps see it.
func TestSweepTimerFollowsWhatTheLoopHolds1381(t *testing.T) {
	p := newStateProbe1381(t)
	r := start886(t, 2, 0, false)
	loops := r.e.loopSnapshot()
	addr := r.e.Addr().String()

	for _, l := range loops {
		waitState1381(t, p, "an idle loop on the slow timer", []*Loop{l}, func(slow, live bool) bool { return slow && !live })
	}
	c := dial1381(t, addr)
	waitState1381(t, p, "the loop that holds a connection back on the 25 ms timer", loops, func(slow, live bool) bool { return live && !slow })
	_ = c.Close()
	for _, l := range loops {
		waitState1381(t, p, "an idle loop on the slow timer again", []*Loop{l}, func(slow, live bool) bool { return slow && !live })
	}
}

// TestTransplantDrainTakesTheLoopOffTheLongWait1381: StartTransplant on an idle
// loop makes it leave the slow timer (a drain is owed a sweep), without an
// event of its own: the call signals the loop's eventfd.
func TestTransplantDrainTakesTheLoopOffTheLongWait1381(t *testing.T) {
	p := newStateProbe1381(t)
	r := start886(t, 2, 0, false)
	loops := r.e.loopSnapshot()
	for _, l := range loops {
		waitState1381(t, p, "an idle loop on the slow timer", []*Loop{l}, func(slow, live bool) bool { return slow && !live })
	}
	r.e.StartTransplant(stubTarget1381{})
	t.Cleanup(r.e.StopTransplant)
	for _, l := range loops {
		waitState1381(t, p, "a loop with a drain set, off the slow timer", []*Loop{l}, func(slow, _ bool) bool { return !slow })
	}
	r.e.StopTransplant()
	for _, l := range loops {
		waitState1381(t, p, "an idle loop on the slow timer after the drain stopped", []*Loop{l}, func(slow, live bool) bool { return slow && !live })
	}
}

type stubTarget1381 struct{}

func (stubTarget1381) AdoptConn(int, engine.Carryover) error { return nil }

// TestLoopWithoutAnEventfdNeverTakesTheLongWait1381: with the wake eventfd
// missing (its creation failed) nothing could end a long wait early, so the loop
// keeps today's waits and its 25 ms timer.
func TestLoopWithoutAnEventfdNeverTakesTheLongWait1381(t *testing.T) {
	old := eventfdCreate
	eventfdCreate = func(uint, int) (int, error) { return -1, unix.EMFILE }
	t.Cleanup(func() { eventfdCreate = old })

	p := newStateProbe1381(t)
	r := start886(t, 2, 0, false)
	loops := r.e.loopSnapshot()
	for _, l := range loops {
		if l.wakeFD.FD() >= 0 {
			t.Fatalf("PREMISE: loop %d has an eventfd", l.id)
		}
	}
	// Past 100 empty rounds a loop with an eventfd takes the slow timer (the test
	// above): wait for the same number of rounds, then watch a second of sweeps.
	for _, l := range loops {
		for dl := time.Now().Add(20 * time.Second); ; time.Sleep(2 * time.Millisecond) {
			if _, _, n := p.state(l); n > 120 { // a sweep every 25 ms, 100 empty rounds are well behind it
				break
			}
			if time.Now().After(dl) {
				t.Fatalf("loop %d: fewer than 120 sweeps in 20 s", l.id)
			}
		}
	}
	_, _, before := p.state(loops[0])
	time.Sleep(time.Second) // the window, not a synchronisation
	_, _, after := p.state(loops[0])
	for _, l := range loops {
		if slow, _, _ := p.state(l); slow {
			t.Errorf("loop %d has no eventfd and took the slow timer", l.id)
		}
	}
	t.Logf("celeris1381 NO-EVENTFD: %d sweeps in 1 s on loop 0 (the 25 ms timer gives 40, the slow one 10)", after-before)
	if after-before < 25 {
		t.Errorf("a loop without an eventfd made %d sweeps in 1 s, want at least 25", after-before)
	}
}

// TestSetSweepPeriodRecordsTheTimerOnlyOnceItIsArmed1381: a re-arm that fails is
// not recorded, so the loop tries again at its next iteration; a loop that holds
// a connection is never left on a 100 ms timer it believes is 25 ms. The syscall
// is the package's timerfdSettime seam, so no kernel failure is needed.
func TestSetSweepPeriodRecordsTheTimerOnlyOnceItIsArmed1381(t *testing.T) {
	old := timerfdSettime
	t.Cleanup(func() { timerfdSettime = old })
	var calls int
	var lastInterval unix.Timespec
	fail := true
	timerfdSettime = func(_ int, _ int, spec *unix.ItimerSpec, _ *unix.ItimerSpec) error {
		calls++
		lastInterval = spec.Interval
		if fail {
			return unix.EINVAL
		}
		return nil
	}
	l := &Loop{timerFD: 4}
	l.setSweepPeriod(true)
	if l.timerSlow || calls != 1 {
		t.Fatalf("slow re-arm that failed: timerSlow %v after %d calls, want false after 1 (not recorded)", l.timerSlow, calls)
	}
	fail = false
	l.setSweepPeriod(true)
	if !l.timerSlow || lastInterval.Nsec != 100*1e6 {
		t.Fatalf("slow re-arm: timerSlow %v, interval %v, want true and 100 ms", l.timerSlow, lastInterval)
	}
	fail = true
	l.setSweepPeriod(false)
	if !l.timerSlow {
		t.Fatal("a fast re-arm that failed was recorded: the loop would keep its 100 ms timer believing it is 25 ms")
	}
	fail = false
	l.setSweepPeriod(false)
	if l.timerSlow || lastInterval.Nsec != 25*1e6 {
		t.Fatalf("fast re-arm: timerSlow %v, interval %v, want false and 25 ms", l.timerSlow, lastInterval)
	}
}

// TestSetSweepPeriodLeavesATimerATestDisarmedAlone1381: a loop already on the slow
// timer whose timer a test has disarmed by hand (idleWaitOff) is not re-armed at
// 25 ms by its next top of iteration.
func TestSetSweepPeriodLeavesATimerATestDisarmedAlone1381(t *testing.T) {
	old := timerfdSettime
	t.Cleanup(func() { timerfdSettime = old })
	calls := 0
	timerfdSettime = func(int, int, *unix.ItimerSpec, *unix.ItimerSpec) error { calls++; return nil }
	l := &Loop{timerFD: 4, timerSlow: true}
	l.idleWaitOff.Store(true)
	l.setSweepPeriod(false)
	if calls != 0 || l.timerSlow {
		t.Fatalf("a hand-disarmed timer was re-armed (%d calls) or still recorded slow (%v)", calls, l.timerSlow)
	}
}
