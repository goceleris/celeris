//go:build linux

package scaler

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/resource"
)

// TestResolve_Defaults locks in the data-validated defaults captured in
// the v1.4.1 spike-B sweep: start-high, min=numCPU/2, target=20,
// interval=250ms, upStep=2, downStep=1, hyst=1, idleTicks=4.
func TestResolve_Defaults(t *testing.T) {
	t.Parallel()
	rcfg := resource.Config{WorkerScaling: &resource.WorkerScalingConfig{}}
	c := Resolve(rcfg, 8)
	if !c.Enabled {
		t.Fatal("typed config (non-nil) must enable the scaler")
	}
	if !c.StartHigh {
		t.Errorf("StartHigh: expected true (zero value Strategy = StartHigh), got false")
	}
	if c.MinActive != 4 {
		t.Errorf("MinActive: expected 4 (numCPU/2 with numCPU=8), got %d", c.MinActive)
	}
	if c.TargetConnsPerWorker != 20 {
		t.Errorf("TargetConnsPerWorker: expected 20, got %d", c.TargetConnsPerWorker)
	}
	if c.Interval != 250*time.Millisecond {
		t.Errorf("Interval: expected 250ms, got %v", c.Interval)
	}
	if c.ScaleUpStep != 2 {
		t.Errorf("ScaleUpStep: expected 2, got %d", c.ScaleUpStep)
	}
	if c.ScaleDownStep != 1 {
		t.Errorf("ScaleDownStep: expected 1, got %d", c.ScaleDownStep)
	}
	if c.ScaleDownHysteresis != 1 {
		t.Errorf("ScaleDownHysteresis: expected 1, got %d", c.ScaleDownHysteresis)
	}
	if c.ScaleDownIdleTicks != 4 {
		t.Errorf("ScaleDownIdleTicks: expected 4, got %d", c.ScaleDownIdleTicks)
	}
}

// TestResolve_StartLow verifies opt-in to start-low.
func TestResolve_StartLow(t *testing.T) {
	t.Parallel()
	rcfg := resource.Config{WorkerScaling: &resource.WorkerScalingConfig{
		Strategy: resource.ScalingStrategyStartLow,
	}}
	c := Resolve(rcfg, 8)
	if c.StartHigh {
		t.Errorf("StartHigh: expected false (Strategy=StartLow), got true")
	}
}

// TestResolve_MinActiveClamping covers the floor + cap on MinActive.
func TestResolve_MinActiveClamping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		minActive  int
		numWorkers int
		want       int
	}{
		{"zero defaults to numCPU/2", 0, 8, 4},
		{"zero with small pool floors to 2", 0, 2, 2},
		{"explicit 1 respected", 1, 8, 1},
		{"above pool clamped to pool", 16, 8, 8},
		{"explicit 3", 3, 8, 3},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := Resolve(resource.Config{WorkerScaling: &resource.WorkerScalingConfig{MinActive: tc.minActive}}, tc.numWorkers)
			if c.MinActive != tc.want {
				t.Errorf("MinActive: got %d, want %d", c.MinActive, tc.want)
			}
		})
	}
}

// TestResolve_ConfigBeatsEnv verifies typed config wins over env vars.
func TestResolve_ConfigBeatsEnv(t *testing.T) {
	t.Setenv("CELERIS_DYN_TARGET", "999")
	rcfg := resource.Config{WorkerScaling: &resource.WorkerScalingConfig{TargetConnsPerWorker: 25}}
	c := Resolve(rcfg, 4)
	if c.TargetConnsPerWorker != 25 {
		t.Errorf("typed config did not take precedence over env: got %d, want 25", c.TargetConnsPerWorker)
	}
	if !c.Enabled {
		t.Error("typed config should produce Enabled=true")
	}
}

// TestResolve_EnvFallback verifies the env-var path is the fallback.
func TestResolve_EnvFallback(t *testing.T) {
	t.Setenv("CELERIS_DYN_WORKERS", "1")
	t.Setenv("CELERIS_DYN_TARGET", "33")
	c := Resolve(resource.Config{}, 4)
	if !c.Enabled {
		t.Fatal("CELERIS_DYN_WORKERS=1 should enable the scaler")
	}
	if c.TargetConnsPerWorker != 33 {
		t.Errorf("env target read incorrectly: got %d, want 33", c.TargetConnsPerWorker)
	}
}

// TestResolve_DisabledByDefault verifies the legacy zero-config path.
func TestResolve_DisabledByDefault(t *testing.T) {
	t.Setenv("CELERIS_DYN_WORKERS", "")
	c := Resolve(resource.Config{}, 4)
	if c.Enabled {
		t.Errorf("scaler should be disabled when neither env nor config provides it")
	}
}

// fakeSource implements Source for unit-testing the algorithm without
// spinning up an engine. PauseWorker / ResumeWorker calls are tracked
// per worker.
type fakeSource struct {
	mu         sync.Mutex
	numWorkers int
	conns      int64
	gen        uint64
	pauseLog   []int
	resumeLog  []int
	paused     []bool
}

func newFake(n int) *fakeSource { return &fakeSource{numWorkers: n, paused: make([]bool, n)} }

