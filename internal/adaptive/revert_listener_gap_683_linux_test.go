//go:build linux

package adaptive

import (
	"context"
	"errors"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/engine/epoll"
	"github.com/goceleris/celeris/internal/engine/iouring"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#683: a switch whose outgoing engine closes its listeners at the
// pause (resource.Config.DisableDeferAccept: nothing to linger for) must not
// leave the port without a listener while the incoming engine's loops are
// slow to listen. epoll's ResumeAccept returns before any loop has re-created
// its socket, so the old order (resume, then pause the outgoing engine) opened
// a listener-free window as long as the slowest first loop, and a dial inside
// it was refused. With the default configuration the outgoing engine lingers
// for ~1.5 s (celeris#662) and the window is covered; the measurement that
// says so is in the PR.

// lateEpoll683 is the positive control and the failing-first fixture: an epoll
// engine whose ResumeAccept returns at once and takes effect delay later, so
// every loop's re-listen is late by construction.
type lateEpoll683 struct {
	*epoll.Engine
	delay time.Duration
	calls atomic.Int32
	// calledAt is the time.Now().UnixNano() of the first ResumeAccept: the
	// moment the switch leaves the resume and enters awaitIncomingListener.
	calledAt atomic.Int64
}

func (l *lateEpoll683) ResumeAccept() error {
	l.calledAt.CompareAndSwap(0, time.Now().UnixNano())
	l.calls.Add(1)
	go func() {
		time.Sleep(l.delay)
		_ = l.Engine.ResumeAccept()
	}()
	return nil
}

// installLateEpoll683 swaps the epoll sub-engine for a late one in every slot
// the switch reads: the engine, the controller and, when epoll is active, the
// active pointer.
func installLateEpoll683(t *testing.T, e *Engine, delay time.Duration) *lateEpoll683 {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	ep, ok := e.primary.(*epoll.Engine)
	if !ok {
		t.Fatalf("the primary is %T, want *epoll.Engine", e.primary)
	}
	l := &lateEpoll683{Engine: ep, delay: delay}
	e.switchMu.Lock()
	e.slotsMu.Lock()
	e.primary = l
	e.slotsMu.Unlock()
	e.ctrl.primary = l
	e.switchMu.Unlock()
	if e.ActiveEngine().Type() == engine.Epoll {
		var eng engine.Engine = l
		e.active.Store(&eng)
	}
	return l
}

// dialer683 dials 127.0.0.1:port at ~5k/s across four goroutines and records
// every refused dial with its start time.
type dialer683 struct {
	t0      time.Time
	mu      sync.Mutex
	started []time.Duration // the start of every dial, from t0
	refused []time.Duration
	other   int
	dials   atomic.Int64
	stop    chan struct{}
	wg      sync.WaitGroup
}

func startDialer683(addr string) *dialer683 {
	d := &dialer683{t0: time.Now(), stop: make(chan struct{})}
	for range 4 {
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			for tick := time.Now(); ; tick = tick.Add(800 * time.Microsecond) {
				select {
				case <-d.stop:
					return
				default:
				}
				if w := time.Until(tick); w > 0 {
					time.Sleep(w)
				}
				at := time.Since(d.t0)
				d.mu.Lock()
				d.started = append(d.started, at)
				d.mu.Unlock()
				c, err := net.DialTimeout("tcp", addr, time.Second)
				d.dials.Add(1)
				if err != nil {
					d.mu.Lock()
					if errors.Is(err, syscall.ECONNREFUSED) {
						d.refused = append(d.refused, at)
					} else {
						d.other++
					}
					d.mu.Unlock()
					continue
				}
				_ = c.Close()
			}
		}()
	}
	return d
}

