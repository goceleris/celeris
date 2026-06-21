//go:build linux

package adaptive

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/resource"
)

// mockEngine is a mock engine for testing the adaptive controller.
type mockEngine struct {
	engineType    engine.EngineType
	metrics       engine.EngineMetrics
	mu            sync.Mutex
	addr          net.Addr
	acceptPaused  atomic.Bool
	pauseCalls    atomic.Int32
	resumeCalls   atomic.Int32
	listenStarted chan struct{}
}

func newMockEngine(et engine.EngineType) *mockEngine {
	return &mockEngine{
		engineType:    et,
		addr:          &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9000 + int(et)},
		listenStarted: make(chan struct{}),
	}
}

func (m *mockEngine) Listen(ctx context.Context) error {
	close(m.listenStarted)
	<-ctx.Done()
	return nil
}

func (m *mockEngine) Shutdown(_ context.Context) error { return nil }

func (m *mockEngine) Metrics() engine.EngineMetrics {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.metrics
}

func (m *mockEngine) SetMetrics(met engine.EngineMetrics) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.metrics = met
}

func (m *mockEngine) Type() engine.EngineType { return m.engineType }

func (m *mockEngine) Addr() net.Addr { return m.addr }

func (m *mockEngine) PauseAccept() error {
	m.acceptPaused.Store(true)
	m.pauseCalls.Add(1)
	return nil
}

func (m *mockEngine) ResumeAccept() error {
	m.acceptPaused.Store(false)
	m.resumeCalls.Add(1)
	return nil
}

func TestInitialBias(t *testing.T) {
	// newFromEngines always starts with primary. In production, New()
	// creates epoll as primary, so all protocols start with epoll.
	tests := []struct {
		protocol engine.Protocol
		wantType engine.EngineType
	}{
		{engine.HTTP1, engine.Epoll},
		{engine.H2C, engine.Epoll},
		{engine.Auto, engine.Epoll},
	}

	for _, tt := range tests {
		t.Run(tt.protocol.String(), func(t *testing.T) {
			primary := newMockEngine(engine.Epoll)
			secondary := newMockEngine(engine.IOUring)
			sampler := newSyntheticSampler()

			cfg := resource.Config{Protocol: tt.protocol}
			e := newFromEngines(primary, secondary, sampler, cfg)

			got := e.ActiveEngine().Type()
			if got != tt.wantType {
				t.Errorf("protocol %s: got active %v, want %v", tt.protocol, got, tt.wantType)
			}
		})
	}
}

// newAdaptiveStartingOnEpoll builds an adaptive engine whose active engine is
// epoll (primary), the production starting configuration. The synthetic
// sampler returned lets the test drive conns-per-worker for the active engine.
func newAdaptiveStartingOnEpoll(t *testing.T) (*Engine, *syntheticSampler) {
	t.Helper()
	primary := newMockEngine(engine.Epoll)     // active
	secondary := newMockEngine(engine.IOUring) // standby
	sampler := newSyntheticSampler()
	e := newFromEngines(primary, secondary, sampler, resource.Config{Protocol: engine.HTTP1})
	e.ctrl.cooldown = 0
	return e, sampler
}

// newAdaptiveStartingOnIOUring builds an adaptive engine whose active engine is
// io_uring (primary), for exercising the revert-to-epoll path.
func newAdaptiveStartingOnIOUring(t *testing.T) (*Engine, *syntheticSampler) {
	t.Helper()
	primary := newMockEngine(engine.IOUring)
	secondary := newMockEngine(engine.Epoll)
	sampler := newSyntheticSampler()
	e := newFromEngines(primary, secondary, sampler, resource.Config{Protocol: engine.HTTP1})
	e.ctrl.cooldown = 0
	return e, sampler
}

// TestSwitchTrigger: sustained high conns/worker on the active epoll engine
// triggers an epoll→io_uring switch after sustainTicks ticks.
func TestSwitchTrigger(t *testing.T) {
	e, sampler := newAdaptiveStartingOnEpoll(t)
	sampler.Set(engine.Epoll, TelemetrySnapshot{ConnsPerWorker: 24})

	if e.ActiveEngine().Type() != engine.Epoll {
		t.Fatal("expected epoll initially")
	}

	now := time.Now()
	// First tick arms the sustain counter but must not switch yet.
	if e.ctrl.evaluate(now, false) {
		t.Fatal("switch must require sustainTicks consecutive ticks, not one")
	}
	// Second tick crosses sustainTicks → switch.
	if !e.ctrl.evaluate(now.Add(time.Second), false) {
		t.Fatal("expected switch after sustained high conns/worker")
	}

	e.performSwitch()
	if e.ActiveEngine().Type() != engine.IOUring {
		t.Errorf("expected io_uring after switch, got %v", e.ActiveEngine().Type())
	}
}

