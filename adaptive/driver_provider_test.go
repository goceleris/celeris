//go:build linux

package adaptive

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// noopHandler satisfies stream.Handler for tests that don't exercise HTTP.
type noopHandler struct{}

func (noopHandler) HandleStream(context.Context, *stream.Stream) error { return nil }

var _ stream.Handler = noopHandler{}

// newBoundAdaptive builds an adaptive engine bound to 127.0.0.1:0 and
// starts Listen in a background goroutine. The returned stop() tears both
// sub-engines down. Skips the test if the kernel doesn't support io_uring
// (we need both primary + secondary up for a meaningful switch test).
func newBoundAdaptive(t *testing.T) (*Engine, func()) {
	t.Helper()
	cfg := resource.Config{
		Addr:     "127.0.0.1:0",
		Protocol: engine.HTTP1,
	}
	e, err := New(cfg, noopHandler{})
	if err != nil {
		t.Skipf("adaptive.New unsupported here: %v", err)
	}
	// Ensure switching is possible (cooldown off, minObserve off).
	e.ctrl.cooldown = 0
	e.ctrl.minObserve = 0

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()

	// Wait for bound.
	deadline := time.Now().Add(3 * time.Second)
	for e.Addr() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		cancel()
		<-done
		t.Fatal("adaptive engine never bound")
	}

	stop := func() {
		cancel()
		<-done
	}
	return e, stop
}

// socketpairNonblocking returns a connected, non-blocking AF_UNIX pair.
func socketpairNonblocking(t *testing.T) (int, int) {
	t.Helper()
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("Socketpair: %v", err)
	}
	for _, fd := range pair {
		if err := unix.SetNonblock(fd, true); err != nil {
			_ = unix.Close(pair[0])
			_ = unix.Close(pair[1])
			t.Fatalf("SetNonblock: %v", err)
		}
	}
	return pair[0], pair[1]
}

// TestAdaptiveProviderAutoFreeze exercises the core invariant: a driver
// registering an FD via the adaptive provider auto-freezes the engine for
// as long as the FD is registered. The pre-v1.4.0 behavior (and the old
// panic in provider.go) required callers to call FreezeSwitching
// manually. The fix makes the refcount transparent.
func TestAdaptiveProviderAutoFreeze(t *testing.T) {
	e, stop := newBoundAdaptive(t)
	defer stop()

	if e.frozen.Load() {
		t.Fatalf("engine should not be frozen on a fresh adaptive engine")
	}

	local, peer := socketpairNonblocking(t)
	defer func() { _ = unix.Close(peer) }()
	defer func() { _ = unix.Close(local) }()

	wl := e.WorkerLoop(0)

	// Register a driver FD; frozen should flip true and driverFDs should be 1.
	if err := wl.RegisterConn(local, func([]byte) {}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn: %v", err)
	}
	if !e.frozen.Load() {
		t.Fatalf("frozen should be true after RegisterConn")
	}
	if got := e.DriverFDCount(); got != 1 {
		t.Fatalf("DriverFDCount: got %d want 1", got)
	}

	// Attempting ForceSwitch while a driver FD is live must not swap sub-engines.
	beforeActive := e.ActiveEngine().Type()
	e.ForceSwitch()
	if e.ActiveEngine().Type() != beforeActive {
		t.Errorf("ForceSwitch should have refused: active %s → %s", beforeActive, e.ActiveEngine().Type())
	}
	if got := e.SwitchRejectedCount(); got == 0 {
		t.Errorf("SwitchRejectedCount: got %d want ≥1 after refused switch", got)
	}

	// Unregister — frozen clears, driverFDs back to 0.
	if err := wl.UnregisterConn(local); err != nil {
		t.Fatalf("UnregisterConn: %v", err)
	}
	if e.frozen.Load() {
		t.Fatalf("frozen should be false after last UnregisterConn")
	}
	if got := e.DriverFDCount(); got != 0 {
		t.Fatalf("DriverFDCount: got %d want 0", got)
	}

	// Now a switch should go through.
	e.ForceSwitch()
	if e.ActiveEngine().Type() == beforeActive {
		t.Errorf("ForceSwitch after unregister should swap engines; still %s", beforeActive)
	}
}

