//go:build linux

package iouring

import (
	"os"
	"runtime"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// celeris#884 item 49, a neighbour found while routing the run loop's waits:
// SubmitAndWaitTimeout and WaitCQETimeout answer nil when a signal ends the
// enter (EINTR), and SubmitAndWait, the loop's two untimed waits, did not, so
// a signal delivered to a worker's thread while it slept in the kernel (a
// profiler's SIGPROF, a runtime preemption signal) was an error the loop
// answered with shutdown. The signal is a SIGURG, which the Go runtime handles
// and ignores, sent to the thread that is inside the enter.
func TestSubmitAndWaitIsNotEndedBySignal884(t *testing.T) {
	// The wait runs on a goroutine of its own, pinned to its thread, so that
	// a kernel that restarts the enter after a signal (ERESTARTSYS) cannot
	// hang the test until go test's -timeout: the test goroutine gives up at
	// 5 s and fails.
	type result struct {
		err  error
		took time.Duration
	}
	tidCh := make(chan int, 1)
	resCh := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		ring, err := NewRing(16, 0, 0)
		if err != nil {
			tidCh <- -1
			resCh <- result{err: err}
			return
		}
		defer func() { _ = ring.Close() }()
		tidCh <- unix.Gettid()
		// Nothing is queued and no completion is owed: the wait can only end
		// by a signal.
		start := time.Now()
		werr := ring.SubmitAndWait()
		resCh <- result{werr, time.Since(start)}
	}()
	tid := <-tidCh
	if tid < 0 {
		r := <-resCh
		skipOrFail656(t, "io_uring unavailable: %v", r.err)
	}
	pid := os.Getpid()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				_ = unix.Tgkill(pid, tid, unix.SIGURG)
			}
		}
	}()
	defer func() { close(stop); <-done }()

	select {
	case r := <-resCh:
		t.Logf("celeris884 SubmitAndWait returned %v after %v", r.err, r.took.Round(time.Millisecond))
		if r.err != nil {
			t.Fatalf("SubmitAndWait answered %v to a signal; the timed waits answer nil for it, and the loop shuts the worker down on an error", r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SubmitAndWait was still in the kernel 5 s into a stream of signals: the kernel restarts the enter after one, and a signal does not end the wait")
	}
}
