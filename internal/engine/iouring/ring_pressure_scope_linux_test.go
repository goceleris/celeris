//go:build linux

package iouring

import (
	"errors"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// A host or environment where the engine refuses io_uring for good (a tier
// capped to none, a kernel before 5.19) answers "io_uring not available on this
// system" at once; retrying it for ringUnchargeBound turns a skip into 10 s of
// waiting per test. io_uring itself works there (a 4-entry ring is created), so
// ioUringUsableHere cannot tell it from the probe ring that met a memlock
// shortage; the refusal is forced here with CELERIS_MAX_IOURING_TIER=none.
func TestRetryDoesNotWaitWhenTheEngineRefusesIOUring1104(t *testing.T) {
	t.Setenv("CELERIS_MAX_IOURING_TIER", "none")
	if memlockSmall, small := memlockBelowRingBudgetFloor(); small {
		t.Skipf("RLIMIT_MEMLOCK is %d bytes: retryRingENOMEM tries once below the floor anyway", memlockSmall)
	}
	start := time.Now()
	_, tries, err := retryRingENOMEM(func() error {
		_, err := New(resource.Config{Addr: "127.0.0.1:0", Protocol: engine.HTTP1, Resources: resource.Resources{Workers: 2}}, transplantTestHandler{})
		return err
	})
	if err == nil {
		t.Skip("the engine accepted io_uring with the tier capped to none")
	}
	if tries != 1 || time.Since(start) > time.Second {
		t.Errorf("a refusal of io_uring was retried: %d tries over %v (%v); want 1 try at once", tries, time.Since(start).Round(time.Millisecond), err)
	}
}

// TestIsRingStartPressure1104: the ring ENOMEM is retried whatever the host; the
// bare "io_uring not available" only where the engine would run io_uring and the
// syscall works; the kernel-before-5.19 refusal (suffixed), a bad config and a
// port in use never.
func TestIsRingStartPressure1104(t *testing.T) {
	enomem := "worker 0 ring setup: io_uring_setup: cannot allocate memory (likely RLIMIT_MEMLOCK; current=8388608 bytes (soft) / 8388608 bytes (hard))"
	tiers := "all io_uring tiers failed, last error: io_uring_setup: cannot allocate memory (likely RLIMIT_MEMLOCK)"
	bare := "io_uring not available on this system"
	old := "io_uring not available on this system: the io_uring engine requires Linux 5.19 or later, and kernel 5.15.0 predates it; use the epoll engine (celeris#682, #872)"
	for _, tc := range []struct {
		name           string
		msg            string
		mayUse, usable bool // engineMayUseIOUring, ioUringUsableHere
		want           bool
	}{
		{"ring ENOMEM", enomem, true, true, true},
		{"ring ENOMEM, no io_uring here", enomem, false, false, true},
		{"all tiers failed on ENOMEM", tiers, true, true, true},
		{"probe ring ENOMEM: bare answer where the engine would run io_uring", bare, true, true, true},
		{"bare answer, tier capped to none or a kernel before 5.19", bare, false, true, false},
		{"bare answer, io_uring syscall blocked", bare, true, false, false},
		{"bare answer, nothing there", bare, false, false, false},
		{"kernel before 5.19 refused by the engine", old, true, true, false},
		{"bad config", "config validation: bad addr", true, true, false},
		{"port in use", "listen tcp 127.0.0.1:1: bind: address already in use", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func(m, u func() bool) { engineMayUseIOUring, ringUsableHere = m, u }(engineMayUseIOUring, ringUsableHere)
			engineMayUseIOUring = func() bool { return tc.mayUse }
			ringUsableHere = func() bool { return tc.usable }
			if got := isRingStartPressure(errors.New("create engine: " + tc.msg)); got != tc.want {
				t.Errorf("isRingStartPressure(%q) = %v, want %v", tc.msg, got, tc.want)
			}
		})
	}
	if isRingStartPressure(nil) {
		t.Error("isRingStartPressure(nil) = true")
	}
}
