//go:build linux

package iouring

import (
	"testing"
	"time"
)

// celeris#1229: a test that fails after it started an engine must stop the
// engine. The helpers below start engines for the 896 and 1173 tests and
// used to leave the stopping to the test body, which a failed assertion
// (t.Fatalf: runtime.Goexit) never reaches. On the Coverage job's 8 MiB
// RLIMIT_MEMLOCK four 896 tests failed on a dial timeout, left four engines
// running, and every later test that installs a package seam (ringWaitSeam,
// cancelAcceptSeam) had those workers read it: answered EBUSY for waits it
// never asked about, and raced with the seam's restore.

// engineAfterItsTestEnded1229 runs start in a subtest that returns at once
// without stopping what it started, which is all a failed assertion does to
// the cleanups of its test (runtime.Goexit runs them, and nothing else of the
// test's body), and returns the run the helper started and
// whether Listen had returned by the time the subtest's cleanups had run. A
// run that is still going is cancelled here, so that this test does not leak
// what it looks for.
func engineAfterItsTestEnded1229(t *testing.T, start func(t *testing.T) *engineRun896) (run *engineRun896, stopped bool) {
	t.Helper()
	t.Run("ended", func(t *testing.T) {
		run = start(t)
	})
	if run == nil {
		t.Skip("the engine did not start here")
	}
	select {
	case err := <-run.done:
		if err != nil {
			t.Errorf("Listen returned %v", err)
		}
		return run, true
	default:
	}
	run.cancel()
	select {
	case <-run.done:
	case <-time.After(10 * time.Second):
		t.Error("the leaked engine did not stop when cancelled")
	}
	return run, false
}

func TestEngineStartHelpersStopTheirEngineWhenTheTestEnds1229(t *testing.T) {
	const leaked = "the engine was still running after the test that started it had ended: " +
		"a failed assertion would leave its workers reading the package's test seams"
	t.Run("start896", func(t *testing.T) {
		if _, stopped := engineAfterItsTestEnded1229(t, func(t *testing.T) *engineRun896 {
			return start896(t, freeAddr639(t), 2)
		}); !stopped {
			t.Error(leaked)
		}
	})
	t.Run("startHold896", func(t *testing.T) {
		if _, stopped := engineAfterItsTestEnded1229(t, func(t *testing.T) *engineRun896 {
			return startHold896(t, freeAddr639(t), 2, 0, newHoldHandler896(false))
		}); !stopped {
			t.Error(leaked)
		}
	})
	// A /hold handler still running at the end is what a failure in the middle
	// of an iteration leaves: Listen waits for it, so the engine's cleanup has
	// to release it before it waits.
	t.Run("startHold896/handler running", func(t *testing.T) {
		var h *holdHandler896
		if _, stopped := engineAfterItsTestEnded1229(t, func(t *testing.T) *engineRun896 {
			h = newHoldHandler896(false)
			addr := freeAddr639(t)
			run := startHold896(t, addr, 2, 0, h)
			c := h2Request896(t, addr, "/hold")
			t.Cleanup(func() { _ = c.Close() })
			select {
			case <-h.started:
			case <-time.After(5 * time.Second):
				t.Fatal("the /hold handler never ran")
			}
			return run
		}); !stopped {
			h.unblock()
			t.Error(leaked)
		}
	})
	t.Run("startGate1173", func(t *testing.T) {
		var g *gate1173
		if _, stopped := engineAfterItsTestEnded1229(t, func(t *testing.T) *engineRun896 {
			g = &gate1173{started: make(chan struct{}, 1), gate: make(chan struct{})}
			run, _ := startGate1173(t, g)
			return run
		}); !stopped {
			t.Error(leaked)
		}
	})
}

// seamRestoredUnderALateReader1229 installs a seam with install in a subtest,
// whose cleanup restores it, while a goroutine that is not ordered with that
// restore goes on reading the seam through read: a worker that an earlier test
// leaked. The restore is a write the race detector must not find unordered
// with the read, so a seam read and written as a plain variable fails this
// test under -race ("race detected during execution of test").
func seamRestoredUnderALateReader1229(t *testing.T, install func(t *testing.T), read func()) {
	t.Helper()
	stop := make(chan struct{})
	finished := make(chan struct{})
	t.Run("seam", func(t *testing.T) {
		install(t)
		go func() {
			defer close(finished)
			for {
				select {
				case <-stop:
					return
				default:
					read()
				}
			}
		}()
	})
	close(stop)
	<-finished
}

func TestRingWaitSeamRestoreIsSafeAgainstALateReader1229(t *testing.T) {
	var w Worker
	seamRestoredUnderALateReader1229(t, func(t *testing.T) {
		setRingWaitSeam884(t, func(*Worker, ringWaitMode, error) error { return nil })
	}, func() { _ = w.ringWaitContinues(waitModeTimed, nil) })
}

func TestCancelAcceptSeamRestoreIsSafeAgainstALateReader1229(t *testing.T) {
	r := newAcceptRig896(t, &baseTier{}, false)
	seamRestoredUnderALateReader1229(t, func(t *testing.T) {
		setCancelAcceptSeam1229(t, func(*Worker, int) {})
	}, func() {
		// stopAccepting reads the seam first; the listen socket is closed by the
		// first call, and a later one finds none and returns.
		r.w.stopAccepting(t.Context())
	})
}

// setCancelAcceptSeam1229 installs cancelAcceptSeam for the test and restores
// it nil when the test ends.
func setCancelAcceptSeam1229(t *testing.T, fn func(*Worker, int)) {
	t.Helper()
	cancelAcceptSeam.Store(&fn)
	t.Cleanup(func() { cancelAcceptSeam.Store(nil) })
}
