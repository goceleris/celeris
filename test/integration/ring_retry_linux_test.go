//go:build linux

package integration

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/engine/iouring"
	"github.com/goceleris/celeris/internal/resource"
)

// An io_uring engine's start can fail with "cannot allocate memory" at ring
// setup while the process's (or another test binary's, run by the same user)
// rings are still charged against RLIMIT_MEMLOCK: the CI runners' 8 MiB holds a
// couple of rings, and the ring of an engine stopped moments ago is returned
// only once its workers have closed. That is the runner, not the code under
// test, so a starter in this package retries it for ringENOMEMWindow and fails
// on any other error at once. This is the twin of test/server's
// ring_enomem_test.go (a helper cannot be shared across test packages)
// (celeris#1104).

// ringENOMEMWindow is how long a start is retried on a ring ENOMEM.
const ringENOMEMWindow = 30 * time.Second

// isRingENOMEM reports a start error that is the ring-setup ENOMEM: the engine's
// "worker N ring setup: io_uring_setup: cannot allocate memory (likely
// RLIMIT_MEMLOCK; ...)" or "all io_uring tiers failed, last error: ...cannot
// allocate memory", or the same pressure one step earlier: the capability
// probe builds a private ring per engine (probe.Probe is not cached) and reads a
// failed ring as no io_uring, so New answers "io_uring not available on this
// system" without the ENOMEM text. The starters that use this start an engine
// the kernel supports, so that error there is the same transient; on a kernel
// without io_uring such a start fails after the window instead of at once.
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

func TestIsRingENOMEM1104(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("worker 0 ring setup: io_uring_setup: cannot allocate memory (likely RLIMIT_MEMLOCK; current=8388608 bytes (soft) / 8388608 bytes (hard))"), true},
		{errors.New("all io_uring tiers failed, last error: io_uring_setup: cannot allocate memory (likely RLIMIT_MEMLOCK)"), true},
		{errors.New("create engine: io_uring not available on this system"), true},
		{errors.New("config validation: bad addr"), false},
		{errors.New("listen tcp 127.0.0.1:1: bind: address already in use"), false},
	} {
		if got := isRingENOMEM(tc.err); got != tc.want {
			t.Errorf("isRingENOMEM(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestRetryRingENOMEM1104(t *testing.T) {
	enomem := errors.New("worker 0 ring setup: io_uring_setup: cannot allocate memory")
	other := errors.New("config validation: bad addr")

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

	calls = 0
	tries, err = retryRingENOMEM(func() error { calls++; return other }, time.Second)
	if !errors.Is(err, other) || tries != 1 || calls != 1 {
		t.Errorf("a non-ENOMEM error: %d tries, err %v; want 1, %v", tries, err, other)
	}

	start := time.Now()
	tries, err = retryRingENOMEM(func() error { return enomem }, 100*time.Millisecond)
	if !errors.Is(err, enomem) || tries < 2 || time.Since(start) < 100*time.Millisecond {
		t.Errorf("a persistent ENOMEM: %d tries after %v, err %v; want >=2 tries over the 100 ms window, then %v", tries, time.Since(start), err, enomem)
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

// A host or environment where the engine refuses io_uring (a tier capped to
// none, a kernel before 5.19) answers "io_uring not available on this system"
// at once and for good; retrying it for the whole window turns a skip into 30 s
// of waiting. Here the refusal is forced with CELERIS_MAX_IOURING_TIER=none,
// where a 4-entry ring still works: only the engine says no.
func TestRetryDoesNotWaitWhenTheEngineRefusesIOUring1104(t *testing.T) {
	t.Setenv("CELERIS_MAX_IOURING_TIER", "none")
	start := time.Now()
	tries, err := retryRingENOMEM(func() error {
		_, err := iouring.New(resource.Config{Addr: "127.0.0.1:0", Protocol: engine.HTTP1, Resources: resource.Resources{Workers: 2}}, &echoHandler{})
		return err
	}, 3*time.Second)
	if err == nil {
		t.Skip("the engine accepted io_uring with the tier capped to none")
	}
	if tries != 1 || time.Since(start) > time.Second {
		t.Errorf("a refusal of io_uring was retried: %d tries over %v (%v); want 1 try at once", tries, time.Since(start).Round(time.Millisecond), err)
	}
}
