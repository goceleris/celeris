//go:build linux

package adaptive

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
)

// celeris#656, the adaptive half. An io_uring worker whose ring setup fails
// makes the io_uring engine's Listen return an error without ever publishing
// an address. When that engine is the lazy standby, buildAndStartStandby used
// to log the error from its Listen goroutine and keep polling Addr() for the
// full 5 s anyway, holding e.mu; performSwitch then aborted without recording
// anything, and the controller, whose load had not changed, asked for the same
// build on the next tick. Every attempt put the io_uring engine's sockets on
// the port being served again, and a standby that bound only after the 5 s
// deadline was left running: in the SO_REUSEPORT group, accepting, and in no
// slot that a later switch or Shutdown would reach.

// failingListenStandby is a lazy standby whose Listen fails at once and which
// never publishes an address: the shape of an io_uring engine whose worker
// init failed.
type failingListenStandby struct {
	*mockEngine
	listenCalls atomic.Int32
}

func (f *failingListenStandby) Listen(context.Context) error {
	f.listenCalls.Add(1)
	return errors.New("injected standby Listen failure (celeris#656)")
}

func (f *failingListenStandby) Addr() net.Addr { return nil }

// lateBindingStandby is a lazy standby that starts and runs, like a real
// engine, but has not published an address by the bind deadline. listenEnded
// is closed when its Listen returns.
type lateBindingStandby struct {
	*mockEngine
	listenEnded chan struct{}
}

func (l *lateBindingStandby) Listen(ctx context.Context) error {
	defer close(l.listenEnded)
	close(l.listenStarted)
	<-ctx.Done()
	return nil
}

func (l *lateBindingStandby) Addr() net.Addr { return nil }

// heavyLoad656 is a load the controller turns into an epoll -> io_uring
// switch on a single tick (the heavy-load fast path).
var heavyLoad656 = TelemetrySnapshot{ConnsPerWorker: 64, ActiveConnections: 256}

// TestLazyStandbyListenFailureAbortsPromptlyAndBacksOff: a standby whose
// Listen fails must abort the switch as soon as Listen says so, and the
// controller must not ask for the same build again on the next ticks.
func TestLazyStandbyListenFailureAbortsPromptlyAndBacksOff(t *testing.T) {
	e, sampler, _, builtCount := newLazyAdaptive(t)
	var standbys []*failingListenStandby
	e.buildStandby = func() (engine.Engine, error) {
		*builtCount++
		s := &failingListenStandby{mockEngine: newMockEngine(engine.IOUring)}
		standbys = append(standbys, s)
		return s, nil
	}
	sampler.Set(engine.Epoll, heavyLoad656)

	if !e.ctrl.evaluate(time.Now(), false) {
		t.Fatal("precondition: the controller does not recommend the up-switch for this load")
	}

	before := time.Now()
	e.performSwitch()
	after := time.Now()
	t.Logf("first failed build: performSwitch took %v", after.Sub(before))

	if *builtCount != 1 {
		t.Fatalf("buildStandby ran %d times, want 1", *builtCount)
	}
	// wg.Go may not have scheduled the Listen goroutine on the unfixed path,
	// which returns only after the deadline; it has certainly run by then.
	if got := standbys[0].listenCalls.Load(); got != 1 {
		t.Fatalf("standby Listen ran %d times, want 1", got)
	}
	if e.ActiveEngine().Type() != engine.Epoll || e.secondary != nil || !e.ctrl.state.activeIsPrimary {
		t.Fatalf("a failed build changed the engine: active=%v secondary=%v activeIsPrimary=%v",
			e.ActiveEngine().Type(), e.secondary, e.ctrl.state.activeIsPrimary)
	}
	if d := after.Sub(before); d > 2*time.Second {
		t.Errorf("performSwitch held e.mu for %v after the standby's Listen had already failed; "+
			"it must abort when Listen returns, not wait out the bind deadline", d)
	}

	// The eval loop's next tick, and one well inside the first backoff.
	for _, at := range []time.Time{after.Add(e.ctrl.evalInterval), before.Add(29 * time.Second)} {
		if e.ctrl.evaluate(at, false) {
			t.Errorf("the controller recommends the same build again %v after it failed: no backoff",
				at.Sub(before).Round(time.Second))
		}
	}
	if !e.ctrl.evaluate(after.Add(31*time.Second), false) {
		t.Error("the controller never recommends the switch again 31s after one failed build: the backoff does not expire")
	}

	// A second consecutive failure backs off longer.
	before = time.Now()
	e.performSwitch()
	after = time.Now()
	if *builtCount != 2 {
		t.Fatalf("buildStandby ran %d times after the second attempt, want 2", *builtCount)
	}
	if e.ctrl.evaluate(before.Add(59*time.Second), false) {
		t.Error("after a second consecutive failed build the controller retries within 59s: the backoff does not grow")
	}
	if !e.ctrl.evaluate(after.Add(11*time.Minute), false) {
		t.Error("the controller never recommends the switch again 11 minutes after two failed builds: the backoff is not capped")
	}
}

// TestLazyStandbyThatMissesTheBindDeadlineIsStopped: a standby that has not
// bound by the deadline must not be left running once the switch is abandoned.
func TestLazyStandbyThatMissesTheBindDeadlineIsStopped(t *testing.T) {
	e, _, _, builtCount := newLazyAdaptive(t)
	standby := &lateBindingStandby{mockEngine: newMockEngine(engine.IOUring), listenEnded: make(chan struct{})}
	e.buildStandby = func() (engine.Engine, error) {
		*builtCount++
		return standby, nil
	}

	e.performSwitch()

	if *builtCount != 1 || e.secondary != nil || e.ActiveEngine().Type() != engine.Epoll {
		t.Fatalf("switch did not abort: built=%d secondary=%v active=%v",
			*builtCount, e.secondary, e.ActiveEngine().Type())
	}
	select {
	case <-standby.listenStarted:
	default:
		t.Fatal("precondition: the standby's Listen never started")
	}
	select {
	case <-standby.listenEnded:
	case <-time.After(2 * time.Second):
		t.Fatal("the abandoned standby is still running 2s after the switch gave up on it: " +
			"it is in no slot, so no later switch or Shutdown will stop it")
	}
}
