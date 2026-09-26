//go:build linux

package iouring

import (
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Ring memory, and the tests that start engines one after another.
//
// The kernel charges the SQ and CQ rings of every io_uring instance a process
// creates to the per-UID locked-memory count, against RLIMIT_MEMLOCK (7.0:
// io_create_region -> __io_account_mem), and gives the pages back only when
// the ring context is freed. That happens asynchronously, in
// io_ring_exit_work, after an RCU grace period for DEFER_TASKRUN rings: 12-23
// ms after the close, measured at GitHub's default 8 MiB in a golang:1.27
// container on kernel 7.0 (evidence celeris-662/r6/gate2). An engine start
// holds about 1.6 MiB of that 8 MiB (an 8192-entry tier-probe ring and an
// 8192-entry worker ring), so five or six starts inside that latency fail
// with ENOMEM although nothing leaked; the rings are closed and merely not
// uncharged yet.
//
// The tests that start engines back to back used to read that ENOMEM as "io_uring
// unavailable on this runner" and skip. TestListenWithCancelledContextPublishesTheBoundAddress
// skipped in every 8 MiB run on record (31 of 31), and so did
// TestListenRefusesToStartWhenNoWorkerReportsItsAddress in 24 of 31, which it
// follows. The CI step that ran this package at 8 MiB printed no skips, so
// neither celeris#639 test had asserted anything in CI.
//
// retryRingENOMEM waits that latency out instead of skipping, and
// skipUnlessIOUringUnusable turns an ENOMEM that outlasts it into a failure
// whenever io_uring itself works here and RLIMIT_MEMLOCK is at least
// ringBudgetFloor.

// ringUnchargeBound is how long an engine start that failed only on ring
// ENOMEM is retried: about 400 times the uncharge latency measured.
const ringUnchargeBound = 10 * time.Second

// ringBudgetFloor is the smallest RLIMIT_MEMLOCK these tests hold themselves
// to running under: room for about two and a half engine starts. Below
// it -- the 64 KiB default of an older distribution, say -- a start may not
// fit at all, waiting cannot help, and an ENOMEM is the environment's: the
// tests skip, as they always did. GitHub's runners give 8 MiB.
const ringBudgetFloor = 4 << 20

// memlockBelowRingBudgetFloor returns the soft RLIMIT_MEMLOCK and whether it
// is finite and below ringBudgetFloor.
func memlockBelowRingBudgetFloor() (uint64, bool) {
	var rl unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_MEMLOCK, &rl); err != nil || rl.Cur == ^uint64(0) {
		return 0, false
	}
	return rl.Cur, rl.Cur < ringBudgetFloor
}

// isRingENOMEM reports whether err is a ring allocation that failed on
// RLIMIT_MEMLOCK.
func isRingENOMEM(err error) bool {
	return err != nil && strings.Contains(err.Error(), "cannot allocate memory")
}

var (
	ioUringUsableOnce sync.Once
	ioUringUsable     bool
)

// ioUringUsableHere reports whether this process can create an io_uring
// instance at all: a 4-entry ring, two pages. False means the environment has
// no usable io_uring (seccomp, io_uring_disabled, an old kernel), which is a
// legitimate reason to skip.
func ioUringUsableHere() bool {
	ioUringUsableOnce.Do(func() {
		if r, err := NewRing(4, 0, 0); err == nil {
			_ = r.Close()
			ioUringUsable = true
		}
	})
	return ioUringUsable
}

// retryRingENOMEM runs start until it returns something other than a ring
// ENOMEM, or until ringUnchargeBound has passed. It returns how long it
// waited before the last start (the retries, not the last start's own
// duration), how many times it ran start, and start's last error. Below
// ringBudgetFloor it runs start once: there is nothing to wait for.
func retryRingENOMEM(start func() error) (waited time.Duration, tries int, err error) {
	t0 := time.Now()
	_, small := memlockBelowRingBudgetFloor()
	for {
		tries++
		waited = time.Since(t0)
		err = start()
		if small || !isRingENOMEM(err) || time.Since(t0) > ringUnchargeBound {
			return waited, tries, err
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// skipUnlessIOUringUnusable is what a test does with an engine start that
// failed on io_uring itself (ioUringUnavailable639) after retryRingENOMEM: it
// skips only when io_uring cannot be used here at all or RLIMIT_MEMLOCK is
// below ringBudgetFloor, and FAILS otherwise, because then the rings are
// genuinely exhausted -- leaked, or held by something still running -- and
// that is not the environment's fault.
func skipUnlessIOUringUnusable(t *testing.T, err error) {
	t.Helper()
	if lim, small := memlockBelowRingBudgetFloor(); small {
		t.Skipf("RLIMIT_MEMLOCK is %d KiB, below the %d MiB these tests need to start io_uring engines "+
			"one after another: %v", lim>>10, ringBudgetFloor>>20, err)
	}
	if ioUringUsableHere() {
		t.Fatalf("io_uring works here (a 4-entry ring can be created) but an engine start still failed "+
			"after retrying for %v while the kernel uncharged closed rings: %v -- a ring leaked, or the "+
			"memlock budget is held by something still running", ringUnchargeBound, err)
	}
	t.Skipf("io_uring unavailable on this runner: %v", err)
}
