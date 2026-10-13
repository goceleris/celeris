//go:build linux

package integration

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/goceleris/celeris/internal/engine/iouring"
	"github.com/goceleris/celeris/internal/probe"
)

const env886 = "CELERIS_ADTST_886_INTEGRATION_CELL"

// TestAdaptiveSwitchVerdictOnAFailedStandby886 runs TestAdaptiveSwitchUnderLoad
// in a child process with the io_uring standby refused (the async-cancel probe
// forced to a rejection, so the lazy build fails): the child must skip, or fail
// under CELERIS_REQUIRE_UPSWITCH=1, and never report "expected engine type to
// change after ForceSwitch" (celeris#886 item 11, the sweep of
// test/integration).
func TestAdaptiveSwitchVerdictOnAFailedStandby886(t *testing.T) {
	if os.Getenv(env886) != "" {
		// Child.
		adaptiveSwitchUnderLoad(t, func(*testing.T) {
			// The start engine is epoll (no CELERIS_ADAPTIVE_START), already
			// built; only the standby's build asks the probe.
			t.Cleanup(iouring.ForceAsyncCancelProbe(iouring.AsyncCancelRejected, "forced by the test"))
		})
		if t.Failed() {
			return
		}
		t.Fatal("the cell ran to the end with no io_uring standby: it must skip, or fail under CELERIS_REQUIRE_UPSWITCH=1")
	}
	if testing.Short() {
		t.Skip("integration")
	}
	if !probe.Probe().IOUringTier.Available() {
		t.Skip("io_uring unavailable: the standby refusal is never reached")
	}
	run := func(require bool) (string, int) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestAdaptiveSwitchVerdictOnAFailedStandby886$", "-test.v", "-test.count=1", "-test.timeout=120s")
		var env []string
		for _, kv := range os.Environ() {
			if strings.HasPrefix(kv, "CELERIS_REQUIRE_UPSWITCH=") || strings.HasPrefix(kv, env886+"=") || strings.HasPrefix(kv, "CELERIS_ADAPTIVE_START=") {
				continue
			}
			env = append(env, kv)
		}
		env = append(env, env886+"=1")
		if require {
			env = append(env, "CELERIS_REQUIRE_UPSWITCH=1")
		}
		cmd.Env = env
		b, err := cmd.CombinedOutput()
		exit := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exit = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("running the child: %v", err)
		}
		return string(b), exit
	}
	const name = "TestAdaptiveSwitchVerdictOnAFailedStandby886"
	t.Run("skips", func(t *testing.T) {
		out, exit := run(false)
		if exit != 0 || !strings.Contains(out, "--- SKIP: "+name) || !strings.Contains(out, "the io_uring standby did not come up here") {
			t.Errorf("want a skip naming the missing standby, child exit %d:\n%s", exit, out)
		}
		if strings.Contains(out, "expected engine type to change") {
			t.Errorf("the child reported a false switch verdict:\n%s", out)
		}
	})
	t.Run("fails-under-require", func(t *testing.T) {
		out, exit := run(true)
		if exit == 0 || !strings.Contains(out, "--- FAIL: "+name) || !strings.Contains(out, "forbids skipping") ||
			strings.Contains(out, "expected engine type to change") {
			t.Errorf("want a failure naming CELERIS_REQUIRE_UPSWITCH=1 and the missing standby, child exit %d:\n%s", exit, out)
		}
	})
}
