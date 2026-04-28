//go:build linux

package epoll

import (
	"testing"

	"github.com/goceleris/celeris/resource"
)

// epoll's scaler.go mirrors iouring's; its core resolveScalerConfig +
// scalerFromTyped logic is the same. The iouring scaler tests cover the
// algorithm; this file exists to lock in the contract that the typed
// config also flows through epoll's path identically (so a future
// refactor that lifts the scaler into a shared package can verify both
// engines stay equivalent).
func TestEpollScalerFromTyped_DefaultsMatchIouring(t *testing.T) {
	t.Parallel()
	c := scalerFromTyped(&resource.WorkerScalingConfig{}, 8)
	if !c.Enabled || !c.StartHigh {
		t.Errorf("expected Enabled=true, StartHigh=true (data-validated default), got Enabled=%v StartHigh=%v",
			c.Enabled, c.StartHigh)
	}
	if c.MinActive != 4 {
		t.Errorf("MinActive: expected 4 (numCPU/2 with numCPU=8), got %d", c.MinActive)
	}
	if c.TargetConnsPerWorker != 20 {
		t.Errorf("TargetConnsPerWorker: expected 20, got %d", c.TargetConnsPerWorker)
	}
}

func TestEpollResolveScalerConfig_ConfigBeatsEnv(t *testing.T) {
	t.Setenv("CELERIS_DYN_TARGET", "999")
	rcfg := resource.Config{
		WorkerScaling: &resource.WorkerScalingConfig{TargetConnsPerWorker: 17},
	}
	c := resolveScalerConfig(rcfg, 4)
	if c.TargetConnsPerWorker != 17 {
		t.Errorf("typed config should beat env: got %d, want 17", c.TargetConnsPerWorker)
	}
}
