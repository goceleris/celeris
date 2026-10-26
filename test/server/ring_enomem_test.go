package celeris_test

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// An io_uring engine's Start can fail with "cannot allocate memory" at ring
// setup while the process's (or another test binary's, run by the same user)
// rings are still charged against RLIMIT_MEMLOCK: the CI runners' 8 MiB is
// enough for a couple of rings, and the ring of an engine stopped moments ago
// is returned only once its workers have closed
// (internal/engine/iouring/ring_budget_linux_test.go). That is the runner, not
// the code under test, so a test helper that starts a server retries it for
// ringENOMEMWindow, as startServerConfig761, startServer421 and
// startKeptServerOn do, and fails on any other error at once (celeris#1104).

// ringENOMEMWindow is how long a start is retried on a ring ENOMEM.
const ringENOMEMWindow = 30 * time.Second

// isRingENOMEM reports a Start error that is the ring-setup ENOMEM: the
// engine's "worker N ring setup: io_uring_setup: cannot allocate memory
// (likely RLIMIT_MEMLOCK; ...)", or the same pressure one step earlier: the
// capability probe, which builds a private ring per engine (probe.Probe is not
// cached) and reads a failed ring as no io_uring, so New answers "io_uring not
// available on this system" without the ENOMEM text. The tests that use this
// start an engine the kernel does support, so an "io_uring not available"
// error here is that transient and is retried; on a kernel without io_uring the
// start fails after the window instead of at once.
func isRingENOMEM(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "cannot allocate memory") || strings.Contains(msg, "io_uring not available on this system")
}

// retryRingENOMEM calls attempt until it returns nil or an error that is not a
// ring ENOMEM, or until window has passed since the first call. It returns the
// number of calls and the last error. attempt must release whatever a failed
// try holds (stop the server, close the listener) before it returns.
func retryRingENOMEM(attempt func() error, window time.Duration) (tries int, err error) {
	deadline := time.Now().Add(window)
	for {
		tries++
		if err = attempt(); err == nil || !isRingENOMEM(err) || !time.Now().Before(deadline) {
			return tries, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// startRetryingRingENOMEM runs attempt with retryRingENOMEM and fails t with
// what if it still fails.
func startRetryingRingENOMEM(t testing.TB, what string, attempt func() error) {
	t.Helper()
	tries, err := retryRingENOMEM(attempt, ringENOMEMWindow)
	if err != nil {
		t.Fatalf("%s: %v (try %d)", what, err, tries)
	}
	if tries > 1 {
		t.Logf("%s: retried on ring ENOMEM: %d tries", what, tries)
	}
}

func TestIsRingENOMEM1104(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("worker 0 ring setup: io_uring_setup: cannot allocate memory (likely RLIMIT_MEMLOCK; current=8388608 bytes (soft) / 8388608 bytes (hard); raise via `ulimit -l unlimited`)"), true},
		{errors.New("create engine: io_uring not available on this system"), true}, // the probe's ring hit the same ENOMEM
		{errors.New("create engine: config validation: bad addr"), false},
		{errors.New("listen tcp 127.0.0.1:1: bind: address already in use"), false},
	} {
		if got := isRingENOMEM(tc.err); got != tc.want {
			t.Errorf("isRingENOMEM(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestRetryRingENOMEM1104(t *testing.T) {
	enomem := errors.New("worker 0 ring setup: io_uring_setup: cannot allocate memory")
	other := errors.New("create engine: config validation: bad addr")

	// A ring ENOMEM is retried until the start succeeds.
	calls := 0
	tries, err := retryRingENOMEM(func() error {
		calls++
		if calls < 3 {
			return enomem
		}
		return nil
	}, time.Second)
	if err != nil || tries != 3 {
		t.Errorf("two ENOMEMs then success: %d tries, err %v; want 3, nil", tries, err)
	}

	// Any other error is returned at once.
	calls = 0
	tries, err = retryRingENOMEM(func() error { calls++; return other }, time.Second)
	if !errors.Is(err, other) || tries != 1 || calls != 1 {
		t.Errorf("a non-ENOMEM error: %d tries, err %v; want 1, %v", tries, err, other)
	}

	// A persistent ENOMEM is retried for the window, then returned.
	start := time.Now()
	tries, err = retryRingENOMEM(func() error { return enomem }, 100*time.Millisecond)
	if !errors.Is(err, enomem) || tries < 2 || time.Since(start) < 100*time.Millisecond {
		t.Errorf("a persistent ENOMEM: %d tries after %v, err %v; want >=2 tries over the 100 ms window, then %v", tries, time.Since(start), err, enomem)
	}
}
