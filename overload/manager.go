package overload

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/goceleris/celeris/internal/cpumon"
)

// Manager monitors CPU utilization and drives a 5-stage degradation ladder.
type Manager struct {
	mu           sync.Mutex
	cfg          Config
	cpuMon       cpumon.Monitor
	hooks        EngineHooks
	freezeHook   func(frozen bool)
	logger       *slog.Logger
	currentStage Stage

	escalateAboveSince   time.Time
	deescalateBelowSince time.Time
	cooldownUntil        time.Time

	baseWorkers int
}

// NewManager creates an overload manager.
func NewManager(cfg Config, mon cpumon.Monitor, hooks EngineHooks, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		cfg:         cfg,
		cpuMon:      mon,
		hooks:       hooks,
		logger:      logger,
		baseWorkers: hooks.WorkerCount(),
	}
}

// SetFreezeHook registers a callback for freeze/unfreeze (adaptive engine integration).
func (m *Manager) SetFreezeHook(fn func(frozen bool)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.freezeHook = fn
}

// Stage returns the current overload stage.
func (m *Manager) Stage() Stage {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.currentStage
}

// Run starts the overload evaluation loop. Blocks until ctx is canceled.
func (m *Manager) Run(ctx context.Context) error {
	if !m.cfg.Enabled {
		<-ctx.Done()
		return nil
	}

	ticker := time.NewTicker(m.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-ticker.C:
			m.evaluate(now)
		}
	}
}

func (m *Manager) evaluate(now time.Time) {
	sample, err := m.cpuMon.Sample()
	if err != nil {
		m.logger.Error("cpu sample failed", "err", err)
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	cpu := sample.Utilization
	current := m.currentStage

	// Check escalation to next stage.
	if current < Reject {
		next := current + 1
		sc := m.cfg.Stages[next]
		if cpu >= sc.EscalateThreshold {
			if m.escalateAboveSince.IsZero() {
				m.escalateAboveSince = now
			} else if now.Sub(m.escalateAboveSince) >= sc.EscalateSustained {
				m.escalateTo(next, now)
				m.escalateAboveSince = time.Time{}
				m.deescalateBelowSince = time.Time{}
				return
			}
		} else {
			m.escalateAboveSince = time.Time{}
		}
	}

	// Check de-escalation from current stage.
	if current > Normal {
		sc := m.cfg.Stages[current]
		if !now.Before(m.cooldownUntil) && cpu < sc.DeescalateThreshold {
			if m.deescalateBelowSince.IsZero() {
				m.deescalateBelowSince = now
			} else if now.Sub(m.deescalateBelowSince) >= sc.DeescalateSustained {
				m.deescalateTo(current-1, now)
				m.deescalateBelowSince = time.Time{}
				m.escalateAboveSince = time.Time{}
			}
		} else if cpu >= sc.DeescalateThreshold {
			m.deescalateBelowSince = time.Time{}
		}
	}
}

func (m *Manager) escalateTo(stage Stage, now time.Time) {
	prev := m.currentStage
	m.currentStage = stage

	m.logger.Warn("overload escalation", "from", prev.String(), "to", stage.String())

	switch stage {
	case Expand:
		m.hooks.ExpandWorkers(m.baseWorkers * 2)
	case Reap:
		m.hooks.ReapIdleConnections(30 * time.Second)
	case Reorder:
		m.hooks.SetSchedulingMode(true)
		if m.freezeHook != nil {
			m.freezeHook(true)
		}
	case Backpressure:
		m.hooks.SetAcceptDelay(10 * time.Millisecond)
		m.hooks.SetMaxConcurrent(int(m.hooks.ActiveConnections()))
	case Reject:
		m.hooks.SetAcceptPaused(true)
	}

	m.cooldownUntil = now.Add(m.cfg.Stages[stage].Cooldown)
}

func (m *Manager) deescalateTo(stage Stage, now time.Time) {
	prev := m.currentStage
	m.currentStage = stage

	m.logger.Info("overload de-escalation", "from", prev.String(), "to", stage.String())

	// Undo the actions of the stage we're leaving.
	switch prev {
	case Expand:
		m.hooks.ShrinkWorkers(m.baseWorkers, 30*time.Second)
	case Reap:
		// No undo needed — reap was a one-time action.
	case Reorder:
		m.hooks.SetSchedulingMode(false)
		if m.freezeHook != nil {
			m.freezeHook(false)
		}
	case Backpressure:
		m.hooks.SetAcceptDelay(0)
		m.hooks.SetMaxConcurrent(0) // 0 = unlimited
	case Reject:
		m.hooks.SetAcceptPaused(false)
	}

	_ = now // suppress unused warning in case we add cooldown tracking later
}
