//go:build linux

package adaptive

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/probe"
	"github.com/goceleris/celeris/resource"
)

// viableProfile is a modern kernel with the full io_uring fast tier.
func viableProfile() engine.CapabilityProfile {
	return engine.CapabilityProfile{
		KernelMajor: 6, KernelMinor: 12, IOUringTier: engine.Optional,
		DeferTaskrun: true, SingleIssuer: true, MultishotRecv: true, ProvidedBuffers: true,
	}
}

// oldProfile is a pre-fast-tier kernel (io_uring not worth running). Its tier
// is the one 5.15 probes as (Base), so it is the kernel/feature test that
// rejects it, not the availability test.
func oldProfile() engine.CapabilityProfile {
	return engine.CapabilityProfile{KernelMajor: 5, KernelMinor: 15, IOUringTier: engine.Base}
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

// TestIOUringViable_TierUnavailable pins celeris#679 item 4. The profile is
// what probe.Probe returns on a 6.12 kernel under CELERIS_MAX_IOURING_TIER=none
// (probe.capIOUringTier with maxTier None clears IOUringTier and every feature
// flag, and leaves KernelMajor/KernelMinor alone), which is also what it
// returns when io_uring_setup is refused there (a default seccomp profile,
// kernel.io_uring_disabled). iouring.New refuses to build an engine on that
// profile, so io_uring is not viable; the kernel-version branch alone used to
// say it was.
func TestIOUringViable_TierUnavailable(t *testing.T) {
	withMemlock(t, -1)
	capped := engine.CapabilityProfile{KernelMajor: 6, KernelMinor: 12, IOUringTier: engine.None}
	if ioUringViable(capped, resource.Config{}) {
		t.Error("io_uring tier None on a 6.12 kernel is viable: every promotion would fail to build the engine")
	}
	// Even a profile that (impossibly) kept the fast-tier flags is not viable
	// without an available tier: availability is checked on its own.
	flagsButNone := viableProfile()
	flagsButNone.IOUringTier = engine.None
	if ioUringViable(flagsButNone, resource.Config{}) {
		t.Error("io_uring tier None with fast-tier flags is viable")
	}
	// Control: capping at base leaves a buildable engine, and on a 6.12
	// kernel the bundles-era branch still makes it viable. The availability
	// test must not reject it.
	base := engine.CapabilityProfile{KernelMajor: 6, KernelMinor: 12, IOUringTier: engine.Base, LinkedSQEs: true}
	if !ioUringViable(base, resource.Config{}) {
		t.Error("io_uring tier Base on a 6.12 kernel is not viable, but iouring.New builds it")
	}
}

// TestNew_TierCapNoneNeverPromotes is the same defect through the production
// path: probe.Probe with the real CELERIS_MAX_IOURING_TIER=none, then New. It
// discriminates only on a 6.10+ kernel, where the unfixed code called io_uring
// viable; below that the capped profile fails the fast-tier test anyway and
// the test passes on either code (it says which, so a green run on an old
// kernel is not read as proof). GitHub's ubuntu runners are on 6.17.
func TestNew_TierCapNoneNeverPromotes(t *testing.T) {
	withMemlock(t, -1)
	t.Setenv("CELERIS_ADAPTIVE_START", "")
	t.Setenv("CELERIS_MAX_IOURING_TIER", "none")
	p := probe.Probe()
	if p.IOUringTier != engine.None {
		t.Fatalf("precondition: CELERIS_MAX_IOURING_TIER=none must cap the probed tier to none, got %v", p.IOUringTier)
	}
	discriminating := p.KernelMajor > 6 || (p.KernelMajor == 6 && p.KernelMinor >= 10)
	t.Logf("kernel %s: discriminating=%v", p.KernelVersion, discriminating)

	for _, tc := range []struct {
		name string
		hint resource.WorkloadHint
	}{
		{"no-hint", resource.WorkloadUnspecified},
		{"high-concurrency-hint", resource.WorkloadHighConcurrency},
	} {
		hint := tc.hint
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			cfg := resource.Config{
				Addr:      "127.0.0.1:0",
				Protocol:  engine.HTTP1,
				Resources: resource.Resources{WorkloadHint: hint},
				Logger:    slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})),
			}
			e, err := New(cfg, noopHandler{}, nil)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			// Release what New built (its start engine) the way a server does.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_ = e.Listen(ctx)

			if e.startType != engine.Epoll {
				t.Errorf("start engine = %v, want epoll: io_uring is capped to none", e.startType)
			}
			if e.ctrl.connSwitchEnabled {
				t.Error("the conns-per-worker UP switch is enabled with io_uring capped to none: every promotion builds an io_uring engine that iouring.New refuses, then backs off with a WARN")
			}
			if logs.Len() > 0 {
				t.Errorf("New logged at WARN or above with io_uring capped to none:\n%s", logs.String())
			}
		})
	}
}
