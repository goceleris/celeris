//go:build linux

package adaptive

import (
	"testing"

	"github.com/goceleris/celeris/resource"
)

// TestAdaptiveScalerConfig_TypedDefaults confirms the typed config path
// in adaptive matches the iouring/epoll built-ins. Adaptive uses ONE
// higher-level scaler (sub-engines have SkipBuiltinScaler=true), so this
// is the only scaler that runs in an adaptive setup.
func TestAdaptiveScalerConfig_TypedDefaults(t *testing.T) {
	t.Parallel()
	c := scalerFromTyped(&resource.WorkerScalingConfig{}, 8)
	if !c.Enabled || !c.StartHigh {
		t.Errorf("default config should produce Enabled+StartHigh, got Enabled=%v StartHigh=%v",
			c.Enabled, c.StartHigh)
	}
	if c.MinActive != 4 {
		t.Errorf("MinActive: expected 4, got %d", c.MinActive)
	}
}

// TestAdaptiveResolveScalerConfig_ConfigBeatsEnv documents the
// precedence rule for adaptive: typed config wins over env vars.
func TestAdaptiveResolveScalerConfig_ConfigBeatsEnv(t *testing.T) {
	t.Setenv("CELERIS_DYN_TARGET", "999")
	rcfg := resource.Config{
		WorkerScaling: &resource.WorkerScalingConfig{TargetConnsPerWorker: 42},
	}
	c := resolveScalerConfig(rcfg, 8)
	if c.TargetConnsPerWorker != 42 {
		t.Errorf("typed config should beat env: got %d, want 42", c.TargetConnsPerWorker)
	}
}
