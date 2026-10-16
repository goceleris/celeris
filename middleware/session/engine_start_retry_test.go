package session

import (
	"strings"
	"testing"
	"time"
)

// startLoginServer shares one rule for a Start that fails with the start
// helpers of the other packages that boot io_uring engines (see
// middleware/sse's startSSEServer). The io_uring ring memory is charged to
// RLIMIT_MEMLOCK per user, so a start made while another test binary holds
// rings fails although nothing is wrong: either io_uring_setup answers ENOMEM,
// or the engine's own probe does and the engine reports "io_uring not
// available". Both are retried with a new server and listener for
// startRetryWindow when io_uring exists on this host (ioUringExists). After the
// window the test fails; it skips only where io_uring does not exist at all.
const startRetryWindow = 10 * time.Second

// startFailure is what startLoginServer does with a Start that failed.
type startFailure int

const (
	startRetry startFailure = iota // a new server and listener, after a pause
	startFail                      // a real failure: fail the test
	startSkip                      // io_uring does not work on this host: skip
)

// classifyStart decides what a failed Start means. msg is the Start error.
// exists is consulted only for an io_uring failure that is not ENOMEM, and
// reports whether io_uring exists on this host (works, or is only short of
// memory). retryLeft says whether the retry window is still open.
func classifyStart(msg string, exists func() bool, retryLeft bool) startFailure {
	if strings.Contains(msg, "cannot allocate memory") {
		// io_uring_setup answered ENOMEM, so io_uring exists on this host; the
		// RLIMIT_MEMLOCK charge of other rings is what is failing. The probe
		// would fail with the same ENOMEM and report "no io_uring", so it is
		// not consulted: inside the window the start is retried, after it the
		// test fails (it must not skip where io_uring works).
		if retryLeft {
			return startRetry
		}
		return startFail
	}
	if strings.Contains(msg, "io_uring") || strings.Contains(msg, "not available") {
		// The engine's own probe can fail with that ENOMEM and then reports
		// "io_uring not available on this system", without the word ENOMEM
		// (celeris#888 item 31, seen at 4 MiB of memlock). So the answer is
		// the test's own look at the host: io_uring does not exist (skip), or it
		// does and the start failed (retry inside the window, fail after it).
		if !exists() {
			return startSkip
		}
		if retryLeft {
			return startRetry
		}
		return startFail
	}
	return startFail
}

// TestClassifyStartTable pins classifyStart's decision for each kind of Start
// failure. exists is consulted only for a failure that names io_uring and is
// not ENOMEM; ENOMEM is retried inside the window and fails after it.
func TestClassifyStartTable(t *testing.T) {
	yes := func() bool { return true }
	no := func() bool { return false }
	never := func() bool {
		t.Fatal("exists consulted for an ENOMEM or for a failure that does not name io_uring")
		return false
	}
	cases := []struct {
		name      string
		msg       string
		exists    func() bool
		retryLeft bool
		want      startFailure
	}{
		{"ENOMEM inside the window is retried", "worker 0 ring setup: io_uring_setup: cannot allocate memory (likely RLIMIT_MEMLOCK)", never, true, startRetry},
		// The probe's own ring fails with the same ENOMEM under sustained
		// pressure, so "no io_uring" from it is not evidence: after the window the
		// message itself proves io_uring exists, and the test fails whatever the
		// probe says.
		{"ENOMEM after the window fails, probe not consulted", "io_uring_setup: cannot allocate memory", never, false, startFail},
		{"ENOMEM after the window fails even if the probe says no", "worker 0 ring setup: io_uring_setup: cannot allocate memory", no, false, startFail},
		{"io_uring unavailable where it does not exist skips", "engine unavailable: io_uring not available", no, true, startSkip},
		{"io_uring unavailable where it does not exist skips after the window too", "engine unavailable: io_uring not available", no, false, startSkip},
		// The engine's probe can fail with the ENOMEM and report this message;
		// where io_uring exists the start is retried, and fails after the window.
		{"io_uring unavailable where it exists is retried", "create engine: io_uring not available on this system", yes, true, startRetry},
		{"io_uring unavailable where it exists fails after the window", "create engine: io_uring not available on this system", yes, false, startFail},
		{"not available without io_uring skips where io_uring does not exist", "feature not available", no, true, startSkip},
		{"a start error that is not about io_uring fails", "listen tcp: address already in use", never, true, startFail},
		{"a memory error without io_uring is retried in the window", "mmap: cannot allocate memory", never, true, startRetry},
		{"a memory error without io_uring fails after the window", "mmap: cannot allocate memory", never, false, startFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyStart(tc.msg, tc.exists, tc.retryLeft); got != tc.want {
				t.Fatalf("classifyStart(%q, retryLeft=%t) = %d, want %d", tc.msg, tc.retryLeft, got, tc.want)
			}
		})
	}
}
