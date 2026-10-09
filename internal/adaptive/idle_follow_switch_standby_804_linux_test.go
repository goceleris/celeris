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

// celeris#804: the idle-follow cells must not crash, and must not judge a
// switch that never happened, when the lazy io_uring standby cannot start.
//
// The standby fails to build here the way it does on a host whose io_uring_setup
// returns ENOMEM (RLIMIT_MEMLOCK shared with another container): buildStandby
// returns an error, performSwitch aborts, epoll stays active and e.secondary
// stays nil. Before the fix the revert cells then dereferenced that nil slot
// and the SIGSEGV took the whole package's test binary down; the promote cells
// reported a false celeris#657 PLACEMENT verdict.
//
// Each cell runs in a CHILD process (this test binary re-executed), for the
// reason the crash is the defect: a panic or a Fatal cannot be observed from
// inside the test it kills or fails, and CELERIS_REQUIRE_UPSWITCH=1 must be
// provable both ways from one run, which a process-wide env var in-process
// cannot be.
const (
	env804Cell    = "CELERIS_E8_804_CELL"
	test804       = "TestIdleFollowSwitchStandbyBuildFailure804"
	standbyMsg804 = "the io_uring standby did not come up here"
)

// failStandbyBuild804 makes every lazy standby build fail the way a failed
// io_uring ring setup does.
func failStandbyBuild804(e *Engine) {
	e.mu.Lock()
	e.buildStandby = func() (engine.Engine, error) {
		return nil, errors.New("injected: io_uring_setup: cannot allocate memory (celeris#804)")
	}
	e.mu.Unlock()
}

func TestIdleFollowSwitchStandbyBuildFailure804(t *testing.T) {
	if cell := os.Getenv(env804Cell); cell != "" {
		// Child: run one cell with the standby build failing. It must end in
		// a skip, or in a Fatal under CELERIS_REQUIRE_UPSWITCH=1.
		switch cell {
		case "sync-promote":
			idleFollowSwitchWith(t, respHandler{}, false, false, failStandbyBuild804)
		case "async-promote":
			idleFollowSwitchWith(t, asyncRespHandler{}, true, false, failStandbyBuild804)
		case "sync-revert":
			idleFollowSwitchWith(t, respHandler{}, false, true, failStandbyBuild804)
		case "async-revert":
			idleFollowSwitchWith(t, asyncRespHandler{}, true, true, failStandbyBuild804)
		case "carry-543-promote":
			arm := carry543Arms[0]
			adoptCarriedAsync543(t, arm.name, arm.switches, arm.want, failStandbyBuild804)
		case "carry-543-revert":
			arm := carry543Arms[1]
			adoptCarriedAsync543(t, arm.name, arm.switches, arm.want, failStandbyBuild804)
		case "abort-791-adopted":
			abortAdaptive791(t, true, failStandbyBuild804)
		default:
			t.Fatalf("unknown cell %q", cell)
		}
		// Reaching here means the cell neither skipped nor failed: it went on
		// to judge a switch that did not happen.
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

	run := func(cell string, require bool) (out string, exit int) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^"+test804+"$", "-test.v", "-test.count=1", "-test.timeout=90s")
		var env []string
		for _, kv := range os.Environ() {
			if strings.HasPrefix(kv, "CELERIS_REQUIRE_UPSWITCH=") || strings.HasPrefix(kv, env804Cell+"=") {
				continue
			}
			env = append(env, kv)
		}
		env = append(env, env804Cell+"="+cell)
		if require {
			env = append(env, "CELERIS_REQUIRE_UPSWITCH=1")
		}
		cmd.Env = env
		b, err := cmd.CombinedOutput()
		exit = 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exit = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("running the child for %s: %v", cell, err)
		}
		return string(b), exit
	}

	// Never a crash, never a verdict on a switch that did not happen.
	forbid := func(t *testing.T, cell, out string) {
		t.Helper()
		for _, bad := range []string{"panic:", "SIGSEGV", "PLACEMENT", "LOSS:", "W1:", "W2:", "PREMISE", "AdaptiveSwitches =", "the active engine is"} {
			if strings.Contains(out, bad) {
				t.Errorf("%s: the child's output contains %q, it must neither crash nor judge a switch "+
					"that never happened:\n%s", cell, bad, out)
				return
			}
		}
	}

	t.Run("skips", func(t *testing.T) {
		for _, cell := range []string{"sync-promote", "async-promote", "sync-revert", "async-revert",
			"carry-543-promote", "carry-543-revert", "abort-791-adopted"} {
			t.Run(cell, func(t *testing.T) {
				t0 := time.Now()
				out, exit := run(cell, false)
				if exit != 0 {
					t.Errorf("child exit %d, want 0 (a skip):\n%s", exit, out)
				}
				if !strings.Contains(out, "--- SKIP: "+test804) || !strings.Contains(out, standbyMsg804) {
					t.Errorf("the cell did not skip with the standby message (want %q):\n%s", standbyMsg804, out)
				}
				forbid(t, cell, out)
				t.Logf("%s: exit=%d in %v", cell, exit, time.Since(t0).Round(time.Millisecond))
			})
		}
	})

	t.Run("fails-under-require", func(t *testing.T) {
		for _, cell := range []string{"sync-revert", "async-promote", "carry-543-revert", "abort-791-adopted"} {
			t.Run(cell, func(t *testing.T) {
				out, exit := run(cell, true)
				if exit == 0 {
					t.Errorf("child exit 0 under CELERIS_REQUIRE_UPSWITCH=1: a missing standby must fail the cell:\n%s", out)
				}
				if !strings.Contains(out, "--- FAIL: "+test804) || !strings.Contains(out, "forbids skipping") ||
					!strings.Contains(out, standbyMsg804) {
					t.Errorf("the cell did not fail with the require message and the standby message:\n%s", out)
				}
				forbid(t, cell, out)
			})
		}
	})
}
