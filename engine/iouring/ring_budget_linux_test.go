//go:build linux

package iouring

import (
	"strings"
	"sync"
	"testing"
	"time"
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
// whenever io_uring itself works here.

// ringUnchargeBound is how long an engine start that failed only on ring
// ENOMEM is retried: about 400 times the uncharge latency measured.
const ringUnchargeBound = 10 * time.Second

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
// waited, how many times it ran start, and start's last error.
func retryRingENOMEM(start func() error) (waited time.Duration, tries int, err error) {
	t0 := time.Now()
	for {
		tries++
		err = start()
		if !isRingENOMEM(err) || time.Since(t0) > ringUnchargeBound {
			return time.Since(t0), tries, err
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// skipUnlessIOUringUnusable is what a test does with an engine start that
// failed on io_uring itself (ioUringUnavailable639) after retryRingENOMEM: it
// skips only when io_uring cannot be used here at all, and FAILS otherwise,
// because then the rings are genuinely exhausted -- leaked, or held by
// something still running -- and that is not the environment's fault.
func skipUnlessIOUringUnusable(t *testing.T, err error) {
	t.Helper()
	if ioUringUsableHere() {
		t.Fatalf("io_uring works here (a 4-entry ring can be created) but an engine start still failed "+
			"after retrying for %v while the kernel uncharged closed rings: %v -- a ring leaked, or the "+
			"memlock budget is held by something still running", ringUnchargeBound, err)
	}
	t.Skipf("io_uring unavailable on this runner: %v", err)
}
