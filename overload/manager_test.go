package overload

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/cpumon"
)

type mockHooks struct {
	mu            sync.Mutex
	expandCalls   int
	reapCalls     int
	lifoMode      bool
	acceptPaused  bool
	acceptDelay   time.Duration
	maxConcurrent int
	shrinkCalls   int
	activeConns   int64
	workers       int
}

func newMockHooks() *mockHooks {
	return &mockHooks{workers: 4}
}

func (m *mockHooks) ExpandWorkers(n int) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expandCalls++
	m.workers = n
	return n
}

func (m *mockHooks) ReapIdleConnections(_ time.Duration) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reapCalls++
	return 5
}

func (m *mockHooks) SetSchedulingMode(lifo bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lifoMode = lifo
}

func (m *mockHooks) SetAcceptPaused(paused bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.acceptPaused = paused
}

func (m *mockHooks) SetAcceptDelay(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.acceptDelay = d
}

func (m *mockHooks) SetMaxConcurrent(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.maxConcurrent = n
}

func (m *mockHooks) ShrinkWorkers(base int, _ time.Duration) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.shrinkCalls++
	m.workers = base
	return base
}

func (m *mockHooks) ActiveConnections() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeConns
}

func (m *mockHooks) WorkerCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.workers
}

func (m *mockHooks) getAcceptPaused() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.acceptPaused
}

func (m *mockHooks) getAcceptDelay() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.acceptDelay
}

func (m *mockHooks) getMaxConcurrent() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.maxConcurrent
}

func fastConfig() Config {
	cfg := DefaultConfig()
	cfg.Interval = 1 * time.Millisecond
	for i := range cfg.Stages {
		cfg.Stages[i].EscalateSustained = 3 * time.Millisecond
		cfg.Stages[i].DeescalateSustained = 3 * time.Millisecond
		cfg.Stages[i].Cooldown = 0
	}
	return cfg
}

func runForDuration(t *testing.T, mgr *Manager, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), d)
	defer cancel()
	_ = mgr.Run(ctx)
}

func TestStageEscalation(t *testing.T) {
	mon := cpumon.NewSynthetic(0.0)
	hooks := newMockHooks()
	cfg := fastConfig()
	mgr := NewManager(cfg, mon, hooks, nil)

	stages := []struct {
		cpu  float64
		want Stage
	}{
		{0.72, Expand},
		{0.82, Reap},
		{0.87, Reorder},
		{0.92, Backpressure},
		{0.97, Reject},
	}

	for _, tc := range stages {
		mon.Set(tc.cpu)
		runForDuration(t, mgr, 30*time.Millisecond)
		got := mgr.Stage()
		if got != tc.want {
			t.Errorf("cpu=%.2f: got stage %v, want %v", tc.cpu, got, tc.want)
		}
	}
}

func TestDeescalationHysteresis(t *testing.T) {
	mon := cpumon.NewSynthetic(0.88)
	hooks := newMockHooks()
	cfg := fastConfig()
	mgr := NewManager(cfg, mon, hooks, nil)

	// Escalate to Reorder (stage 3): 0.88 exceeds 0.85 threshold.
	runForDuration(t, mgr, 80*time.Millisecond)
	if mgr.Stage() < Reorder {
		t.Fatalf("expected at least Reorder, got %v", mgr.Stage())
	}

	// Drop CPU to 0.74 (below de-escalate threshold of 0.75 for Reorder).
	mon.Set(0.74)
	runForDuration(t, mgr, 50*time.Millisecond)

	got := mgr.Stage()
	if got >= Reorder {
		t.Errorf("expected below Reorder after CPU drop, got %v", got)
	}
}

func Test503AtStage5(t *testing.T) {
	mon := cpumon.NewSynthetic(0.97)
	hooks := newMockHooks()
	cfg := fastConfig()
	mgr := NewManager(cfg, mon, hooks, nil)

	runForDuration(t, mgr, 150*time.Millisecond)

	if mgr.Stage() != Reject {
		t.Fatalf("expected Reject, got %v", mgr.Stage())
	}
	if !hooks.getAcceptPaused() {
		t.Error("expected SetAcceptPaused(true) at Reject stage")
	}
}

func TestBackpressureAtStage4(t *testing.T) {
	mon := cpumon.NewSynthetic(0.92)
	hooks := newMockHooks()
	hooks.mu.Lock()
	hooks.activeConns = 100
	hooks.mu.Unlock()
	cfg := fastConfig()
	mgr := NewManager(cfg, mon, hooks, nil)

	runForDuration(t, mgr, 120*time.Millisecond)

	if mgr.Stage() < Backpressure {
		t.Fatalf("expected at least Backpressure, got %v", mgr.Stage())
	}
	if hooks.getAcceptDelay() == 0 {
		t.Error("expected non-zero accept delay at Backpressure")
	}
	if hooks.getMaxConcurrent() == 0 {
		t.Error("expected non-zero max concurrent at Backpressure")
	}
}

