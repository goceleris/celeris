//go:build linux

package adaptive

import (
	"context"
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
	tests := []struct {
		protocol engine.Protocol
		wantType engine.EngineType
	}{
		{engine.HTTP1, engine.IOUring},
		{engine.H2C, engine.Epoll},
		{engine.Auto, engine.IOUring},
	}

	for _, tt := range tests {
		t.Run(tt.protocol.String(), func(t *testing.T) {
			primary := newMockEngine(engine.IOUring)
			secondary := newMockEngine(engine.Epoll)
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

func TestSwitchTrigger(t *testing.T) {
	primary := newMockEngine(engine.IOUring)
	secondary := newMockEngine(engine.Epoll)
	sampler := newSyntheticSampler()

	cfg := resource.Config{Protocol: engine.HTTP1}
	e := newFromEngines(primary, secondary, sampler, cfg)
	e.ctrl.evalInterval = 1 * time.Millisecond
	e.ctrl.cooldown = 0
	e.ctrl.minObserve = 0

	// Make active (io_uring) score 100.
	sampler.Set(engine.IOUring, TelemetrySnapshot{ThroughputRPS: 100, ErrorRate: 0.01})

	// Pre-seed standby historical score (epoll was previously active with score 150).
	e.ctrl.state.lastActiveScore[engine.Epoll] = 150
	e.ctrl.state.lastActiveTime[engine.Epoll] = time.Now()

	if e.ActiveEngine().Type() != engine.IOUring {
		t.Fatal("expected io_uring initially")
	}

	// Run evaluation — should trigger switch (150 > 100*1.15).
	now := time.Now()
	switched := e.ctrl.evaluate(now, false)
	if !switched {
		t.Fatal("expected switch to be recommended")
	}

	e.performSwitch()

	if e.ActiveEngine().Type() != engine.Epoll {
		t.Errorf("expected epoll after switch, got %v", e.ActiveEngine().Type())
	}
}

func TestHysteresis(t *testing.T) {
	primary := newMockEngine(engine.IOUring)
	secondary := newMockEngine(engine.Epoll)
	sampler := newSyntheticSampler()

	cfg := resource.Config{Protocol: engine.HTTP1}
	e := newFromEngines(primary, secondary, sampler, cfg)
	e.ctrl.cooldown = 1 * time.Hour // Very long cooldown to test blocking.
	e.ctrl.minObserve = 0

	// Pre-seed standby historical score so initial switch triggers.
	now := time.Now()
	e.ctrl.state.lastActiveScore[engine.Epoll] = 200
	e.ctrl.state.lastActiveTime[engine.Epoll] = now

	// Trigger initial switch.
	sampler.Set(engine.IOUring, TelemetrySnapshot{ThroughputRPS: 100})

	if !e.ctrl.evaluate(now, false) {
		t.Fatal("expected initial switch")
	}
	e.performSwitch()

	// Immediately try to switch back — should be blocked by cooldown.
	sampler.Set(engine.Epoll, TelemetrySnapshot{ThroughputRPS: 100})

	if e.ctrl.evaluate(now.Add(1*time.Second), false) {
		t.Error("switch should be blocked by cooldown")
	}
}

func TestConnectionDraining(t *testing.T) {
	primary := newMockEngine(engine.IOUring)
	secondary := newMockEngine(engine.Epoll)
	sampler := newSyntheticSampler()

	cfg := resource.Config{Protocol: engine.HTTP1}
	e := newFromEngines(primary, secondary, sampler, cfg)
	e.ctrl.cooldown = 0
	e.ctrl.minObserve = 0

	// Initial state: primary active, secondary should be paused.
	// Simulate the initial pause that Listen() would do.
	_ = secondary.PauseAccept()
	secondary.pauseCalls.Store(0) // reset counter

	// Pre-seed standby historical score.
	now := time.Now()
	e.ctrl.state.lastActiveScore[engine.Epoll] = 200
	e.ctrl.state.lastActiveTime[engine.Epoll] = now

	// Trigger switch.
	sampler.Set(engine.IOUring, TelemetrySnapshot{ThroughputRPS: 100})

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
	primary := newMockEngine(engine.IOUring)
	secondary := newMockEngine(engine.Epoll)
	sampler := newSyntheticSampler()

	cfg := resource.Config{Protocol: engine.HTTP1}
	e := newFromEngines(primary, secondary, sampler, cfg)
	e.ctrl.cooldown = 0
	e.ctrl.minObserve = 0

	now := time.Now()

	// Perform 3 switches rapidly.
	for range 3 {
		activeType := e.ActiveEngine().Type()
		sampler.Set(activeType, TelemetrySnapshot{ThroughputRPS: 50})
		other := engine.Epoll
		if activeType == engine.Epoll {
			other = engine.IOUring
		}
		// Pre-seed standby historical score for each iteration.
		e.ctrl.state.lastActiveScore[other] = 200
		e.ctrl.state.lastActiveTime[other] = now

		if !e.ctrl.evaluate(now, false) {
			t.Fatal("expected switch before lock")
		}
		e.performSwitch()
		now = now.Add(10 * time.Second)
	}

	// Fourth switch should be locked.
	activeType := e.ActiveEngine().Type()
	sampler.Set(activeType, TelemetrySnapshot{ThroughputRPS: 50})
	other := engine.Epoll
	if activeType == engine.Epoll {
		other = engine.IOUring
	}
	e.ctrl.state.lastActiveScore[other] = 200
	e.ctrl.state.lastActiveTime[other] = now

	if e.ctrl.evaluate(now, false) {
		t.Error("expected oscillation lock to prevent switch")
	}
	if !e.ctrl.state.locked {
		t.Error("expected locked state")
	}
}

func TestOverloadFreeze(t *testing.T) {
	primary := newMockEngine(engine.IOUring)
	secondary := newMockEngine(engine.Epoll)
	sampler := newSyntheticSampler()

	cfg := resource.Config{Protocol: engine.HTTP1}
	e := newFromEngines(primary, secondary, sampler, cfg)
	e.ctrl.cooldown = 0
	e.ctrl.minObserve = 0

	now := time.Now()
	sampler.Set(engine.IOUring, TelemetrySnapshot{ThroughputRPS: 50})

	// Pre-seed standby historical score.
	e.ctrl.state.lastActiveScore[engine.Epoll] = 200
	e.ctrl.state.lastActiveTime[engine.Epoll] = now

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

func TestResourceSplit(t *testing.T) {
	tests := []struct {
		workers     int
		wantWorkers int
	}{
		{1, 1},
		{2, 1},
		{4, 2},
		{8, 4},
		{16, 8},
	}
	for _, tt := range tests {
		cfg := resource.Config{
			Resources: resource.Resources{
				Workers: tt.workers,
			},
		}
		ioCfg, epCfg := splitResources(cfg)

		if ioCfg.Resources.Workers != tt.wantWorkers {
			t.Errorf("workers=%d: io_uring workers = %d, want %d", tt.workers, ioCfg.Resources.Workers, tt.wantWorkers)
		}
		if epCfg.Resources.Workers != tt.wantWorkers {
			t.Errorf("workers=%d: epoll workers = %d, want %d", tt.workers, epCfg.Resources.Workers, tt.wantWorkers)
		}

		// Both sub-engine configs should be identical.
		if ioCfg.Resources != epCfg.Resources {
			t.Errorf("workers=%d: io_uring and epoll resources differ", tt.workers)
		}

		// Per-connection settings should be unchanged.
		if ioCfg.Resources.BufferSize != cfg.Resources.BufferSize {
			t.Errorf("workers=%d: BufferSize changed", tt.workers)
		}
		if ioCfg.Resources.SocketRecv != cfg.Resources.SocketRecv {
			t.Errorf("workers=%d: SocketRecv changed", tt.workers)
		}
		if ioCfg.Resources.SocketSend != cfg.Resources.SocketSend {
			t.Errorf("workers=%d: SocketSend changed", tt.workers)
		}
	}
}

func TestHistoricalScoreDecay(t *testing.T) {
	primary := newMockEngine(engine.IOUring)
	secondary := newMockEngine(engine.Epoll)
	sampler := newSyntheticSampler()
	cfg := resource.Config{Protocol: engine.HTTP1}
	e := newFromEngines(primary, secondary, sampler, cfg)

	base := time.Now()
	e.ctrl.state.lastActiveScore[engine.Epoll] = 100.0
	e.ctrl.state.lastActiveTime[engine.Epoll] = base

	// At t=0, score should be 100.
	got := e.ctrl.historicalScore(engine.Epoll, base)
	if got != 100.0 {
		t.Errorf("at t=0: score = %f, want 100.0", got)
	}

	// At t=50s, score should be 50 (50% decay).
	got = e.ctrl.historicalScore(engine.Epoll, base.Add(50*time.Second))
	if got != 50.0 {
		t.Errorf("at t=50s: score = %f, want 50.0", got)
	}

	// At t=100s, score should be 0 (100% decay).
	got = e.ctrl.historicalScore(engine.Epoll, base.Add(100*time.Second))
	if got != 0.0 {
		t.Errorf("at t=100s: score = %f, want 0.0", got)
	}

	// At t=200s, score should still be 0 (clamped).
	got = e.ctrl.historicalScore(engine.Epoll, base.Add(200*time.Second))
	if got != 0.0 {
		t.Errorf("at t=200s: score = %f, want 0.0", got)
	}
}

func TestHistoricalScoreSeeding(t *testing.T) {
	primary := newMockEngine(engine.IOUring)
	secondary := newMockEngine(engine.Epoll)
	sampler := newSyntheticSampler()

	cfg := resource.Config{Protocol: engine.HTTP1}
	e := newFromEngines(primary, secondary, sampler, cfg)
	e.ctrl.cooldown = 0
	e.ctrl.minObserve = 0

	// Active (io_uring) has score 100. No standby history yet.
	sampler.Set(engine.IOUring, TelemetrySnapshot{ThroughputRPS: 100})

	now := time.Now()
	e.ctrl.evaluate(now, false)

	// Standby should be seeded at 80% of active.
	standbyScore, ok := e.ctrl.state.lastActiveScore[engine.Epoll]
	if !ok {
		t.Fatal("expected standby score to be seeded")
	}

	// Active score = 1.0*100 - 2.0*0 = 100. 80% = 80.
	if standbyScore != 80.0 {
		t.Errorf("standby seed score = %f, want 80.0", standbyScore)
	}
}

func TestSwitchAfterActiveDegrades(t *testing.T) {
	primary := newMockEngine(engine.IOUring)
	secondary := newMockEngine(engine.Epoll)
	sampler := newSyntheticSampler()

	cfg := resource.Config{Protocol: engine.HTTP1}
	e := newFromEngines(primary, secondary, sampler, cfg)
	e.ctrl.cooldown = 0
	e.ctrl.minObserve = 0

	now := time.Now()

	// Pre-seed standby (epoll) with a strong historical score.
	e.ctrl.state.lastActiveScore[engine.Epoll] = 100.0
	e.ctrl.state.lastActiveTime[engine.Epoll] = now

	// Active (io_uring) degrades to 50 RPS.
	sampler.Set(engine.IOUring, TelemetrySnapshot{ThroughputRPS: 50})

	// Standby historical = 100 * (1 - 0.01*0) = 100.
	// Active score = 50. 100 > 50*1.15 = 57.5 → switch.
	if !e.ctrl.evaluate(now, false) {
		t.Error("expected switch when active degrades below standby historical")
	}
}