// TestSwitchFastPath: conns/worker above the high watermark snaps to io_uring
// on a single tick (no sustain wait).
func TestSwitchFastPath(t *testing.T) {
	e, sampler := newAdaptiveStartingOnEpoll(t)
	sampler.Set(engine.Epoll, TelemetrySnapshot{ConnsPerWorker: 50})

	if !e.ctrl.evaluate(time.Now(), false) {
		t.Fatal("expected heavy-load fast-path snap on a single tick")
	}
}

// TestNoSwitchInHysteresisBand: conns/worker between the down and up
// thresholds (12–20) must not switch in either direction.
func TestNoSwitchInHysteresisBand(t *testing.T) {
	e, sampler := newAdaptiveStartingOnEpoll(t)
	sampler.Set(engine.Epoll, TelemetrySnapshot{ConnsPerWorker: 16})

	now := time.Now()
	for i := range 5 {
		if e.ctrl.evaluate(now.Add(time.Duration(i)*time.Second), false) {
			t.Fatal("must not switch up inside the 12–20 hysteresis band")
		}
	}
}

// TestLargePayloadSuppressesSwitch: even far above the high watermark, a
// large average payload (link-bound) must suppress the io_uring switch.
func TestLargePayloadSuppressesSwitch(t *testing.T) {
	e, sampler := newAdaptiveStartingOnEpoll(t)
	sampler.Set(engine.Epoll, TelemetrySnapshot{ConnsPerWorker: 64, BytesPerReq: 32768})

	now := time.Now()
	for i := range 5 {
		if e.ctrl.evaluate(now.Add(time.Duration(i)*time.Second), false) {
			t.Fatal("large payload (link-bound) must suppress io_uring switch")
		}
	}
}

// TestRevertOnLowLoad: io_uring active, conns/worker drops below the down
// threshold for sustainTicks → revert to epoll.
func TestRevertOnLowLoad(t *testing.T) {
	e, sampler := newAdaptiveStartingOnIOUring(t)
	sampler.Set(engine.IOUring, TelemetrySnapshot{ConnsPerWorker: 4})

	now := time.Now()
	if e.ctrl.evaluate(now, false) {
		t.Fatal("revert must require sustainTicks below the down threshold")
	}
	if !e.ctrl.evaluate(now.Add(time.Second), false) {
		t.Fatal("expected revert to epoll after sustained low load")
	}
}

// TestErrorRevert: an error rate above errorRevertRate forces a revert from
// io_uring to epoll regardless of load (even at high conns/worker).
func TestErrorRevert(t *testing.T) {
	e, sampler := newAdaptiveStartingOnIOUring(t)
	sampler.Set(engine.IOUring, TelemetrySnapshot{ConnsPerWorker: 40, ErrorRate: 0.10})

	if !e.ctrl.evaluate(time.Now(), false) {
		t.Fatal("expected error-rate safety revert from io_uring")
	}
}

// TestNoRevertWhenIOUringBusy: io_uring active with conns/worker above the
// down threshold and no errors must stay on io_uring.
func TestNoRevertWhenIOUringBusy(t *testing.T) {
	e, sampler := newAdaptiveStartingOnIOUring(t)
	sampler.Set(engine.IOUring, TelemetrySnapshot{ConnsPerWorker: 24})

	now := time.Now()
	for i := range 5 {
		if e.ctrl.evaluate(now.Add(time.Duration(i)*time.Second), false) {
			t.Fatal("must not revert while io_uring is still busy")
		}
	}
}

// TestCooldownGate: after a switch, a revert is blocked until the cooldown
// window elapses, even when the revert condition holds.
func TestCooldownGate(t *testing.T) {
	e, sampler := newAdaptiveStartingOnEpoll(t)
	e.ctrl.cooldown = 1 * time.Hour

	now := time.Now()
	// Fast-path switch epoll→io_uring.
	sampler.Set(engine.Epoll, TelemetrySnapshot{ConnsPerWorker: 50})
	if !e.ctrl.evaluate(now, false) {
		t.Fatal("expected initial fast-path switch")
	}
	e.performSwitch()

	// Now io_uring active and idle — but cooldown blocks the revert.
	sampler.Set(engine.IOUring, TelemetrySnapshot{ConnsPerWorker: 0})
	if e.ctrl.evaluate(now.Add(1*time.Second), false) {
		t.Error("revert should be blocked by cooldown")
	}
}