func TestRecovery(t *testing.T) {
	mon := cpumon.NewSynthetic(0.97)
	hooks := newMockHooks()
	cfg := fastConfig()
	mgr := NewManager(cfg, mon, hooks, nil)

	// Escalate to Reject.
	runForDuration(t, mgr, 150*time.Millisecond)
	if mgr.Stage() != Reject {
		t.Fatalf("expected Reject, got %v", mgr.Stage())
	}

	// Recover fully.
	mon.Set(0.20)
	runForDuration(t, mgr, 200*time.Millisecond)

	if mgr.Stage() != Normal {
		t.Errorf("expected Normal after recovery, got %v", mgr.Stage())
	}
	if hooks.getAcceptPaused() {
		t.Error("accept should be unpaused after recovery")
	}
}

func TestAdaptiveFreeze(t *testing.T) {
	mon := cpumon.NewSynthetic(0.0)
	hooks := newMockHooks()
	cfg := fastConfig()
	mgr := NewManager(cfg, mon, hooks, nil)

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
	mon.Set(0.88)
	runForDuration(t, mgr, 80*time.Millisecond)

	if mgr.Stage() < Reorder {
		t.Fatalf("expected at least Reorder, got %v", mgr.Stage())
	}
	if !getFrozen() {
		t.Error("expected freeze at stage >= Reorder")
	}

	// Drop below Reorder threshold.
	mon.Set(0.50)
	runForDuration(t, mgr, 50*time.Millisecond)

	if mgr.Stage() >= Reorder {
		t.Fatalf("expected below Reorder, got %v", mgr.Stage())
	}
	if getFrozen() {
		t.Error("expected unfreeze below Reorder")
	}
}

func TestNoEscalationWithoutSustained(t *testing.T) {
	mon := cpumon.NewSynthetic(0.0)
	hooks := newMockHooks()
	cfg := fastConfig()
	cfg.Stages[1].EscalateSustained = 100 * time.Millisecond // long sustained requirement
	mgr := NewManager(cfg, mon, hooks, nil)

	// Briefly spike above threshold then drop.
	mon.Set(0.75)
	runForDuration(t, mgr, 5*time.Millisecond)
	mon.Set(0.30)
	runForDuration(t, mgr, 5*time.Millisecond)

	if mgr.Stage() != Normal {
		t.Errorf("expected Normal (not sustained), got %v", mgr.Stage())
	}
}

func TestFreezeSuppression(t *testing.T) {
	mon := cpumon.NewSynthetic(0.0)
	hooks := newMockHooks()
	cfg := fastConfig()
	mgr := NewManager(cfg, mon, hooks, nil)

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

	// Suppress freeze for a long duration.
	mgr.SuppressFreeze(5 * time.Second)

	// Escalate to Reorder (stage 3). Freeze hook should NOT fire.
	mon.Set(0.88)
	runForDuration(t, mgr, 80*time.Millisecond)

	if mgr.Stage() < Reorder {
		t.Fatalf("expected at least Reorder, got %v", mgr.Stage())
	}
	if getFrozen() {
		t.Error("freeze hook should be suppressed during suppression period")
	}
}

func TestFreezeFiringAfterSuppressionExpiry(t *testing.T) {
	mon := cpumon.NewSynthetic(0.0)
	hooks := newMockHooks()
	cfg := fastConfig()
	mgr := NewManager(cfg, mon, hooks, nil)

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

	// Suppress for a very short duration (already expired).
	mgr.SuppressFreeze(0)

	// Escalate to Reorder. Freeze hook SHOULD fire (suppression expired).
	mon.Set(0.88)
	runForDuration(t, mgr, 80*time.Millisecond)

	if mgr.Stage() < Reorder {
		t.Fatalf("expected at least Reorder, got %v", mgr.Stage())
	}
	if !getFrozen() {
		t.Error("freeze hook should fire after suppression expiry")
	}
}

func TestSuppressDoesNotBlockOtherStages(t *testing.T) {
	mon := cpumon.NewSynthetic(0.0)
	hooks := newMockHooks()
	cfg := fastConfig()
	mgr := NewManager(cfg, mon, hooks, nil)

	// Suppress freeze for a long duration.
	mgr.SuppressFreeze(5 * time.Second)

	// Escalate to Reject (stage 5). All stages should fire except freeze.
	mon.Set(0.97)
	runForDuration(t, mgr, 150*time.Millisecond)

	if mgr.Stage() != Reject {
		t.Fatalf("expected Reject, got %v", mgr.Stage())
	}
	if !hooks.getAcceptPaused() {
		t.Error("SetAcceptPaused should still be called at Reject stage")
	}
}