// holdListenWaitBound683 lengthens the bound of the switch's wait for the
// incoming engine's listener for the rest of the test. The promotion test
// asserts that the wait SAW the late listener, not how long a loaded host took
// to produce it: on a loaded container two measured promotions at --cpus 4
// (338 ms on main, 411 ms on an intermediate tree) spent more than the 250 ms
// bound in the wait, with no dial refused (celeris#683). Why is NOT measured: the wait lists again before
// it can return a timeout, so a descheduled waiter explains the overshoot past
// 250 ms but not why io_uring had no new listener at the last list; cpu.stat
// was not captured for those two runs. (An epoll setup wait that had nothing to
// find overshot to 411 ms in the same way at --cpus 2, so the overshoot itself
// is a scheduling tail.) The bound is chosen below the 1 s ring
// wait that a worker with no listener sits in (Worker.baseTimeout): a resume
// that is lost and found only at that timeout still ends the wait at the bound
// and fails the test, while the observed tail is absorbed. A listener that
// never comes also fails. The trade-off: a late listener that arrives between
// 250 ms and the new bound now passes, and that is the range in which
// production pauses the outgoing engine first; the product bound is not what
// this test measures. Taken after the setup: the setup's own revert can wait
// for a listener that cannot come (see the promotion test) and must not take
// the longer bound.
//
// It writes the package-level listenWaitBound without synchronisation. That is
// safe only because no other switch is in flight: the setup's switches are
// synchronous and s0Bind froze the controller's (FreezeSwitching), and no test
// in this package runs in parallel. Do not use it from a parallel test. The
// engine is stopped before t.Cleanup restores the value.
func holdListenWaitBound683(t *testing.T, bound time.Duration) {
	t.Helper()
	old := listenWaitBound
	listenWaitBound = bound
	t.Cleanup(func() { listenWaitBound = old })
}

// windowDials683 returns how many dials started, and how many were refused,
// in the switch window: from 20 ms before the switch to 400 ms after it. The
// dial floor (awaitDials683) can be met by dials after the switch, so a run's
// log carries this count: a starved run that probed the switch with a handful
// of dials shows it.
func windowDials683(d *dialer683, swStart, swEnd time.Duration) (started, refused int) {
	lo, hi := swStart-20*time.Millisecond, swEnd+400*time.Millisecond
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, at := range d.started {
		if at >= lo && at <= hi {
			started++
		}
	}
	for _, at := range d.refused {
		if at >= lo && at <= hi {
			refused++
		}
	}
	return started, refused
}

// listenWaitHold683 is the promotion test's bound for the measured switch's
// wait: below the 1 s ring wait of an io_uring worker with no listener, above
// the 411 ms tail seen under load.
const listenWaitHold683 = 800 * time.Millisecond

// minDials683 is the least number of dials a run must have made for a clean
// result to mean something.
const minDials683 = 500

// awaitDials683 gives a dialer that a loaded host has starved the time to reach
// n dials, up to limit. The dialer paces itself at ~5k/s, so on an idle host the
// run's fixed windows hold thousands of dials and this returns at once; on a
// 2-CPU host shared with busy loops the same windows held as few as 122, which
// failed the premise without any refused dial (celeris#683). The trade-off: the
// gap a regression would open is the switch itself (about 20 ms with the
// injected delay), and a dial made after the windows only shows that the dialer
// is alive, so in exactly the starved runs fewer probes land inside the switch.
// The floor keeps a starved dialer from failing the premise; it does not make a
// starved run as sensitive as an idle one.
func awaitDials683(d *dialer683, n int64, limit time.Duration) {
	for end := time.Now().Add(limit); d.dials.Load() < n && time.Now().Before(end); {
		time.Sleep(5 * time.Millisecond)
	}
}

func (d *dialer683) finish() (refused []time.Duration, other int) {
	close(d.stop)
	d.wg.Wait()
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.refused, d.other
}