func TestConnectionDraining(t *testing.T) {
	e, sampler := newAdaptiveStartingOnEpoll(t)
	primary := e.primary.(*mockEngine)
	secondary := e.secondary.(*mockEngine)

	// Simulate the initial pause that Listen() would do on the standby.
	_ = secondary.PauseAccept()
	secondary.pauseCalls.Store(0) // reset counter

	now := time.Now()
	sampler.Set(engine.Epoll, TelemetrySnapshot{ConnsPerWorker: 50}) // fast-path

	if !e.ctrl.evaluate(now, false) {
		t.Fatal("expected switch")
	}
	e.performSwitch()

	// Primary (now standby) should have PauseAccept called.
	if primary.pauseCalls.Load() == 0 {
		t.Error("expected PauseAccept on old active engine")
	}
	// Secondary (now active) should have ResumeAccept called.
	if secondary.resumeCalls.Load() == 0 {
		t.Error("expected ResumeAccept on new active engine")
	}
}

func TestOscillationLock(t *testing.T) {
	e, sampler := newAdaptiveStartingOnEpoll(t)

	now := time.Now()

	// Perform 3 switches rapidly via the fast path; each iteration sets a
	// load that triggers the active engine's switch direction.
	for range 3 {
		switch e.ActiveEngine().Type() {
		case engine.Epoll:
			sampler.Set(engine.Epoll, TelemetrySnapshot{ConnsPerWorker: 50}) // switch up
		case engine.IOUring:
			sampler.Set(engine.IOUring, TelemetrySnapshot{ConnsPerWorker: 40, ErrorRate: 0.5}) // error revert
		}
		if !e.ctrl.evaluate(now, false) {
			t.Fatal("expected switch before lock")
		}
		e.performSwitch()
		now = now.Add(10 * time.Second)
	}

	// Fourth switch should be locked (3 switches within 5 minutes).
	switch e.ActiveEngine().Type() {
	case engine.Epoll:
		sampler.Set(engine.Epoll, TelemetrySnapshot{ConnsPerWorker: 50})
	case engine.IOUring:
		sampler.Set(engine.IOUring, TelemetrySnapshot{ConnsPerWorker: 40, ErrorRate: 0.5})
	}
	if e.ctrl.evaluate(now, false) {
		t.Error("expected oscillation lock to prevent switch")
	}
	if !e.ctrl.state.locked {
		t.Error("expected locked state")
	}
}

func TestOverloadFreeze(t *testing.T) {
	e, sampler := newAdaptiveStartingOnEpoll(t)

	now := time.Now()
	sampler.Set(engine.Epoll, TelemetrySnapshot{ConnsPerWorker: 50}) // fast-path load

	// Freeze switching.
	e.FreezeSwitching()
	if e.ctrl.evaluate(now, e.frozen.Load()) {
		t.Error("expected freeze to block evaluation")
	}

	// Unfreeze.
	e.UnfreezeSwitching()
	if !e.ctrl.evaluate(now, e.frozen.Load()) {
		t.Error("expected evaluation to proceed after unfreeze")
	}
}

// TestSwitchTriggersFreezeSuppression is deferred until SetFreezeSuppressor
// is implemented on *Engine (post-v1.0.0).

// --- Part A: feature-gated start-engine selection ------------------------

// chooseStartEngine + env-override + ioUringViable coverage now lives in
// start_test.go (the policy was redesigned: default epoll, io_uring only on an
// explicit high-concurrency hint with a viable kernel + memlock + non-h2c).

// --- Part B: lazy standby build on first switch + reuse ------------------

