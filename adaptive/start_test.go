//go:build linux

package adaptive

import (
	"testing"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/resource"
)

// viableProfile is a modern kernel with the full io_uring fast tier.
func viableProfile() engine.CapabilityProfile {
	return engine.CapabilityProfile{
		KernelMajor: 6, KernelMinor: 12,
		DeferTaskrun: true, SingleIssuer: true, MultishotRecv: true, ProvidedBuffers: true,
	}
}

// oldProfile is a pre-fast-tier kernel (io_uring not worth running).
func oldProfile() engine.CapabilityProfile {
	return engine.CapabilityProfile{KernelMajor: 5, KernelMinor: 15}
}

// withMemlock overrides the memlock probe for the duration of a test.
func withMemlock(t *testing.T, maxWorkers int) {
	t.Helper()
	prev := maxWorkersForMemlock
	maxWorkersForMemlock = func() int { return maxWorkers }
	t.Cleanup(func() { maxWorkersForMemlock = prev })
}

// TestChooseStartEngine_GateOrder asserts the new start-engine policy: the
// DEFAULT is epoll (the flipped default), io_uring only on an explicit
// high-concurrency hint with a viable kernel + memlock + non-h2c protocol.
func TestChooseStartEngine_GateOrder(t *testing.T) {
	cfg := func(proto engine.Protocol, hint resource.WorkloadHint) resource.Config {
		return resource.Config{Protocol: proto, Resources: resource.Resources{WorkloadHint: hint}}
	}
	tests := []struct {
		name    string
		profile engine.CapabilityProfile
		cfg     resource.Config
		memlock int // -1 = no cap
		want    engine.EngineType
	}{
		{"default-is-epoll (viable, no hint)", viableProfile(), cfg(engine.HTTP1, resource.WorkloadUnspecified), -1, engine.Epoll},
		{"low-conc hint -> epoll", viableProfile(), cfg(engine.HTTP1, resource.WorkloadLowConcurrency), -1, engine.Epoll},
		{"high-conc hint + viable -> iouring", viableProfile(), cfg(engine.HTTP1, resource.WorkloadHighConcurrency), -1, engine.IOUring},
		{"high-conc hint but old kernel -> epoll", oldProfile(), cfg(engine.HTTP1, resource.WorkloadHighConcurrency), -1, engine.Epoll},
		{"high-conc hint but h2c -> epoll", viableProfile(), cfg(engine.H2C, resource.WorkloadHighConcurrency), -1, engine.Epoll},
		{"high-conc hint but memlock-starved -> epoll", viableProfile(), cfg(engine.HTTP1, resource.WorkloadHighConcurrency), 1, engine.Epoll},
		{"auto protocol + high-conc hint -> iouring", viableProfile(), cfg(engine.Auto, resource.WorkloadHighConcurrency), -1, engine.IOUring},
	}
	t.Setenv("CELERIS_ADAPTIVE_START", "") // neutralize any ambient override
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withMemlock(t, tc.memlock)
			if got := chooseStartEngine(tc.profile, tc.cfg); got != tc.want {
				t.Fatalf("chooseStartEngine = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestChooseStartEngine_EnvOverride asserts the env escape hatch wins over policy.
func TestChooseStartEngine_EnvOverride(t *testing.T) {
	withMemlock(t, -1)
	t.Setenv("CELERIS_ADAPTIVE_START", "iouring")
	if got := chooseStartEngine(oldProfile(), resource.Config{Protocol: engine.H2C}); got != engine.IOUring {
		t.Fatalf("env=iouring should force IOUring even on old/h2c, got %v", got)
	}
	t.Setenv("CELERIS_ADAPTIVE_START", "epoll")
	if got := chooseStartEngine(viableProfile(), resource.Config{Resources: resource.Resources{WorkloadHint: resource.WorkloadHighConcurrency}}); got != engine.Epoll {
		t.Fatalf("env=epoll should force Epoll even with high-conc hint, got %v", got)
	}
}

// TestIOUringViable covers the two t0 disqualifiers.
func TestIOUringViable(t *testing.T) {
	withMemlock(t, -1)
	if !ioUringViable(viableProfile(), resource.Config{}) {
		t.Fatal("viable profile + unlimited memlock should be viable")
	}
	if ioUringViable(oldProfile(), resource.Config{}) {
		t.Fatal("old kernel should be non-viable")
	}
	withMemlock(t, 1) // 1 worker < resolved workers -> non-viable
	if ioUringViable(viableProfile(), resource.Config{}) {
		t.Fatal("memlock-starved should be non-viable")
	}
}
