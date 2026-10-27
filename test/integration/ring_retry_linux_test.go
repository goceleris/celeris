//go:build linux

package integration

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/engine/iouring"
	"github.com/goceleris/celeris/internal/probe"
	"github.com/goceleris/celeris/internal/resource"
)

// An io_uring engine's start can fail with "cannot allocate memory" at ring
// setup while the process's (or another test binary's, run by the same user)
// rings are still charged against RLIMIT_MEMLOCK: the CI runners' 8 MiB holds a
// couple of rings, and the ring of an engine stopped moments ago is returned
// only once its workers have closed. That is the runner, not the code under
// test, so a starter in this package retries it for ringENOMEMWindow and fails
// on any other error at once. This is the twin of test/server's
// ring_enomem_test.go (a helper cannot be shared across test packages). The
// flake is part of the lane that celeris#1104 tracks; that issue's own report
// is a different test.

// ringENOMEMWindow is how long a start is retried on a ring ENOMEM.
const ringENOMEMWindow = 30 * time.Second

// isRingENOMEM reports a start error that is the ring-setup ENOMEM: the engine's
// "worker N ring setup: io_uring_setup: cannot allocate memory (likely
// RLIMIT_MEMLOCK; ...)" or "all io_uring tiers failed, last error: ...cannot
// allocate memory", or the same pressure one step earlier: the capability
// probe builds a private ring per engine (probe.Probe is not cached) and reads a
// failed ring as no io_uring, so New answers "io_uring not available on this
// system" without the ENOMEM text. That second form is retried only where the
// engine would run io_uring on this host (ioUringRunnableHere): on a host that
// has none, or where the engine refuses it for good, the answer is final and
// the caller skips or fails at once, as it did before the retry existed.
func isRingENOMEM(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "cannot allocate memory") {
		return true
	}
	// The bare answer only: New's refusal of a kernel before 5.19 carries a
	// suffix ("...: the io_uring engine requires Linux 5.19 or later ...").
	return strings.HasSuffix(msg, "io_uring not available on this system") && ioUringRunnableHere()
}

// engineMayUseIOUring reports whether the engine would run io_uring on this host
// were no ring refused for want of memory: a kernel of 5.19 or later (the
// engine refuses io_uring below it, for the async-cancel flags every connection
// close uses) and no CELERIS_MAX_IOURING_TIER that caps the tier to none
// (probe.Probe reads a value that is not optional, high or base as none). It
// looks at no ring, so memlock pressure cannot make it answer false. It is a
// variable so a test can stand in for another host.
var engineMayUseIOUring = func() bool {
	switch os.Getenv("CELERIS_MAX_IOURING_TIER") {
	case "", "optional", "high", "base":
	default:
		return false
	}
	var u unix.Utsname
	if unix.Uname(&u) != nil {
		return false
	}
	kv, err := probe.ParseKernelVersion(unix.ByteSliceToString(u.Release[:]))
	return err == nil && kv.AtLeast(5, 19)
}

// ioUringRunnableHere reports whether io_uring is there for the engine to use:
// engineMayUseIOUring, and the io_uring syscall works (a 4-entry ring is
// created, or refused for want of memory, which is the pressure being waited
// out). False for a host whose seccomp profile or kernel has none.
func ioUringRunnableHere() bool {
	return engineMayUseIOUring() && ioUringSyscallPresent()
}

// ioUringSyscallPresent reports that the io_uring syscall works here: a 4-entry
// ring is created, or refused for want of memory. A variable for tests, as is
// engineMayUseIOUring.
var ioUringSyscallPresent = func() bool {
	r, err := iouring.NewRing(4, 0, 0)
	if err == nil {
		_ = r.Close()
		return true
	}
	return strings.Contains(err.Error(), "cannot allocate memory")
}

