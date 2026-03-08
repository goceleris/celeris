//go:build linux

package integration

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/adaptive"
	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/internal/cpumon"
	"github.com/goceleris/celeris/overload"
)

func TestAdaptiveEngineLifecycle(t *testing.T) {
	port := freePort(t)
	cfg := defaultTestConfig(port, engine.Auto)
	cfg.Engine = engine.Adaptive

	e, err := adaptive.New(cfg, &echoHandler{})
	if err != nil {
		t.Skipf("adaptive engine not available: %v", err)
	}

	startEngine(t, e)

	if e.Type() != engine.Adaptive {
		t.Errorf("got type %v, want adaptive", e.Type())
	}

	m := e.Metrics()
	if m.ActiveConnections < 0 {
		t.Error("negative active connections")
	}
}

func TestOverloadManagerStageTransitions(t *testing.T) {
	mon := cpumon.NewSynthetic(0.0)
	hooks := &testHooks{workers: 4}
	cfg := overload.DefaultConfig()
	cfg.Interval = 1 * time.Millisecond
	for i := range cfg.Stages {
		cfg.Stages[i].EscalateSustained = 3 * time.Millisecond
		cfg.Stages[i].DeescalateSustained = 3 * time.Millisecond
		cfg.Stages[i].Cooldown = 0
	}

	mgr := overload.NewManager(cfg, mon, hooks, nil)

	// Escalate to Reject.
	mon.Set(0.97)
	runManager(t, mgr, 50*time.Millisecond)

	if mgr.Stage() != overload.Reject {
		t.Fatalf("expected Reject, got %v", mgr.Stage())
	}

	// Recover to Normal.
	mon.Set(0.10)
	runManager(t, mgr, 100*time.Millisecond)

	if mgr.Stage() != overload.Normal {
		t.Errorf("expected Normal after recovery, got %v", mgr.Stage())
	}
}

func TestOverloadFreezesAdaptive(t *testing.T) {
	mon := cpumon.NewSynthetic(0.0)
	hooks := &testHooks{workers: 4}
	cfg := overload.DefaultConfig()
	cfg.Interval = 1 * time.Millisecond
	for i := range cfg.Stages {
		cfg.Stages[i].EscalateSustained = 3 * time.Millisecond
		cfg.Stages[i].DeescalateSustained = 3 * time.Millisecond
		cfg.Stages[i].Cooldown = 0
	}

	mgr := overload.NewManager(cfg, mon, hooks, nil)

	var frozenMu sync.Mutex
	frozen := false
	mgr.SetFreezeHook(func(f bool) {
		frozenMu.Lock()
		frozen = f
		frozenMu.Unlock()
	})

	getFrozen := func() bool {
		frozenMu.Lock()
		defer frozenMu.Unlock()
		return frozen
	}

	// Escalate to Reorder (stage 3) where freeze kicks in.
	mon.Set(0.87)
	runManager(t, mgr, 30*time.Millisecond)

	if mgr.Stage() < overload.Reorder {
		t.Fatalf("expected at least Reorder, got %v", mgr.Stage())
	}
	if !getFrozen() {
		t.Error("expected freeze at Reorder+")
	}

	// Recover.
	mon.Set(0.20)
	runManager(t, mgr, 100*time.Millisecond)

	if mgr.Stage() >= overload.Reorder {
		t.Fatalf("expected below Reorder, got %v", mgr.Stage())
	}
	if getFrozen() {
		t.Error("expected unfreeze after recovery")
	}
}

func TestStateCombinationMatrix(t *testing.T) {
	mon := cpumon.NewSynthetic(0.0)
	hooks := &testHooks{workers: 4}
	cfg := overload.DefaultConfig()
	cfg.Interval = 1 * time.Millisecond
	for i := range cfg.Stages {
		cfg.Stages[i].EscalateSustained = 2 * time.Millisecond
		cfg.Stages[i].DeescalateSustained = 2 * time.Millisecond
		cfg.Stages[i].Cooldown = 0
	}

	mgr := overload.NewManager(cfg, mon, hooks, nil)

	cpuValues := []float64{0.10, 0.72, 0.82, 0.87, 0.92, 0.97, 0.50, 0.10}
	for _, cpu := range cpuValues {
		mon.Set(cpu)
		runManager(t, mgr, 10*time.Millisecond)
		// Just verify no panic or deadlock.
		_ = mgr.Stage()
	}
}

func runManager(t *testing.T, mgr *overload.Manager, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), d)
	defer cancel()
	_ = mgr.Run(ctx)
}

// testHooks is a minimal EngineHooks implementation for integration tests.
type testHooks struct {
	mu          sync.Mutex
	workers     int
	activeConns atomic.Int64
}

func (h *testHooks) ExpandWorkers(n int) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.workers = n
	return n
}

func (h *testHooks) ReapIdleConnections(_ time.Duration) int { return 0 }
func (h *testHooks) SetSchedulingMode(_ bool)                {}
func (h *testHooks) SetAcceptPaused(_ bool)                  {}
func (h *testHooks) SetAcceptDelay(_ time.Duration)          {}
func (h *testHooks) SetMaxConcurrent(_ int)                  {}

func (h *testHooks) ShrinkWorkers(base int, _ time.Duration) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.workers = base
	return base
}

func (h *testHooks) ActiveConnections() int64 {
	return h.activeConns.Load()
}

func (h *testHooks) WorkerCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.workers
}