// newLazyAdaptive builds an adaptive engine in the LAZY shape: epoll is the
// eager active (primary), io_uring is nil and built on demand by a counting
// buildStandby. listenCtx/listenWG are populated as Listen would, so
// performSwitch can launch + bind-wait the lazily-built standby. The returned
// counter pointer records how many times buildStandby ran.
func newLazyAdaptive(t *testing.T) (*Engine, *syntheticSampler, *mockEngine, *int) {
	t.Helper()
	active := newMockEngine(engine.Epoll)
	sampler := newSyntheticSampler()

	var builtCount int
	lazyStandby := newMockEngine(engine.IOUring)
	e := &Engine{
		primary:   active, // epoll active
		secondary: nil,    // io_uring lazy standby
		cfg:       resource.Config{Protocol: engine.HTTP1},
		logger:    testLogger(),
		startType: engine.Epoll,
	}
	e.buildStandby = func() (engine.Engine, error) {
		builtCount++
		return lazyStandby, nil
	}
	e.ctrl = newController(e.primary, e.secondary, sampler, e.logger)
	e.ctrl.state.activeIsPrimary = true
	e.ctrl.cooldown = 0
	var ap engine.Engine = active
	e.active.Store(&ap)

	// Wire the Listen-owned ctx + wait group so performSwitch can start the
	// lazily-built standby's Listen goroutine.
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	var wg sync.WaitGroup
	// Hold the wait group above zero for the test's lifetime (mirrors the
	// eval-loop goroutine being a wg member in production), so wg.Go from
	// performSwitch is always called while the counter is positive.
	wg.Add(1)
	t.Cleanup(func() { wg.Done() })
	e.listenCtx = ctx
	e.listenWG = &wg

	return e, sampler, lazyStandby, &builtCount
}

// TestLazyStandbyBuiltOnFirstSwitchAndReused verifies the lazy New() path
// builds the standby exactly once (on the first switch) and reuses it on a
// subsequent switch back.
func TestLazyStandbyBuiltOnFirstSwitchAndReused(t *testing.T) {
	e, _, lazyStandby, builtCount := newLazyAdaptive(t)

	if e.secondary != nil {
		t.Fatal("standby must be nil before the first switch")
	}
	if *builtCount != 0 {
		t.Fatalf("buildStandby ran %d times before any switch, want 0", *builtCount)
	}

	// First switch: epoll -> io_uring. Must build + cache + activate the standby.
	e.performSwitch()

	if *builtCount != 1 {
		t.Fatalf("buildStandby ran %d times after first switch, want 1", *builtCount)
	}
	if e.secondary == nil {
		t.Fatal("standby must be cached after the first switch")
	}
	if e.ActiveEngine().Type() != engine.IOUring {
		t.Fatalf("active = %v after first switch, want io_uring", e.ActiveEngine().Type())
	}
	// The lazily-built standby must have been Listen'd (its goroutine ran).
	select {
	case <-lazyStandby.listenStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("lazy standby Listen was never started")
	}
	// New active (io_uring) must have been resumed; old active (epoll) paused.
	if lazyStandby.resumeCalls.Load() == 0 {
		t.Error("expected ResumeAccept on the freshly-built (now active) standby")
	}
	if e.primary.(*mockEngine).pauseCalls.Load() == 0 {
		t.Error("expected PauseAccept on the old active (epoll)")
	}

	// Second switch: io_uring -> epoll. The standby slot for THIS direction is
	// epoll (e.primary, already non-nil), so no new build happens. The io_uring
	// engine stays cached too.
	e.performSwitch()

	if *builtCount != 1 {
		t.Fatalf("buildStandby ran %d times after second switch, want 1 (reuse)", *builtCount)
	}
	if e.ActiveEngine().Type() != engine.Epoll {
		t.Fatalf("active = %v after second switch, want epoll", e.ActiveEngine().Type())
	}
	if e.secondary != lazyStandby {
		t.Error("io_uring standby must remain cached (same instance) after switching back")
	}

	// Third switch: epoll -> io_uring again. Reuses the cached io_uring standby.
	e.performSwitch()
	if *builtCount != 1 {
		t.Fatalf("buildStandby ran %d times after third switch, want 1 (reuse)", *builtCount)
	}
	if e.ActiveEngine().Type() != engine.IOUring {
		t.Fatalf("active = %v after third switch, want io_uring", e.ActiveEngine().Type())
	}
}

// TestLazyStandbyBuildFailureAbortsSwitch verifies a buildStandby error aborts
// the switch cleanly: the active engine is unchanged and the standby slot stays
// nil.
func TestLazyStandbyBuildFailureAbortsSwitch(t *testing.T) {
	e, _, _, _ := newLazyAdaptive(t)
	e.buildStandby = func() (engine.Engine, error) {
		return nil, errBuildFailed
	}

	e.performSwitch()

	if e.ActiveEngine().Type() != engine.Epoll {
		t.Fatalf("active changed to %v after a failed build; must stay epoll", e.ActiveEngine().Type())
	}
	if e.secondary != nil {
		t.Fatal("standby slot must stay nil after a failed build")
	}
	if e.ctrl.state.activeIsPrimary != true {
		t.Fatal("controller direction must be unchanged after a failed build")
	}
}

var errBuildFailed = errors.New("build failed")