func TestRevertKeepsAListenerUpWhenTheIncomingEngineListensLate683(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	e, addr, stop := s0Bind(t, resource.Config{Addr: "127.0.0.1:0", Protocol: engine.HTTP1, DisableDeferAccept: true,
		Resources: resource.Resources{Workers: 4}}, respHandler{})
	defer stop()
	port := e.Addr().(*net.TCPAddr).Port

	forceSwitchTo(t, e, engine.IOUring)
	// DisableDeferAccept: epoll's pause closed its listeners at once, and its
	// idle loops park. That is the state a revert finds, and the one in which
	// the issue's refusals were measured.
	time.Sleep(1500 * time.Millisecond)
	onlyIOUring, err := listenInodes(port)
	if err != nil {
		skipOrFailUpswitch662(t, "cannot list listening sockets here: %v", err)
	}
	late := installLateEpoll683(t, e, 20*time.Millisecond)
	// No longer bound here. The revert's wait did not run into the 250 ms
	// bound in 1998 runs under load with the default bound (celeris#683), epoll's
	// re-listen was seen in 56 ms at most, and the default bound is below the
	// 500 ms wait of an epoll loop with no listener (epoll/loop.go: timeoutMs =
	// 500), so a wakeup that is lost until that timeout still fails the test.
	// The switch wall reached 262 ms in those runs; that is the wall, which also
	// covers the outgoing engine's pause, not the wait. The promotion test
	// holds a longer bound; see there.
	setupTimeouts := e.listenWaitTimeouts() // the setup's switch is not the measurement; see the promotion twin

	d := startDialer683(addr)
	time.Sleep(200 * time.Millisecond)
	swStart := time.Since(d.t0)
	t0 := time.Now()
	forceSwitchTo(t, e, engine.Epoll)
	swWall := time.Since(t0)
	swEnd := time.Since(d.t0)
	time.Sleep(400 * time.Millisecond)
	awaitDials683(d, minDials683, 10*time.Second)
	refused, other := d.finish()

	both, _ := listenInodes(port)
	dialsIn, inWindow := windowDials683(d, swStart, swEnd)
	timeouts := e.listenWaitTimeouts() - setupTimeouts
	t.Logf("celeris683 LATE-RESUME revert: switch wall %v, %d dials (%d in the switch window), refused %d (%d in the window), other errors %d, "+
		"listeners before %d after %d (%d new), wait timeouts %d (setup %d), resume calls %d",
		swWall.Round(time.Microsecond), d.dials.Load(), dialsIn, len(refused), inWindow, other, len(onlyIOUring), len(both), freshCount683(both, onlyIOUring),
		timeouts, setupTimeouts, late.calls.Load())

	// Premises: the resume was late and was the switch's, epoll had no
	// listener before it and has some after, and the dialer ran.
	if late.calls.Load() != 1 {
		t.Fatalf("celeris683 PREMISE: the late ResumeAccept ran %d times, want 1", late.calls.Load())
	}
	if fresh := freshCount683(both, onlyIOUring); fresh == 0 {
		t.Fatalf("celeris683 PREMISE: no listener on the port after the revert is new, so epoll never listened again "+
			"(before %d, after %d; the outgoing io_uring listeners close at the pause, so the count alone says nothing)",
			len(onlyIOUring), len(both))
	}
	if d.dials.Load() < minDials683 {
		t.Fatalf("celeris683 PREMISE: only %d dials in the run (want %d)", d.dials.Load(), minDials683)
	}
	if len(refused) != 0 {
		t.Errorf("celeris683 GAP: %d dials were refused (%d inside the switch window): the outgoing io_uring listeners "+
			"closed before epoll had one", len(refused), inWindow)
	}
	if timeouts != 0 {
		t.Errorf("celeris683: the wait for the incoming listener gave up %d time(s) in the measured revert", timeouts)
	}
}

// lateIOUring683 is lateEpoll683 for the other direction: io_uring's resume is
// just as asynchronous (its workers re-create their listeners on their own
// threads), so the same gap can open on a promotion.
type lateIOUring683 struct {
	*iouring.Engine
	delay time.Duration
	calls atomic.Int32
}

func (l *lateIOUring683) ResumeAccept() error {
	l.calls.Add(1)
	go func() {
		time.Sleep(l.delay)
		_ = l.Engine.ResumeAccept()
	}()
	return nil
}

