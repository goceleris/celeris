//go:build linux

package adaptive

import (
	"fmt"
	"os"
	"testing"
)

// requireUpSwitch guards the tests whose claim IS the conns-per-worker
// up-switch (celeris#641). adaptive.New turns that switch off when io_uring is
// not viable in the environment -- most often because RLIMIT_MEMLOCK is below
// one worker's rings, which is the GitHub-hosted default of 8 MiB -- and
// production is right to: it will never promote there. A test that needs the
// switch cannot pass in that environment; it used to run to its bound and
// fail, which reads as an engine defect. It now skips and says why.
//
// A skip is not coverage, so the CI job that runs this package raises memlock
// and sets CELERIS_REQUIRE_UPSWITCH=1, which turns the skip into a failure:
// that job cannot go green without exercising the switch.
func requireUpSwitch(t *testing.T, e *Engine) {
	t.Helper()
	if e.ctrl.connSwitchEnabled {
		return
	}
	skipOrFailUpswitch662(t, "the adaptive up-switch is disabled in this environment "+
		"(io_uring not viable; memlock worker ceiling %d, -1 = unlimited)", maxWorkersForMemlock())
}

// skipOrFailUpswitch662 is the one gate of this package's tests that need io_uring (or the up-switch it
// enables) and cannot run where it is unavailable: it skips, and fails instead under
// CELERIS_REQUIRE_UPSWITCH=1, which the CI jobs that raise memlock set. A test must not skip for that reason any
// other way: the job-level SKIP tally (ci.yml, job adaptive) would turn the job red, but only this gate names the
// test and the cause. TestAdaptiveSkipRouting684 fails on a raw t.Skip for it.
func skipOrFailUpswitch662(t *testing.T, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	if os.Getenv("CELERIS_REQUIRE_UPSWITCH") == "1" {
		t.Fatal(msg + " -- CELERIS_REQUIRE_UPSWITCH=1 forbids skipping")
	}
	t.Skip(msg)
}
