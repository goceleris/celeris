//go:build linux

package sse_test

import (
	"strings"
	"testing"
	"time"
)

// The start helpers of this package (startSSEServer and TestSSEOnNativeEngines)
// share one rule for a Start that fails. The io_uring ring memory is charged to
// RLIMIT_MEMLOCK per user, so a start made while another test binary holds
// rings fails with ENOMEM although nothing is wrong. That failure is retried
// with a new server and listener for startRetryWindow, as startSSE732 does.
// After the window an ENOMEM fails the test: io_uring_setup's own answer proves
// io_uring exists here, and the probe would fail with the same ENOMEM and call
// it absent. A failure that names io_uring without ENOMEM fails the test when
// the probe finds io_uring usable, and skips it only when it does not.
const startRetryWindow = 10 * time.Second

// startFailure is what a helper does with a Start that failed.
type startFailure int

const (
	startRetry startFailure = iota // a new server and listener, after a pause
	startFail                      // a real failure: fail the test
	startSkip                      // io_uring does not work on this runner: skip
)

// classifyStart decides what a failed Start means. msg is the Start error.
// usable is consulted only for an io_uring failure that is not ENOMEM, and
// reports whether io_uring works on this host. retryLeft says whether the retry
// window is still open.
func classifyStart(msg string, usable func() bool, retryLeft bool) startFailure {
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
		if usable() {
			return startFail
		}
		return startSkip
	}
	return startFail
}

// ioUringUsable reports whether io_uring works on this host: the probe's tier
// is High or better and provided buffers are present.
func ioUringUsable(tb testing.TB) bool {
	usable, _ := c714ProbeIOUring(tb)
	return usable
}

// TestClassifyStartTable pins classifyStart's decision for each kind of Start
// failure. The usable callback is called only for an io_uring failure that is
// not ENOMEM; ENOMEM is retried inside the window and fails after it.
func TestClassifyStartTable(t *testing.T) {
	yes := func() bool { return true }
	no := func() bool { return false }
	never := func() bool {
		t.Fatal("usable consulted for an ENOMEM or for a failure that does not name io_uring")
		return false
	}
	cases := []struct {
		name      string
		msg       string
		usable    func() bool
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
		{"ENOMEM after the window fails where io_uring works", "io_uring_setup: cannot allocate memory", yes, false, startFail},
		{"io_uring unavailable where it does not work skips", "engine unavailable: io_uring not available", no, true, startSkip},
		{"io_uring unavailable where it works fails", "engine unavailable: io_uring not available", yes, true, startFail},
		{"not available without io_uring skips where unusable", "feature not available", no, true, startSkip},
		{"a start error that is not about io_uring fails", "listen tcp: address already in use", never, true, startFail},
		{"a memory error without io_uring is retried in the window", "mmap: cannot allocate memory", never, true, startRetry},
		{"a memory error without io_uring fails after the window", "mmap: cannot allocate memory", never, false, startFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyStart(tc.msg, tc.usable, tc.retryLeft); got != tc.want {
				t.Fatalf("classifyStart(%q, retryLeft=%t) = %d, want %d", tc.msg, tc.retryLeft, got, tc.want)
			}
		})
	}
}