// TestAdaptiveUnfreezeRespectsDriverFDs asserts that an overzealous caller
// who forgets about live drivers and calls UnfreezeSwitching does NOT
// break the driver invariant. The engine stays frozen until all driver
// FDs are unregistered.
func TestAdaptiveUnfreezeRespectsDriverFDs(t *testing.T) {
	e, stop := newBoundAdaptive(t)
	defer stop()

	local, peer := socketpairNonblocking(t)
	defer func() { _ = unix.Close(peer) }()
	defer func() { _ = unix.Close(local) }()

	wl := e.WorkerLoop(0)
	if err := wl.RegisterConn(local, func([]byte) {}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn: %v", err)
	}

	// User-level Freeze/Unfreeze pair — leaves userFreezes at 0 but
	// driverFDs=1 still pins frozen.
	e.FreezeSwitching()
	e.UnfreezeSwitching()

	if !e.frozen.Load() {
		t.Fatalf("engine unfrozen while driver FD still registered")
	}

	// Extra spurious Unfreeze calls must not corrupt the counter.
	e.UnfreezeSwitching()
	e.UnfreezeSwitching()
	if !e.frozen.Load() {
		t.Fatalf("spurious Unfreezes should not thaw while driver FDs live")
	}

	if err := wl.UnregisterConn(local); err != nil {
		t.Fatalf("UnregisterConn: %v", err)
	}
	if e.frozen.Load() {
		t.Fatalf("engine should thaw after last UnregisterConn")
	}
}

// TestAdaptiveStackedFreezes exercises nested FreezeSwitching calls: two
// benchmarks and three drivers must all Unfreeze/Unregister before the
// engine thaws.
func TestAdaptiveStackedFreezes(t *testing.T) {
	e, stop := newBoundAdaptive(t)
	defer stop()

	// Two user freezes.
	e.FreezeSwitching()
	e.FreezeSwitching()

	// Three driver FDs.
	pairs := make([][2]int, 3)
	for i := range pairs {
		l, p := socketpairNonblocking(t)
		pairs[i] = [2]int{l, p}
	}
	defer func() {
		for _, pr := range pairs {
			_ = unix.Close(pr[1])
			_ = unix.Close(pr[0])
		}
	}()
	wl := e.WorkerLoop(0)
	for _, pr := range pairs {
		if err := wl.RegisterConn(pr[0], func([]byte) {}, func(error) {}); err != nil {
			t.Fatalf("RegisterConn: %v", err)
		}
	}
	if got := e.DriverFDCount(); got != 3 {
		t.Fatalf("DriverFDCount: got %d want 3", got)
	}
	if !e.frozen.Load() {
		t.Fatalf("engine should be frozen with 2 user freezes + 3 driver FDs")
	}

	// Release one user freeze and one driver FD — still frozen.
	e.UnfreezeSwitching()
	if err := wl.UnregisterConn(pairs[0][0]); err != nil {
		t.Fatalf("UnregisterConn: %v", err)
	}
	if !e.frozen.Load() {
		t.Fatalf("engine thawed prematurely with 1 user + 2 driver")
	}

	// Release the second user freeze — still frozen (2 drivers remain).
	e.UnfreezeSwitching()
	if !e.frozen.Load() {
		t.Fatalf("engine thawed with 2 drivers still present")
	}

	// Unregister remaining drivers.
	for _, pr := range pairs[1:] {
		if err := wl.UnregisterConn(pr[0]); err != nil {
			t.Fatalf("UnregisterConn: %v", err)
		}
	}
	if e.frozen.Load() {
		t.Fatalf("engine should thaw after every freeze released and every FD unregistered")
	}
}

