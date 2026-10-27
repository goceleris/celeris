//go:build linux

package iouring

import (
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
