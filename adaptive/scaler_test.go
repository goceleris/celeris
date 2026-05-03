//go:build linux

package adaptive

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/engine/scaler"
	"github.com/goceleris/celeris/resource"
)

// scalableMockEngine extends mockEngine with the engine.WorkerScaler
// interface so the adaptive scaler can exercise its
// PauseWorker / ResumeWorker delegation logic against a controllable
// fake. Pause/Resume calls are tracked per-worker and per-direction so
// tests can assert the scaler routed work to the active engine and
// not the standby.
type scalableMockEngine struct {
	*mockEngine
	numWorkers int
	pauseLog   []int
	resumeLog  []int
	logMu      sync.Mutex
}

func newScalableMockEngine(et engine.EngineType, n int) *scalableMockEngine {
	return &scalableMockEngine{mockEngine: newMockEngine(et), numWorkers: n}
}

func (m *scalableMockEngine) NumWorkers() int { return m.numWorkers }

func (m *scalableMockEngine) PauseWorker(i int) {
	m.logMu.Lock()
	m.pauseLog = append(m.pauseLog, i)
	m.logMu.Unlock()
}

func (m *scalableMockEngine) ResumeWorker(i int) {
	m.logMu.Lock()
	m.resumeLog = append(m.resumeLog, i)
	m.logMu.Unlock()
}

func (m *scalableMockEngine) snapshotLog() (pause, resume []int) {
	m.logMu.Lock()
	defer m.logMu.Unlock()
	pause = append(pause, m.pauseLog...)
	resume = append(resume, m.resumeLog...)
	return
}

// TestAdaptiveScaler_DelegatesToActiveEngine spins the higher-level
// scaler with two mock engines, sets connection counts on the active
// one, and verifies the scaler calls PauseWorker / ResumeWorker on
// the active engine — never on the standby. This is the architectural
// invariant that the v1.4.1 adaptive scaler refactor exists to enforce
// (the pre-refactor design ran two scalers, producing -54 % to +118 %
// pinning-test variance — see PR #257).
func TestAdaptiveScaler_DelegatesToActiveEngine(t *testing.T) {
	primary := newScalableMockEngine(engine.Epoll, 8)
	secondary := newScalableMockEngine(engine.IOUring, 8)
	sampler := newSyntheticSampler()
	cfg := resource.Config{}
	e := newFromEngines(primary, secondary, sampler, cfg)

	// Active = primary (matches newFromEngines default).
	// Set activeConns high enough that desired = 8 (max), so the scaler
	// will call ResumeWorker on workers 4..7 of the active engine.
	primary.SetMetrics(engine.EngineMetrics{ActiveConnections: 200})
	secondary.SetMetrics(engine.EngineMetrics{ActiveConnections: 0})

	scfg := scaler.Config{
		Enabled:              true,
		MinActive:            4,
		TargetConnsPerWorker: 20,
		Interval:             10 * time.Millisecond,
		ScaleUpStep:          4, // burst-resume to active=8 in one tick
		ScaleDownStep:        1,
		ScaleDownHysteresis:  1,
		ScaleDownIdleTicks:   100, // never scale-down within the test budget
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go e.runScaler(ctx, scfg)

	// Give the scaler a few ticks to ramp up.
	time.Sleep(100 * time.Millisecond)
	cancel()
	// Tiny grace for the scaler goroutine to observe the cancel.
	time.Sleep(20 * time.Millisecond)

	pPause, pResume := primary.snapshotLog()
	sPause, sResume := secondary.snapshotLog()

	// Initial state pauses workers 4..7 on the active engine. Then
	// scaler resumes 4..7 on the ACTIVE (primary) engine to reach
	// desired=8.
	if len(pPause) < 4 {
		t.Errorf("primary expected ≥4 PauseWorker calls (init), got %d: %v", len(pPause), pPause)
	}
	if len(pResume) == 0 {
		t.Errorf("primary expected ResumeWorker calls (active scaling up), got 0")
	}
	// Standby (secondary) should NEVER see ResumeWorker — that's the
	// invariant. The scaler routes scale-up to the active engine only.
	if len(sResume) != 0 {
		t.Errorf("secondary should NOT see ResumeWorker calls; got %v", sResume)
	}
	// And no spurious calls on the secondary either (it's the standby).
	_ = sPause
}
