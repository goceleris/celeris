//go:build linux

package adaptive

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/probe"
)

// celeris#886 item 11 (from #967 review r1): the controller-driven switch
// tests wait for the controller to promote to io_uring, and judged a switch
// that never happened when the lazy io_uring standby could not be built:
// TestRampH1Sync, TestRampH1Async, TestRampAutoMixedH1H2 and
// TestRampAutoMixedAsync reported "conns not on io_uring after 20s:
// io_uring=0 epoll=2048", and TestAdaptiveSwitchesWithPreBoundListener
// reported "no engine switch after 30s: AdaptiveSwitches=0", each with the
// standby-abort WARN in its log. That reads as an engine defect, and the
// engine was right (the ring setup failed). They now skip, or fail under
// CELERIS_REQUIRE_UPSWITCH=1 (skipIfStandbyBuildFailed886), as the forced-
// switch tests do since celeris#804.
//
// Each cell runs in a CHILD process (this test binary re-executed, as
// idle_follow_switch_standby_804_linux_test.go does) with the standby build
// failing (afterNew886 = failStandbyBuild804), so that one run proves both
// outcomes, a skip and a failure under the require variable, which a
// process-wide env var in-process cannot be.
const (
	env886Cell   = "CELERIS_ADTST_886_CELL"
	test886      = "TestControllerSwitchVerdictsOnAFailedStandby886"
	standbyMsg   = "the io_uring standby did not come up here"
	falseVerdict = "conns not on io_uring"
	// upSwitchOffMsg is requireUpSwitch's message.
	upSwitchOffMsg = "the adaptive up-switch is disabled in this environment"
)

var cells886 = map[string]func(*testing.T){
	"ramp-h1-sync":     TestRampH1Sync,
	"ramp-h1-async":    TestRampH1Async,
	"auto-mixed-h1h2":  TestRampAutoMixedH1H2,
	"auto-mixed-async": TestRampAutoMixedAsync,
	"prebound":         TestAdaptiveSwitchesWithPreBoundListener,
}

// requireCells886 are the cells also run under CELERIS_REQUIRE_UPSWITCH=1 (a
// child costs a ramp: the three cover a ramp scenario, an Auto-protocol ramp
// and the pre-bound switch test).
var requireCells886 = map[string]bool{"ramp-h1-sync": true, "auto-mixed-async": true, "prebound": true}

func TestControllerSwitchVerdictsOnAFailedStandby886(t *testing.T) {
	if cell := os.Getenv(env886Cell); cell != "" {
		// Child: run one cell with the standby build failing. It must end in a
		// skip, or in a failure under CELERIS_REQUIRE_UPSWITCH=1, whose
		// message names the missing standby.
		fn, ok := cells886[cell]
		if !ok {
			t.Fatalf("unknown cell %q", cell)
		}
		afterNew886 = failStandbyBuild804
		fn(t)
		if t.Failed() {
			return // the cell's own failure is the child's result
		}
		t.Fatal("the cell ran to the end with no io_uring standby: it must skip, or fail under CELERIS_REQUIRE_UPSWITCH=1")
	}

	if testing.Short() {
		t.Skip("integration")
	}
	// The child binds a real adaptive engine; without io_uring it would skip
	// for the probe, which says nothing about the standby build.
	if !probe.Probe().IOUringTier.Available() {
		skipOrFailUpswitch662(t, "io_uring unavailable: needs both sub-engines")
	}

	run := func(t *testing.T, cell string, require bool) (out string, exit int) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^"+test886+"$", "-test.v", "-test.count=1", "-test.timeout=120s")
		var env []string
		for _, kv := range os.Environ() {
			if strings.HasPrefix(kv, "CELERIS_REQUIRE_UPSWITCH=") || strings.HasPrefix(kv, env886Cell+"=") {
				continue
			}
			env = append(env, kv)
		}
		env = append(env, env886Cell+"="+cell)
		if require {
			env = append(env, "CELERIS_REQUIRE_UPSWITCH=1")
		}
		cmd.Env = env
		b, err := cmd.CombinedOutput()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exit = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("running the child for %s: %v", cell, err)
		}
		return string(b), exit
	}

	// A host whose RLIMIT_MEMLOCK cannot fund the workers disables the
	// up-switch (requireUpSwitch): every cell then ends there, before it ever
	// reaches the standby, with a message of its own. That is the environment,
	// and this test cannot judge the cell's verdict on it.
	upSwitchOff := func(t *testing.T, out string) {
		t.Helper()
		if strings.Contains(out, upSwitchOffMsg) {
			skipOrFailUpswitch662(t, "the cell stopped at the disabled up-switch (memlock worker ceiling %d, -1 = unlimited), "+
				"before it reached the standby", maxWorkersForMemlock())
		}
	}

	for cell := range cells886 {
		t.Run(cell+"/skips", func(t *testing.T) {
			t0 := time.Now()
			out, exit := run(t, cell, false)
			upSwitchOff(t, out)
			if exit != 0 || !strings.Contains(out, "--- SKIP: "+test886) || !strings.Contains(out, standbyMsg) {
				t.Errorf("want a skip naming the missing standby (%q), child exit %d:\n%s", standbyMsg, exit, out)
			}
			if strings.Contains(out, falseVerdict) || strings.Contains(out, "no engine switch after") || strings.Contains(out, "panic:") {
				t.Errorf("the child judged a switch that never happened (or crashed):\n%s", out)
			}
			t.Logf("celeris886 item 11 %s: standby build failing, child exit=%d skipped=%v in %v", cell, exit,
				strings.Contains(out, "--- SKIP: "+test886), time.Since(t0).Round(time.Second))
		})
		if !requireCells886[cell] {
			continue
		}
		t.Run(cell+"/fails-under-require", func(t *testing.T) {
			out, exit := run(t, cell, true)
			upSwitchOff(t, out)
			if exit == 0 || !strings.Contains(out, "--- FAIL: "+test886) || !strings.Contains(out, "forbids skipping") ||
				!strings.Contains(out, standbyMsg) {
				t.Errorf("want a failure naming the missing standby and CELERIS_REQUIRE_UPSWITCH=1, child exit %d:\n%s", exit, out)
			}
			if strings.Contains(out, falseVerdict) || strings.Contains(out, "no engine switch after") {
				t.Errorf("the child reported the engine verdict instead of the missing standby:\n%s", out)
			}
		})
	}
}

// TestStandbyBuildFailedGuard886: the guard skips only for a standby that was
// asked for and failed. A controller that never asked for it (no failure on
// record) is the engine defect these tests exist to catch, and an engine that
// switched or holds an io_uring engine has nothing to excuse.
func TestStandbyBuildFailedGuard886(t *testing.T) {
	cases := []struct {
		name      string
		failures  int
		switches  uint64
		hasSecond bool
		want      bool
	}{
		{"build failed twice, no io_uring engine, no switch", 2, 0, false, true},
		{"build failed once", 1, 0, false, true},
		{"controller never asked for the standby", 0, 0, false, false},
		{"a switch landed", 1, 1, false, false},
		{"an io_uring engine exists", 1, 0, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, _, _, _ := newLazyAdaptive(t)
			e.ctrl.state.buildFailures = tc.failures
			e.switchesTotal.Store(tc.switches)
			if tc.hasSecond {
				e.secondary = newMockEngine(engine.IOUring)
			}
			got, n := standbyBuildFailed886(e)
			if got != tc.want || (got && n != tc.failures) {
				t.Errorf("standbyBuildFailed886 = %v, %d; want %v, %d", got, n, tc.want, tc.failures)
			}
		})
	}
}
