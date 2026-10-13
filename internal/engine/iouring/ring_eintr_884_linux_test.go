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
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ring, err := NewRing(16, 0, 0)
	if err != nil {
		skipOrFail656(t, "io_uring unavailable: %v", err)
	}
	defer func() { _ = ring.Close() }()
	tid, pid := unix.Gettid(), os.Getpid()
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

	// Nothing is queued and no completion is owed: the wait can only end by
	// the signal, or, if the kernel restarted it, by the NOP below.
	wake := time.AfterFunc(2*time.Second, func() { t.Log("the enter outlived 2 s of signals") })
	defer wake.Stop()
	start := time.Now()
	werr := ring.SubmitAndWait()
	took := time.Since(start)
	t.Logf("celeris884 SubmitAndWait returned %v after %v", werr, took.Round(time.Millisecond))
	if werr != nil {
		t.Fatalf("SubmitAndWait answered %v to a signal; the timed waits answer nil for it, and the loop shuts the worker down on an error", werr)
	}
}
