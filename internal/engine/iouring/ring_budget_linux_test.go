//go:build linux

package iouring

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
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

// ioUringUsable caches a positive answer of ioUringUsableHere only.
var ioUringUsable atomic.Bool

// ioUringUsableHere reports whether this process can create an io_uring
// instance at all: a 4-entry ring, two pages. False means the environment has
// no usable io_uring (seccomp, io_uring_disabled, an old kernel), which is a
// legitimate reason to skip. A ring ENOMEM is NOT that: io_uring works and
// the memlock budget is held, which is the failure skipUnlessIOUringUnusable
// exists to report. It used to cache its first answer in a sync.Once, so a
// probe that met a transient ENOMEM turned every later budget failure in the
// process into a skip (celeris#662 review). Only "usable" is cached now; a
// negative answer is probed again next time.
func ioUringUsableHere() bool {
	if ioUringUsable.Load() {
		return true
	}
	r, err := NewRing(4, 0, 0)
	if err == nil {
		_ = r.Close()
	}
	if err == nil || isRingENOMEM(err) {
		ioUringUsable.Store(true)
		return true
	}
	return false
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

// errNoBind662 is a Listen that neither failed nor bound with workers in time.
var errNoBind662 = errors.New("engine did not bind with workers within 10s")

// startRingRetried662 builds an engine with build and runs its Listen until it
// has bound with workers. A start that failed only on ring ENOMEM is retried
// (retryRingENOMEM): at a low RLIMIT_MEMLOCK the kernel may not have
// uncharged the rings of the engines the tests before this one stopped. Any
// other failure is judged by skipOrFail656, as the rigs judged it before; a
// Listen that neither failed nor bound fails the test. It returns the running
// engine, its cancel, and a channel that receives Listen's result.
func startRingRetried662(t *testing.T, build func() (*Engine, error)) (*Engine, context.CancelFunc, <-chan error) {
	t.Helper()
	var (
		e      *Engine
		cancel context.CancelFunc
		done   chan error
	)
	waited, tries, err := retryRingENOMEM(func() error {
		var berr error
		if e, berr = build(); berr != nil {
			return berr
		}
		var ctx context.Context
		ctx, cancel = context.WithCancel(context.Background())
		done = make(chan error, 1)
		go func() { done <- e.Listen(ctx) }()
		for dl := time.Now().Add(10 * time.Second); time.Now().Before(dl); {
			if e.Addr() != nil && e.NumWorkers() > 0 {
				return nil
			}
			select {
			case lerr := <-done:
				cancel()
				if lerr == nil {
					lerr = errors.New("Listen returned nil before binding")
				}
				return lerr
			default:
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
		return errNoBind662
	})
	if tries > 1 {
		t.Logf("engine start retried on ring ENOMEM: %d tries over %v", tries, waited.Round(time.Millisecond))
	}
	switch {
	case errors.Is(err, errNoBind662):
		t.Fatal(err)
	case err != nil:
		skipOrFail656(t, "io_uring engine did not start here: %v", err)
	}
	return e, cancel, done
}
