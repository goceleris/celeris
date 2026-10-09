//go:build linux

package adaptive

import (
	"os"
	"testing"

	"github.com/goceleris/celeris/internal/engine"
)

// otherEngine is the engine type a switch from t lands on: the adaptive engine
// switches between epoll and io_uring only.
func otherEngine(t engine.EngineType) engine.EngineType {
	if t == engine.Epoll {
		return engine.IOUring
	}
	return engine.Epoll
}

// forceSwitchTo runs one forced switch and checks that it LANDED on want,
// before the caller judges anything about the engines (celeris#804).
//
// ForceSwitch returns nothing, and performSwitch gives up quietly when it
// cannot build the lazy io_uring standby (ring setup ENOMEM under
// RLIMIT_MEMLOCK, a seccomp profile, a kernel without the ops): it logs
// "aborting switch", keeps epoll active and leaves e.secondary nil. A test
// that went on from there either dereferenced the nil slot and took the whole
// package's test binary down with a SIGSEGV, or judged placement against an
// io_uring engine that did not exist and reported a false celeris#657
// verdict. Both read as an engine defect, and the engine was right.
//
// A switch to io_uring that did not happen is the ENVIRONMENT, so it skips,
// or fails under CELERIS_REQUIRE_UPSWITCH=1 as the adaptive CI job sets it
// (skipOrFailUpswitch662). Any other switch that did not happen is not an
// environment problem and fails the test at once.
//
// A forced switch always leaves the OTHER engine active, so the caller must
// start from the engine that is not want. A run that starts on io_uring
// (CELERIS_ADAPTIVE_START=iouring, a high-concurrency workload hint) would
// switch away from the engine the test asked for; that is a mismatch between
// the test and its environment, not a missing standby, and it fails here with
// its own message.
func forceSwitchTo(t *testing.T, e *Engine, want engine.EngineType) {
	t.Helper()
	if before := e.ActiveEngine().Type(); before == want {
		t.Fatalf("forceSwitchTo(%v): %v is already active and a forced switch would leave %v; this test "+
			"assumes the run starts on the other engine (CELERIS_ADAPTIVE_START=%q)",
			want, before, otherEngine(want), os.Getenv("CELERIS_ADAPTIVE_START"))
	}
	e.ForceSwitch()
	got := e.ActiveEngine().Type()
	if got == want {
		return
	}
	if want == engine.IOUring {
		skipOrFailUpswitch662(t, "the io_uring standby did not come up here: the forced switch left %v active "+
			"(ring setup failed or io_uring is unusable; the engine aborted the switch and stayed put)", got)
	}
	t.Fatalf("a forced switch to %v left %v active", want, got)
}
