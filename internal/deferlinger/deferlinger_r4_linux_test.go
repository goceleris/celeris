//go:build linux

package deferlinger

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestEnterDeadlineIsOnTheMonotonicClock pins the clock a linger deadline is
// on (celeris#662 review). The deadline used to be a Unix-nanosecond reading
// of the wall clock, while PauseAccept's own bound is monotonic: a backward
// step of the wall clock during a linger kept a paused listener open and
// accepting after PauseAccept had returned, and a forward step ended the
// linger early and brought the resets back. On the monotonic Now clock a
// deadline counts from the package's initialisation, so it lies far below
// any Unix-nanosecond reading; a deadline in the wall clock's domain does
// not. The wall clock cannot be stepped from a test without touching the
// host, so the test pins the domain, and the Deadline type pins every
// comparison against it (a wall-clock reading does not compile against it).
func TestEnterDeadlineIsOnTheMonotonicClock(t *testing.T) {
	withSynackFile(t, []byte("5\n"))
	fd := listener(t, true)
	var p PauseState
	p.Begin(nil, "test")
	until := Enter(fd, true, &p, nil, "loop", 0)
	if until == 0 {
		t.Fatal("premise: Enter closed at once on a listener with the option")
	}
	wall := time.Now().UnixNano()
	if int64(until) > wall/2 {
		t.Errorf("the linger deadline is %d, in the Unix-nanosecond domain of the wall clock "+
			"(now %d): a step of the wall clock would move the linger's end", int64(until), wall)
	}
}

// TestClockIsMonotonic pins the two things the Now clock rests on: its epoch
// carries a monotonic reading (time.Time prints it as "m=..."), without which
// time.Since would fall back to the wall clock, and it advances with real
// time.
func TestClockIsMonotonic(t *testing.T) {
	if !strings.Contains(epoch.String(), " m=") {
		t.Errorf("the clock's epoch %q carries no monotonic reading, so time.Since(epoch) "+
			"reads the wall clock", epoch.String())
	}
	a := Now()
	time.Sleep(20 * time.Millisecond)
	b := Now()
	if d := time.Duration(b - a); d < 20*time.Millisecond || d > 5*time.Second {
		t.Errorf("Now advanced %v across a 20ms sleep", d)
	}
	d := After(time.Second)
	if d.Passed() {
		t.Error("a deadline one second ahead has passed")
	}
	if left := d.Left(); left <= 0 || left > time.Second {
		t.Errorf("a deadline one second ahead has %v left", left)
	}
	if !After(-time.Millisecond).Passed() {
		t.Error("a deadline one millisecond behind has not passed")
	}
}

// captureWarns is a slog.Handler that counts WARN records.
type captureWarns struct {
	mu   sync.Mutex
	msgs []string
}

func (c *captureWarns) Enabled(context.Context, slog.Level) bool { return true }
func (c *captureWarns) Handle(_ context.Context, r slog.Record) error {
	if r.Level == slog.LevelWarn {
		c.mu.Lock()
		c.msgs = append(c.msgs, r.Message)
		c.mu.Unlock()
	}
	return nil
}
func (c *captureWarns) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *captureWarns) WithGroup(string) slog.Handler      { return c }
func (c *captureWarns) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.msgs)
}

// TestPauseWarningsAreLoggedOncePerEngine: a pause whose sysctl read fails,
// and a listener whose clear fails, are counted every time and logged once
// per engine (celeris#662 review: Begin warned on every pause, under the
// adaptive engine's lock on every switch, and Enter on every listener of
// every pause). A second engine logs its own first failure.
func TestPauseWarningsAreLoggedOncePerEngine(t *testing.T) {
	withSynackFile(t, nil) // unreadable: every Begin fails to read it
	h := &captureWarns{}
	logger := slog.New(h)
	var p PauseState
	s0 := Snapshot()
	p.Begin(logger, "test")
	p.Begin(logger, "test")
	if got := h.count(); got != 1 {
		t.Errorf("two pauses with tcp_synack_retries unreadable logged %d warnings, want 1", got)
	}
	// A clear on a descriptor that is not a socket fails.
	_ = Enter(-1, true, &p, logger, "loop", 0)
	_ = Enter(-1, true, &p, logger, "loop", 1)
	if got := h.count(); got != 2 {
		t.Errorf("two failed clears on one engine logged %d warnings in all, want 2 (one per kind)", got)
	}
	s1 := Snapshot()
	if s1.SynackUnread-s0.SynackUnread != 2 || s1.SetFailures-s0.SetFailures != 2 {
		t.Errorf("counters moved synackUnread +%d setFailures +%d, want +2 +2: a warning logged "+
			"once must still be counted every time", s1.SynackUnread-s0.SynackUnread,
			s1.SetFailures-s0.SetFailures)
	}
	var q PauseState
	q.Begin(logger, "other")
	if got := h.count(); got != 3 {
		t.Errorf("after a second engine's first unreadable pause there are %d warnings, want 3: "+
			"one more, for that engine's first", got)
	}
}