func (s *fakeSource) NumWorkers() int { return s.numWorkers }
func (s *fakeSource) ActiveConns() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns
}
func (s *fakeSource) PauseWorker(i int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pauseLog = append(s.pauseLog, i)
	if i >= 0 && i < len(s.paused) {
		s.paused[i] = true
	}
}
func (s *fakeSource) ResumeWorker(i int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resumeLog = append(s.resumeLog, i)
	if i >= 0 && i < len(s.paused) {
		s.paused[i] = false
	}
}
func (s *fakeSource) Generation() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen
}
func (s *fakeSource) Logger() *slog.Logger { return nil }

func (s *fakeSource) setConns(n int64) {
	s.mu.Lock()
	s.conns = n
	s.mu.Unlock()
}
func (s *fakeSource) bumpGen() {
	s.mu.Lock()
	s.gen++
	s.mu.Unlock()
}
func (s *fakeSource) numActive() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, p := range s.paused {
		if !p {
			n++
		}
	}
	return n
}

// TestRun_StartHighScalesDownOnIdle verifies the start-high default
// behaviour: starts at NumWorkers active, scales down to a floor of
// MinActive when load is low. Uses a tight Interval to keep the test fast.
//
// Floor: with ScaleDownHysteresis=0 the floor IS MinActive. With
// hysteresis>0 the floor is MinActive+hysteresis to prevent flapping
// near the boundary; that's tested in
// TestRun_HysteresisFloorPreservedAboveMinActive.
func TestRun_StartHighScalesDownOnIdle(t *testing.T) {
	src := newFake(8)
	src.setConns(0) // idle from the start
	cfg := Config{
		Enabled: true, StartHigh: true,
		MinActive: 2, TargetConnsPerWorker: 20,
		Interval:    5 * time.Millisecond,
		ScaleUpStep: 2, ScaleDownStep: 1,
		ScaleDownHysteresis: 0, ScaleDownIdleTicks: 2,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	Run(ctx, src, cfg)

	if got := src.numActive(); got != 2 {
		t.Errorf("expected scale-down to MinActive=2 with hyst=0, got %d active", got)
	}
}

// TestRun_HysteresisFloorPreservedAboveMinActive verifies that with the
// default hysteresis=1 the algorithm stops scaling at MinActive+1, not
// at MinActive. This prevents flapping near the boundary when conn
// count oscillates ±1 around the threshold.
func TestRun_HysteresisFloorPreservedAboveMinActive(t *testing.T) {
	src := newFake(8)
	src.setConns(0)
	cfg := Config{
		Enabled: true, StartHigh: true,
		MinActive: 2, TargetConnsPerWorker: 20,
		Interval:    5 * time.Millisecond,
		ScaleUpStep: 2, ScaleDownStep: 1,
		ScaleDownHysteresis: 1, ScaleDownIdleTicks: 2,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	Run(ctx, src, cfg)

	// Floor is MinActive + hyst = 3.
	if got := src.numActive(); got != 3 {
		t.Errorf("expected scale-down floor at MinActive+hyst=3, got %d active", got)
	}
}

// TestRun_StartLowScalesUpOnLoad verifies the start-low path: starts at
// MinActive, scales up to ceil(conns/target) when load arrives.
func TestRun_StartLowScalesUpOnLoad(t *testing.T) {
	src := newFake(8)
	cfg := Config{
		Enabled: true, StartHigh: false,
		MinActive: 2, TargetConnsPerWorker: 20,
		Interval:    5 * time.Millisecond,
		ScaleUpStep: 4, ScaleDownStep: 1,
		ScaleDownHysteresis: 1, ScaleDownIdleTicks: 100,
	}
	// Set conns so desired = ceil(160/20) = 8 (full pool).
	src.setConns(160)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	Run(ctx, src, cfg)

	if got := src.numActive(); got != 8 {
		t.Errorf("expected scale-up to NumWorkers=8, got %d active", got)
	}
}

// TestRun_GenerationChangeRebaselines verifies that incrementing
// Generation triggers re-baseline of the active count on the new
// underlying engine. This is the adaptive switch-handling invariant.
func TestRun_GenerationChangeRebaselines(t *testing.T) {
	src := newFake(8)
	src.setConns(60) // desired = 3, but with start-high we start at 8
	cfg := Config{
		Enabled: true, StartHigh: true,
		MinActive: 2, TargetConnsPerWorker: 20,
		Interval:    5 * time.Millisecond,
		ScaleUpStep: 2, ScaleDownStep: 1,
		ScaleDownHysteresis: 1, ScaleDownIdleTicks: 100,
	}
	// Run for a few ticks so the scaler settles. Then bump Generation
	// — the next tick should re-baseline the (still-fake) source by
	// pausing/resuming workers to enforce the current `active`.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	go Run(ctx, src, cfg)
	time.Sleep(15 * time.Millisecond)

	// Reset the logs; the next bump should write a full re-baseline burst.
	src.mu.Lock()
	src.pauseLog = nil
	src.resumeLog = nil
	src.mu.Unlock()
	src.bumpGen()
	time.Sleep(15 * time.Millisecond)
	cancel()
	time.Sleep(10 * time.Millisecond)

	src.mu.Lock()
	pauseLogLen := len(src.pauseLog)
	resumeLogLen := len(src.resumeLog)
	src.mu.Unlock()
	if pauseLogLen+resumeLogLen < src.NumWorkers() {
		t.Errorf("expected re-baseline to issue Pause+Resume calls covering all %d workers; got %d pauses + %d resumes",
			src.NumWorkers(), pauseLogLen, resumeLogLen)
	}
}
