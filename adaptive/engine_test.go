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

	// Make standby (epoll) score 20% better than active (io_uring).
	sampler.Set(engine.IOUring, TelemetrySnapshot{ThroughputRPS: 100, ErrorRate: 0.01})
	sampler.Set(engine.Epoll, TelemetrySnapshot{ThroughputRPS: 150, ErrorRate: 0.01})

	if e.ActiveEngine().Type() != engine.IOUring {
		t.Fatal("expected io_uring initially")
	}

	// Run evaluation — should trigger switch.
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

	// Trigger initial switch.
	sampler.Set(engine.IOUring, TelemetrySnapshot{ThroughputRPS: 100})
	sampler.Set(engine.Epoll, TelemetrySnapshot{ThroughputRPS: 200})

	now := time.Now()
	if !e.ctrl.evaluate(now, false) {
		t.Fatal("expected initial switch")
	}
	e.performSwitch()

	// Immediately try to switch back — should be blocked by cooldown.
	sampler.Set(engine.IOUring, TelemetrySnapshot{ThroughputRPS: 300})
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

	// Trigger switch.
	sampler.Set(engine.IOUring, TelemetrySnapshot{ThroughputRPS: 100})
	sampler.Set(engine.Epoll, TelemetrySnapshot{ThroughputRPS: 200})

	if !e.ctrl.evaluate(time.Now(), false) {
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
		sampler.Set(e.ActiveEngine().Type(), TelemetrySnapshot{ThroughputRPS: 50})
		other := engine.Epoll
		if e.ActiveEngine().Type() == engine.Epoll {
			other = engine.IOUring
		}
		sampler.Set(other, TelemetrySnapshot{ThroughputRPS: 200})

		if !e.ctrl.evaluate(now, false) {
			t.Fatal("expected switch before lock")
		}
		e.performSwitch()
		now = now.Add(10 * time.Second)
	}

	// Fourth switch should be locked.
	sampler.Set(e.ActiveEngine().Type(), TelemetrySnapshot{ThroughputRPS: 50})
	other := engine.Epoll
	if e.ActiveEngine().Type() == engine.Epoll {
		other = engine.IOUring
	}
	sampler.Set(other, TelemetrySnapshot{ThroughputRPS: 200})

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

	sampler.Set(engine.IOUring, TelemetrySnapshot{ThroughputRPS: 50})
	sampler.Set(engine.Epoll, TelemetrySnapshot{ThroughputRPS: 200})

	// Freeze switching.
	e.FreezeSwitching()

	if e.ctrl.evaluate(time.Now(), e.frozen.Load()) {
		t.Error("expected freeze to block evaluation")
	}

	// Unfreeze.
	e.UnfreezeSwitching()

	if !e.ctrl.evaluate(time.Now(), e.frozen.Load()) {
		t.Error("expected evaluation to proceed after unfreeze")
	}
}
