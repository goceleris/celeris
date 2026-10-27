//go:build linux

package integration

import (
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// squeezeMemlock lowers this process's soft RLIMIT_MEMLOCK to a size no
// io_uring ring fits in and raises it back to the old value after d (or at
// cleanup). Within d a ring setup fails the way it does on a CI runner that
// still has the rings of the engines before it charged against its 8 MiB:
// "io_uring_setup: cannot allocate memory (likely RLIMIT_MEMLOCK ...)". It is
// the deterministic form of that flake, for the tests of the starters that must
// ride it out (the lane celeris#1104 tracks). The limit is per process, so a test that
// squeezes must not run in parallel with one that starts a ring (none here
// calls t.Parallel).
func squeezeMemlock(t *testing.T, d time.Duration) {
	t.Helper()
	var old unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_MEMLOCK, &old); err != nil {
		t.Skipf("getrlimit(RLIMIT_MEMLOCK): %v", err)
	}
	if old.Cur <= 64<<10 {
		t.Skipf("RLIMIT_MEMLOCK is already %d bytes: nothing to squeeze", old.Cur)
	}
	low := unix.Rlimit{Cur: 64 << 10, Max: old.Max}
	if err := unix.Setrlimit(unix.RLIMIT_MEMLOCK, &low); err != nil {
		t.Skipf("setrlimit(RLIMIT_MEMLOCK): %v", err)
	}
	var once sync.Once
	restore := func() { once.Do(func() { _ = unix.Setrlimit(unix.RLIMIT_MEMLOCK, &old) }) }
	timer := time.AfterFunc(d, restore)
	t.Cleanup(func() { timer.Stop(); restore() })
}