// TestPromotionKeepsAListenerUpWhenTheIncomingEngineListensLate683: the
// promotion twin of the revert test above (RULE 61). The standby is built by
// a first promotion and reverted away from; the second promotion resumes it
// 20 ms late, and epoll, which closes at once under DisableDeferAccept, must
// not close before io_uring listens.
func TestPromotionKeepsAListenerUpWhenTheIncomingEngineListensLate683(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	e, addr, stop := s0Bind(t, resource.Config{Addr: "127.0.0.1:0", Protocol: engine.HTTP1, DisableDeferAccept: true,
		Resources: resource.Resources{Workers: 4}}, respHandler{})
	defer stop()
	port := e.Addr().(*net.TCPAddr).Port

	forceSwitchTo(t, e, engine.IOUring) // builds the standby
	forceSwitchTo(t, e, engine.Epoll)
	// io_uring's pause closes its listeners at once. Before celeris#886 a
	// worker that had not yet parked saw a resume only after its current ring
	// wait (up to a second), and this sleep let them park so that the next
	// promotion was a plain late resume. ResumeAccept now signals the wake
	// eventfd, so a worker that has not parked no longer lags; the sleep stays
	// so the test starts from the state the revert test starts from.
	time.Sleep(2500 * time.Millisecond)
	onlyEpoll, err := listenInodes(port)
	if err != nil {
		skipOrFailUpswitch662(t, "cannot list listening sockets here: %v", err)
	}
	// The setup's own switches are not the measurement. The revert above waits
	// for epoll's listener, and under load epoll can fail to produce a NEW one
	// in the bound: a loop that had not yet seen the pause when the resume
	// withdrew it never closed its listener, so there is nothing new to see
	// (celeris#683). That wait times out and counts, while no dial is refused.
	// What the test asserts is the measured promotion's wait, so the count is
	// taken here and compared as a delta.
	setupTimeouts := e.listenWaitTimeouts()
	holdListenWaitBound683(t, listenWaitHold683)
	e.mu.Lock()
	io, ok := e.secondary.(*iouring.Engine)
	if !ok {
		e.mu.Unlock()
		t.Fatalf("the secondary is %T, want *iouring.Engine", e.secondary)
	}
	late := &lateIOUring683{Engine: io, delay: 20 * time.Millisecond}
	e.switchMu.Lock()
	e.slotsMu.Lock()
	e.secondary = late
	e.slotsMu.Unlock()
	e.ctrl.secondary = late
	e.switchMu.Unlock()
	e.mu.Unlock()

	d := startDialer683(addr)
	time.Sleep(200 * time.Millisecond)
	swStart := time.Since(d.t0)
	t0 := time.Now()
	forceSwitchTo(t, e, engine.IOUring)
	swWall := time.Since(t0)
	swEnd := time.Since(d.t0)
	time.Sleep(400 * time.Millisecond)
	awaitDials683(d, minDials683, 10*time.Second)
	refused, other := d.finish()

	both, _ := listenInodes(port)
	dialsIn, inWindow := windowDials683(d, swStart, swEnd)
	timeouts := e.listenWaitTimeouts() - setupTimeouts
	t.Logf("celeris683 LATE-RESUME promote: switch wall %v, %d dials (%d in the switch window), refused %d (%d in the window), other errors %d, "+
		"listeners before %d after %d (%d new), wait timeouts %d (setup %d), resume calls %d",
		swWall.Round(time.Microsecond), d.dials.Load(), dialsIn, len(refused), inWindow, other, len(onlyEpoll), len(both),
		freshCount683(both, onlyEpoll), timeouts, setupTimeouts, late.calls.Load())
	if late.calls.Load() != 1 {
		t.Fatalf("celeris683 PREMISE: the late ResumeAccept ran %d times, want 1", late.calls.Load())
	}
	if freshCount683(both, onlyEpoll) == 0 {
		t.Fatalf("celeris683 PREMISE: io_uring never listened again (before %d, after %d)", len(onlyEpoll), len(both))
	}
	if d.dials.Load() < minDials683 {
		t.Fatalf("celeris683 PREMISE: only %d dials in the run (want %d)", d.dials.Load(), minDials683)
	}
	if len(refused) != 0 {
		t.Errorf("celeris683 GAP: %d dials were refused (%d inside the switch window): epoll's listeners closed before "+
			"io_uring had one", len(refused), inWindow)
	}
	if timeouts != 0 {
		t.Errorf("celeris683: the wait for the incoming listener gave up %d time(s) in the measured promotion", timeouts)
	}
}

// The wait is not held under freezeState: a driver registering while the
// switch waits for a late incoming engine proceeds at once (RULE 10). With the
// wait inside the lock the registration would sit out the whole delay.
func TestSwitchWaitForTheIncomingListenerDoesNotBlockDriverRegistration683(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	e, _, stop := s0Bind(t, resource.Config{Addr: "127.0.0.1:0", Protocol: engine.HTTP1, DisableDeferAccept: true,
		Resources: resource.Resources{Workers: 2}}, respHandler{})
	defer stop()
	port := e.Addr().(*net.TCPAddr).Port
	forceSwitchTo(t, e, engine.IOUring)
	time.Sleep(1500 * time.Millisecond)
	if _, err := listenInodes(port); err != nil {
		skipOrFailUpswitch662(t, "cannot list listening sockets here: %v", err)
	}
	const delay = 150 * time.Millisecond
	installLateEpoll683(t, e, delay)

	done := make(chan time.Duration, 1)
	go func() {
		t0 := time.Now()
		e.ForceSwitch()
		done <- time.Since(t0)
	}()
	time.Sleep(40 * time.Millisecond) // inside the wait: the engine resumed, the delay not yet over
	t0 := time.Now()
	e.acquireDriverFD()
	e.releaseDriverFD()
	reg := time.Since(t0)
	wall := <-done
	t.Logf("celeris683 LOCK: driver register+unregister took %v while the switch waited; the switch took %v", reg, wall)
	if wall < delay/2 {
		t.Fatalf("celeris683 PREMISE: the switch took %v, it did not wait for the late engine (delay %v)", wall, delay)
	}
	if reg > delay/4 {
		t.Errorf("celeris683 LOCK: a driver registration took %v during the switch's wait (delay %v): "+
			"the wait holds freezeState", reg, delay)
	}
	if got := e.ActiveEngine().Type(); got != engine.Epoll {
		t.Errorf("the switch left %v active, want epoll", got)
	}
}