// TestRestoreWarningLoggedOncePerEngine: the same for a failed restore at a
// resume, and a nil PauseState (a bare test loop) still logs.
func TestRestoreWarningLoggedOncePerEngine(t *testing.T) {
	h := &captureWarns{}
	logger := slog.New(h)
	var p PauseState
	Leave(-1, true, &p, logger, "loop", 0)
	Leave(-1, true, &p, logger, "loop", 1)
	if got := h.count(); got != 1 {
		t.Errorf("two failed restores on one engine logged %d warnings, want 1", got)
	}
	Leave(-1, true, nil, logger, "loop", 2)
	if got := h.count(); got != 2 {
		t.Errorf("a failed restore with no PauseState logged nothing (warnings %d, want 2)", got)
	}
}

// TestNotifyWakesEveryWaiter: Notify closes the channel every waiter took,
// so two PauseAccept calls waiting on one engine both wake; a channel taken
// after the Notify is a fresh one; a wait on a closed channel returns at once
// and counts as woken; a nil PauseState's Notify does nothing.
func TestNotifyWakesEveryWaiter(t *testing.T) {
	var p PauseState
	a, b := p.Changed(), p.Changed()
	if a != b {
		t.Fatal("two waiters before a Notify got different channels")
	}
	p.Notify()
	for i, c := range []<-chan struct{}{a, b} {
		select {
		case <-c:
		default:
			t.Errorf("waiter %d's channel is still open after Notify", i)
		}
	}
	s0 := Snapshot()
	t0 := time.Now()
	if !WaitChanged(a, time.Now().Add(5*time.Second)) {
		t.Error("a wait on a notified channel returned false")
	}
	if d := time.Since(t0); d > WaitRecheck/2 {
		t.Errorf("a wait on a notified channel took %v: it did not see the notification", d)
	}
	if s1 := Snapshot(); s1.WaitWoken-s0.WaitWoken != 1 || s1.WaitRechecks != s0.WaitRechecks {
		t.Errorf("a notified wait moved WaitWoken by %d and WaitRechecks by %d, want 1 and 0",
			s1.WaitWoken-s0.WaitWoken, s1.WaitRechecks-s0.WaitRechecks)
	}
	c := p.Changed()
	select {
	case <-c:
		t.Error("a channel taken after the Notify is already closed")
	default:
	}
	var nilState *PauseState
	nilState.Notify()
}

// TestChangedBeforeTheReadMissesNothing is the no-lost-wakeup order: a
// waiter takes the channel, then reads the state; a change published and
// notified after the read closes the channel it holds.
func TestChangedBeforeTheReadMissesNothing(t *testing.T) {
	var p PauseState
	var flag atomic.Bool
	c := p.Changed()
	if flag.Load() {
		t.Fatal("premise")
	}
	flag.Store(true) // the loop publishes...
	p.Notify()       // ...then notifies
	select {
	case <-c:
	case <-time.After(time.Second):
		t.Fatal("a change notified after the read did not close the channel taken before it")
	}
}

// TestWaitChangedRechecksAndStopsAtTheDeadline: with no Notify the wait
// wakes on its re-check timer (true while the deadline is ahead) and returns
// false at once when the deadline has passed.
func TestWaitChangedRechecksAndStopsAtTheDeadline(t *testing.T) {
	var p PauseState
	s0 := Snapshot()
	t0 := time.Now()
	if !WaitChanged(p.Changed(), time.Now().Add(10*time.Second)) {
		t.Error("the re-check returned false with the deadline ten seconds ahead")
	}
	if d := time.Since(t0); d < WaitRecheck-5*time.Millisecond || d > 5*time.Second {
		t.Errorf("the re-check woke after %v, want about %v", d, WaitRecheck)
	}
	if WaitChanged(p.Changed(), time.Now().Add(-time.Millisecond)) {
		t.Error("a wait whose deadline has passed returned true")
	}
	t1 := time.Now()
	if WaitChanged(p.Changed(), time.Now().Add(20*time.Millisecond)) {
		t.Error("a wait that reached its deadline returned true")
	}
	if d := time.Since(t1); d > 2*time.Second {
		t.Errorf("a 20ms deadline took %v", d)
	}
	if got := Snapshot().WaitRechecks - s0.WaitRechecks; got != 2 {
		t.Errorf("WaitRechecks moved by %d, want 2", got)
	}
}
