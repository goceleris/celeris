//go:build linux

package iouring

import (
	"testing"
	"time"
)

// TestStartFDLEngineRidesOutRingENOMEM: with the memlock budget held for 600 ms
// by rings that are not closed yet, an engine start fails with the ring-setup
// ENOMEM (or, when the capability probe's own ring is what failed, "io_uring
// not available on this system"), and a starter of this package must retry it
// until the budget is back instead of judging the first error. Run with
// CELERIS_REQUIRE_IOURING_WORKERS=1, as the CI step does, where skipOrFail656
// turns the unretried error into a failure. startFDLEngine is one of the
// starters that called New and Listen once (celeris#1104).
func TestStartFDLEngineRidesOutRingENOMEM(t *testing.T) {
	if !engineMayUseIOUring() {
		// The test sets CELERIS_REQUIRE_IOURING_WORKERS=1 itself, so a refusal
		// that is not a memlock shortage would fail it instead of skipping.
		t.Skip("the engine does not run io_uring here (a kernel before 5.19, or CELERIS_MAX_IOURING_TIER capping the tier to none)")
	}
	t.Setenv(envRequireIOUring656, "1")
	chargeMemlock(t, 600*time.Millisecond)
	start := time.Now()
	e, addr := startFDLEngine(t, transplantTestHandler{}, nil)
	t.Logf("engine up after %v at %s with %d workers", time.Since(start).Round(10*time.Millisecond), addr, e.NumWorkers())
}