// Nor does Engine.Metrics() (hence Server.EngineInfo and the observe collector)
// sit behind the wait: it takes only slotsMu for the two sub-engine slots, and
// nothing holds slotsMu across the wait. Read under e.mu, it sat out the rest
// of the late engine's delay (celeris#886 item 12).
func TestSwitchWaitForTheIncomingListenerDoesNotBlockMetrics683(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	e, _, stop := s0Bind(t, resource.Config{Addr: "127.0.0.1:0", Protocol: engine.HTTP1, DisableDeferAccept: true,
		Resources: resource.Resources{Workers: 2}}, respHandler{})
	defer stop()
	port := e.Addr().(*net.TCPAddr).Port
	forceSwitchTo(t, e, engine.IOUring)
	time.Sleep(1500 * time.Millisecond)
	if _, err := listenInodes(port); err != nil {
		skipOrFailUpswitch662(t, "cannot list listening sockets here: %v", err)
	}
	const delay = 150 * time.Millisecond
	late := installLateEpoll683(t, e, delay)

	done := make(chan time.Duration, 1)
	go func() {
		t0 := time.Now()
		e.ForceSwitch()
		done <- time.Since(t0)
	}()
	// Enter the wait before reading: the switch has resumed the incoming
	// engine (its ResumeAccept ran) and the late listener is not up yet. A
	// fixed sleep would let a descheduled switch goroutine run Metrics()
	// before performSwitch takes e.mu, and the unfixed code would pass.
	for late.calls.Load() == 0 {
		select {
		case w := <-done:
			if late.calls.Load() == 0 {
				t.Fatalf("celeris683 PREMISE: the switch ended after %v without resuming the late engine", w)
			}
			done <- w // resumed and already over: the checks below say what that means
		case <-time.After(time.Millisecond):
		}
	}
	time.Sleep(10 * time.Millisecond) // past the resume, into awaitIncomingListener
	t0 := time.Now()
	inWait := t0.UnixNano() - late.calledAt.Load()
	m := e.Metrics()
	took := time.Since(t0)
	wall := <-done
	t.Logf("celeris683 METRICS: Metrics() started %v after the resume and took %v (Workers %d); the switch took %v",
		time.Duration(inWait), took, m.Workers, wall)
	if inWait < 0 || time.Duration(inWait) >= delay {
		t.Fatalf("celeris683 PREMISE: Metrics() started %v after the resume, outside the late engine's %v delay", time.Duration(inWait), delay)
	}
	if wall < delay/2 {
		t.Fatalf("celeris683 PREMISE: the switch took %v, it did not wait for the late engine (delay %v)", wall, delay)
	}
	if m.Workers == 0 {
		t.Errorf("celeris683 METRICS: Metrics() read no sub-engine (Workers 0)")
	}
	if took > delay/4 {
		t.Errorf("celeris683 METRICS: Metrics() took %v during the switch's wait (delay %v): "+
			"the wait holds the lock Metrics() takes", took, delay)
	}
	if got := e.ActiveEngine().Type(); got != engine.Epoll {
		t.Errorf("the switch left %v active, want epoll", got)
	}
}