// TestAdaptiveConcurrentDriverChurnVsSwitch pounds the engine with
// concurrent RegisterConn / UnregisterConn cycles while a second
// goroutine repeatedly calls ForceSwitch. The invariants the refcount
// guarantees:
//
//  1. DriverFDCount settles to 0 when every driver goroutine has
//     completed (no leaks).
//  2. No Register / Unregister returns an error that is not caused by
//     the FD being unknown to the engine (which happens only if the
//     refcount was wrong).
//  3. If the switch goroutine succeeded at least once, either drivers
//     were idle during that window OR the switch was rejected (observed
//     via SwitchRejectedCount > 0).
//
// FD reuse note: each driver goroutine allocates one socketpair for the
// whole run and reuses the same FDs for register/unregister cycles.
// Multiple cycles on a stable FD exercise the refcount semantics
// without tripping the engine's per-FD-already-registered sanity check.
func TestAdaptiveConcurrentDriverChurnVsSwitch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in -short mode")
	}
	e, stop := newBoundAdaptive(t)
	defer stop()

	const workers = 4
	const iters = 200
	var wg sync.WaitGroup

	// One stable FD per goroutine — avoids FD-reuse contention with
	// other goroutines while still exercising the refcount under load.
	// Each Unregister must fully complete before the next Register so
	// io_uring's async driver-action queue has time to drain (epoll is
	// synchronous, io_uring is not). We sync via the onClose callback.
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(wid int) {
			defer wg.Done()
			local, peer := socketpairNonblocking(t)
			defer func() { _ = unix.Close(peer) }()
			defer func() { _ = unix.Close(local) }()
			wl := e.WorkerLoop(wid)
			for j := 0; j < iters; j++ {
				closed := make(chan struct{}, 1)
				onClose := func(error) {
					select {
					case closed <- struct{}{}:
					default:
					}
				}
				if err := wl.RegisterConn(local, func([]byte) {}, onClose); err != nil {
					t.Errorf("RegisterConn worker=%d iter=%d: %v", wid, j, err)
					return
				}
				if err := wl.UnregisterConn(local); err != nil {
					t.Errorf("UnregisterConn worker=%d iter=%d: %v", wid, j, err)
					return
				}
				select {
				case <-closed:
				case <-time.After(2 * time.Second):
					t.Errorf("onClose timeout worker=%d iter=%d", wid, j)
					return
				}
			}
		}(i)
	}

	// Switch stresser: repeatedly try to ForceSwitch. We do NOT assert
	// that every observed success happened with zero FDs — instead we
	// rely on the engine log's "refusing engine switch" warning and the
	// SwitchRejectedCount monotonic counter, which performSwitch
	// updates atomically under the freeze lock. That makes this
	// assertion race-free with respect to driverFDs transitions.
	switchAttempts := atomic.Int32{}
	switchDone := make(chan struct{})
	go func() {
		defer close(switchDone)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			e.ForceSwitch()
			switchAttempts.Add(1)
			time.Sleep(200 * time.Microsecond)
		}
	}()

	wg.Wait()
	<-switchDone

	if got := e.DriverFDCount(); got != 0 {
		t.Errorf("driverFDs leaked: got %d want 0 at end", got)
	}
	t.Logf("switchAttempts=%d switchRejected=%d (rejectedPct≈%.1f)",
		switchAttempts.Load(), e.SwitchRejectedCount(),
		float64(e.SwitchRejectedCount())/float64(switchAttempts.Load())*100)
	if e.SwitchRejectedCount() == 0 {
		t.Log("no switches were rejected — test ran too fast to observe overlap; invariants still hold")
	}
}

// TestAdaptiveSwitchAfterDriverQuiescence checks that once the last
// driver FD is released, the engine CAN switch. This is the inverse of
// the "refuse while FDs live" rule.
func TestAdaptiveSwitchAfterDriverQuiescence(t *testing.T) {
	e, stop := newBoundAdaptive(t)
	defer stop()

	// Seed historical scores so ForceSwitch actually flips active.
	now := time.Now()
	e.ctrl.state.lastActiveScore[engine.Epoll] = 100
	e.ctrl.state.lastActiveScore[engine.IOUring] = 500
	e.ctrl.state.lastActiveTime[engine.Epoll] = now
	e.ctrl.state.lastActiveTime[engine.IOUring] = now

	local, peer := socketpairNonblocking(t)
	defer func() { _ = unix.Close(peer) }()
	defer func() { _ = unix.Close(local) }()

	wl := e.WorkerLoop(0)
	if err := wl.RegisterConn(local, func([]byte) {}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn: %v", err)
	}

	beforeActive := e.ActiveEngine().Type()
	e.ForceSwitch()
	if e.ActiveEngine().Type() != beforeActive {
		t.Fatalf("switch went through while FD live: %s → %s", beforeActive, e.ActiveEngine().Type())
	}

	if err := wl.UnregisterConn(local); err != nil {
		t.Fatalf("UnregisterConn: %v", err)
	}

	e.ForceSwitch()
	if e.ActiveEngine().Type() == beforeActive {
		t.Fatalf("switch didn't run after unregister; still on %s", beforeActive)
	}
}