// skipUnlessIOUringRunnable skips the calling test, or fails it where the CI
// convention says io_uring is required (CELERIS_REQUIRE_IOURING_WORKERS=1, the
// same variable the iouring package's skipOrFail656 reads), when io_uring is
// not there for the engine to use.
func skipUnlessIOUringRunnable(t *testing.T) {
	t.Helper()
	if ioUringRunnableHere() {
		return
	}
	if os.Getenv("CELERIS_REQUIRE_IOURING_WORKERS") == "1" {
		t.Fatal("io_uring is not usable here, and CELERIS_REQUIRE_IOURING_WORKERS=1 forbids skipping")
	}
	t.Skip("io_uring is not usable here (kernel before 5.19, a capped tier or a blocked syscall)")
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
	enomem := "worker 0 ring setup: io_uring_setup: cannot allocate memory (likely RLIMIT_MEMLOCK; current=8388608 bytes (soft) / 8388608 bytes (hard))"
	tiers := "all io_uring tiers failed, last error: io_uring_setup: cannot allocate memory (likely RLIMIT_MEMLOCK)"
	bare := "create engine: io_uring not available on this system"
	old := "create engine: io_uring not available on this system: the io_uring engine requires Linux 5.19 or later, and kernel 5.15.0 predates it"
	for _, tc := range []struct {
		name            string
		msg             string
		mayUse, syscall bool // engineMayUseIOUring, ioUringSyscallPresent
		want            bool
	}{
		{"ring ENOMEM", enomem, true, true, true},
		{"ring ENOMEM, no io_uring here", enomem, false, false, true}, // the text says it is memory
		{"all tiers failed on ENOMEM", tiers, true, true, true},
		{"probe ring ENOMEM: bare answer where the engine would run io_uring", bare, true, true, true},
		{"bare answer, tier capped to none or a kernel before 5.19", bare, false, true, false},
		{"bare answer, io_uring syscall blocked", bare, true, false, false},
		{"bare answer, nothing there", bare, false, false, false},
		{"kernel before 5.19 refused by the engine", old, true, true, false},
		{"bad config", "config validation: bad addr", true, true, false},
		{"port in use", "listen tcp 127.0.0.1:1: bind: address already in use", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func(m, s func() bool) { engineMayUseIOUring, ioUringSyscallPresent = m, s }(engineMayUseIOUring, ioUringSyscallPresent)
			engineMayUseIOUring = func() bool { return tc.mayUse }
			ioUringSyscallPresent = func() bool { return tc.syscall }
			if got := isRingENOMEM(errors.New(tc.msg)); got != tc.want {
				t.Errorf("isRingENOMEM(%q) = %v, want %v", tc.msg, got, tc.want)
			}
		})
	}
	if isRingENOMEM(nil) {
		t.Error("isRingENOMEM(nil) = true")
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

// TestEngineMayUseIOUringAgreesWithTheEngine1104 ties engineMayUseIOUring to the
// decision the engine itself takes. The helper cannot call the engine's parser
// (probe.parseTierName is unexported) or probe.Probe (it builds a ring, which a
// memlock shortage makes fail, the very case the helper must not be fooled by),
// so it re-states the cap rule: CELERIS_MAX_IOURING_TIER reads optional, high
// and base as those tiers and every other non-empty value, case-sensitively, as
// none. This test runs both on a host where probe.Probe works, for the values
// that matter, and fails if they ever disagree (the rule changed in probe, or a
// new alias was added). The kernel-before-5.19 branch of the helper cannot be
// exercised here (the host's kernel is what it is); it mirrors the engine's
// requirement (engine.go, requireAsyncCancelFlags), and the table test above
// covers it with the host stood in. This mirrors the copy of the helper and of
// this test in internal/engine/iouring (ring_budget_linux_test.go,
// ring_pressure_scope_linux_test.go); keep the two in step.
func TestEngineMayUseIOUringAgreesWithTheEngine1104(t *testing.T) {
	t.Setenv("CELERIS_MAX_IOURING_TIER", "")
	if !probe.Probe().IOUringTier.Available() {
		t.Skip("probe.Probe finds no io_uring tier here: nothing to compare the helper with")
	}
	if !engineMayUseIOUring() {
		t.Skip("the kernel is before 5.19: the helper answers false whatever the tier")
	}
	for _, v := range []string{"", "none", "NONE", "optional", "high", "base", "Base", "bogus"} {
		t.Run("tier="+v, func(t *testing.T) {
			t.Setenv("CELERIS_MAX_IOURING_TIER", v)
			// probe.Probe builds a ring; under a transient shortage it reads tier none.
			want := false
			for i := 0; i < 5 && !want; i++ {
				want = probe.Probe().IOUringTier.Available()
				if !want {
					time.Sleep(50 * time.Millisecond)
				}
			}
			if got := engineMayUseIOUring(); got != want {
				t.Errorf("CELERIS_MAX_IOURING_TIER=%q: engineMayUseIOUring() = %v, probe.Probe().IOUringTier.Available() = %v", v, got, want)
			}
		})
	}
}