// The kernel list the wait reads names a socket exactly when the process has
// it open.
func TestListenInodesSeesASocketOnlyWhileItListens683(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("net.Listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	f, err := ln.(*net.TCPListener).File()
	if err != nil {
		t.Fatal(err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	set, err := listenInodes(port)
	if err != nil {
		skipOrFailUpswitch662(t, "cannot list listening sockets here: %v", err)
	}
	if _, ok := set[uint32(st.Ino)]; !ok || len(set) != 1 {
		t.Fatalf("listenInodes(%d) = %v, want exactly the listener's inode %d", port, set, st.Ino)
	}
	other, err := listenInodes(port%60000 + 1) // a port nothing here listens on
	if err != nil || len(other) != 0 {
		t.Fatalf("listenInodes on a port with no listener = %v, %v", other, err)
	}
	_ = ln.Close()
	set, err = listenInodes(port)
	if err != nil || len(set) != 0 {
		t.Fatalf("listenInodes after Close = %v, %v, want none", set, err)
	}
}

// wait returns when a socket that was not in the watch appears, after about
// as long as it took to appear, and gives up at the bound when none does.
func TestListenWatchWaitsForANewListenerAndTimesOut683(t *testing.T) {
	first, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("net.Listen: %v", err)
	}
	defer func() { _ = first.Close() }()
	port := first.Addr().(*net.TCPAddr).Port
	w, err := newListenWatch(port)
	if err != nil {
		skipOrFailUpswitch662(t, "cannot list listening sockets here: %v", err)
	}

	waited, ok, err := w.wait(60 * time.Millisecond)
	if err != nil || ok || waited < 60*time.Millisecond || waited > 500*time.Millisecond {
		t.Fatalf("no new listener: wait = %v, %v, %v; want a timeout at ~60ms", waited, ok, err)
	}

	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
		}); err != nil {
			return err
		}
		return serr
	}}
	// The first listener has no SO_REUSEPORT, so a second one cannot join it:
	// watch a fresh reuseport group instead.
	_ = first.Close()
	a, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("reuseport listen: %v", err)
	}
	defer func() { _ = a.Close() }()
	port = a.Addr().(*net.TCPAddr).Port
	w, err = newListenWatch(port)
	if err != nil {
		t.Fatal(err)
	}
	bc := make(chan net.Listener, 1)
	go func() {
		time.Sleep(30 * time.Millisecond)
		b, _ := lc.Listen(context.Background(), "tcp", "127.0.0.1:"+strconv.Itoa(port))
		bc <- b
	}()
	waited, ok, err = w.wait(2 * time.Second)
	if err != nil || !ok || waited < 25*time.Millisecond || waited > time.Second {
		t.Fatalf("a listener joined after 30ms: wait = %v, %v, %v", waited, ok, err)
	}
	if b := <-bc; b != nil {
		_ = b.Close()
	}
}

// A listener that appears within microseconds of the resume is seen without a
// sleep: the wait lists again at once a few times before its first sleep,
// which on this runtime lasts about a millisecond or more whatever the request
// (listenWaitPoll; about 1.4 ms in the arm64 container, celeris#886). The
// kernel list is a fake here, so the counts are exact. At least 3 and at most 8
// immediate re-lists: a listener that shows on the 4th list is found with no
// sleep, and one that shows on the 50th has cost no more than 8 lists without
// one, so the wait is not a busy poll.
func TestListenWatchListsAgainAtOnceBeforeItsFirstSleep683(t *testing.T) {
	const minSpins, maxSpins = 3, 8
	for _, tc := range []struct {
		name      string
		appearsAt int // the 1-based list that first shows the new inode
		ok        func(lists, sleeps int) bool
		want      string
	}{
		{"found on the last required immediate list", 1 + minSpins,
			func(_, sleeps int) bool { return sleeps == 0 }, "no sleep"},
		{"found late, still a sleeping poll", 50,
			func(lists, sleeps int) bool { return sleeps >= lists-1-maxSpins && sleeps <= lists-1 }, "at most 8 lists without a sleep"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lists, sleeps := 0, 0
			w := &listenWatch{port: 1, before: map[uint32]struct{}{7: {}},
				list: func(_, _ int) (map[uint32]struct{}, error) {
					lists++
					if lists >= tc.appearsAt {
						return map[uint32]struct{}{7: {}, 9: {}}, nil
					}
					return map[uint32]struct{}{7: {}}, nil
				},
				sleep: func(time.Duration) { sleeps++ },
			}
			_, ok, err := w.wait(time.Hour)
			if err != nil {
				skipOrFailUpswitch662(t, "no sock_diag socket here: %v", err)
			}
			if !ok {
				t.Fatalf("celeris683 SPIN: the wait gave up (%d lists, %d sleeps) with the new inode on list %d", lists, sleeps, tc.appearsAt)
			}
			if lists != tc.appearsAt || !tc.ok(lists, sleeps) {
				t.Errorf("celeris683 SPIN: %d lists and %d sleeps; want the new inode seen on list %d with %s",
					lists, sleeps, tc.appearsAt, tc.want)
			}
		})
	}
}

// freshCount683 is the number of inodes in after that are not in before.
func freshCount683(after, before map[uint32]struct{}) int {
	n := 0
	for ino := range after {
		if _, was := before[ino]; !was {
			n++
		}
	}
	return n
}
