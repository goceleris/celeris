//go:build linux

package iouring

import (
	"testing"
	"time"

	"github.com/goceleris/celeris/resource"
)

// TestScalerFromTypedConfig_Defaults verifies that the zero-value
// WorkerScalingConfig produces the data-validated defaults captured in
// the v1.4.1 spike-B sweep: start-high, min=numCPU/2, target=20,
// interval=250ms, upStep=2, downStep=1, hyst=1, idleTicks=4.
func TestScalerFromTypedConfig_Defaults(t *testing.T) {
	t.Parallel()
	cfg := scalerFromTyped(&resource.WorkerScalingConfig{}, 8)
	if !cfg.Enabled {
		t.Fatal("scalerFromTyped should produce Enabled=true (caller already decided to scale)")
	}
	if !cfg.StartHigh {
		t.Errorf("StartHigh: expected true (default ScalingStrategyStartHigh), got false")
	}
	if cfg.MinActive != 4 {
		t.Errorf("MinActive: expected 4 (numCPU/2 with numCPU=8), got %d", cfg.MinActive)
	}
	if cfg.TargetConnsPerWorker != 20 {
		t.Errorf("TargetConnsPerWorker: expected 20, got %d", cfg.TargetConnsPerWorker)
	}
	if cfg.Interval != 250*time.Millisecond {
		t.Errorf("Interval: expected 250ms, got %v", cfg.Interval)
	}
	if cfg.ScaleUpStep != 2 {
		t.Errorf("ScaleUpStep: expected 2, got %d", cfg.ScaleUpStep)
	}
	if cfg.ScaleDownStep != 1 {
		t.Errorf("ScaleDownStep: expected 1, got %d", cfg.ScaleDownStep)
	}
	if cfg.ScaleDownHysteresis != 1 {
		t.Errorf("ScaleDownHysteresis: expected 1, got %d", cfg.ScaleDownHysteresis)
	}
	if cfg.ScaleDownIdleTicks != 4 {
		t.Errorf("ScaleDownIdleTicks: expected 4, got %d", cfg.ScaleDownIdleTicks)
	}
}

// TestScalerFromTypedConfig_StartLow verifies that
// ScalingStrategyStartLow flips StartHigh false. The user opts into
// start-low; zero-value (StartHigh) is the recommended default.
func TestScalerFromTypedConfig_StartLow(t *testing.T) {
	t.Parallel()
	cfg := scalerFromTyped(&resource.WorkerScalingConfig{
		Strategy: resource.ScalingStrategyStartLow,
	}, 8)
	if cfg.StartHigh {
		t.Errorf("StartHigh: expected false (Strategy=StartLow), got true")
	}
}

// TestScalerFromTypedConfig_MinActiveClamping verifies the floor
// clamps to numWorkers (can't have more active than total) and
// floors at 2 (the spike-B safety lower bound).
func TestScalerFromTypedConfig_MinActiveClamping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		minActive   int
		numWorkers  int
		wantActive  int
	}{
		{"zero defaults to numCPU/2", 0, 8, 4},
		{"zero with small numCPU", 0, 2, 2},        // numCPU/2=1 floored to 2
		{"explicit 1 floored to 1", 1, 8, 1},       // user explicitly chose 1, respect it
		{"above pool clamped", 16, 8, 8},
		{"explicit 3", 3, 8, 3},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := scalerFromTyped(&resource.WorkerScalingConfig{MinActive: tc.minActive}, tc.numWorkers)
			if c.MinActive != tc.wantActive {
				t.Errorf("MinActive: got %d, want %d", c.MinActive, tc.wantActive)
			}
		})
	}
}

// TestResolveScalerConfig_ConfigBeatsEnv verifies the precedence rule
// in resolveScalerConfig: a non-nil cfg.WorkerScaling silences env vars.
func TestResolveScalerConfig_ConfigBeatsEnv(t *testing.T) {
	// Set an env var that, if read, would produce a different value than
	// the typed config. Then verify the typed config wins.
	t.Setenv("CELERIS_DYN_TARGET", "999")
	rcfg := resource.Config{
		WorkerScaling: &resource.WorkerScalingConfig{TargetConnsPerWorker: 25},
	}
	c := resolveScalerConfig(rcfg, 4)
	if c.TargetConnsPerWorker != 25 {
		t.Errorf("typed config did not take precedence over env: got %d, want 25", c.TargetConnsPerWorker)
	}
	if !c.Enabled {
		t.Error("typed config should produce Enabled=true")
	}
}

// TestResolveScalerConfig_EnvFallback verifies the env-var path is the
// fallback when WorkerScaling is nil. Used by the v1.4.1 spike-tuning
// workflow before the typed config landed.
func TestResolveScalerConfig_EnvFallback(t *testing.T) {
	t.Setenv("CELERIS_DYN_WORKERS", "1")
	t.Setenv("CELERIS_DYN_TARGET", "33")
	rcfg := resource.Config{}
	c := resolveScalerConfig(rcfg, 4)
	if !c.Enabled {
		t.Fatal("env CELERIS_DYN_WORKERS=1 should enable the scaler")
	}
	if c.TargetConnsPerWorker != 33 {
		t.Errorf("env target read incorrectly: got %d, want 33", c.TargetConnsPerWorker)
	}
}

// TestResolveScalerConfig_DisabledWhenNoConfig verifies that with no
// typed config and no env var, the scaler stays disabled (legacy behaviour).
func TestResolveScalerConfig_DisabledWhenNoConfig(t *testing.T) {
	// CELERIS_DYN_WORKERS unset → Enabled=false → Engine.Listen never
	// starts the scaler goroutine.
	t.Setenv("CELERIS_DYN_WORKERS", "")
	rcfg := resource.Config{}
	c := resolveScalerConfig(rcfg, 4)
	if c.Enabled {
		t.Errorf("scaler should be disabled when neither env nor config provides it")
	}
}
