//go:build linux

package iouring

// celeris#1381, the part of the change the black-box tests of idle_wait_1381
// cannot reach: what "holds nothing" is for a worker, one row per thing a worker
// can hold or be owed, and the wait that follows from it. The default
// ReadHeaderTimeout (10 s) caps an idle worker's ring wait at 25 ms; a worker
// that holds nothing keeps the 100 ms wait.

import (
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/resource"
	"github.com/goceleris/celeris/internal/wakefd"
)

// quiescentWorker1381 is a bare Worker that holds nothing, idle for more than
// 100 rounds, with the default header timeout.
func quiescentWorker1381(t *testing.T) *Worker {
	t.Helper()
	efd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
	if err != nil {
		t.Fatalf("eventfd: %v", err)
	}
	w := &Worker{
		cfg:          resource.Config{ReadHeaderTimeout: 10 * time.Second},
		listenFD:     3,
		wakeFD:       wakefd.New(efd),
		h2PollArmed:  true,
		acceptPaused: new(atomic.Bool),
		emptyIters:   200,
	}
	t.Cleanup(w.wakeFD.Close)
	return w
}

func TestIdleWaitOfAWorkerThatHoldsNothing1381(t *testing.T) {
	w := quiescentWorker1381(t)
	if !w.idleQuiescent() {
		t.Fatal("PREMISE: a worker that holds nothing is not quiescent")
	}
	if got := w.baseTimeout(); got != 100*time.Millisecond {
		t.Errorf("a worker that holds nothing waits %v, want 100ms (the 25 ms cap is for the conns it holds)", got)
	}
	for _, tc := range []struct {
		name string
		mut  func(w *Worker)
	}{
		{"no listener (paused, standby, stopping)", func(w *Worker) { w.listenFD = -1 }},
		{"no wake eventfd", func(w *Worker) { w.wakeFD = wakefd.New(-1) }},
		{"the wake poll not armed", func(w *Worker) { w.h2PollArmed = false }},
		{"a connection counted", func(w *Worker) { w.connCount = 1 }},
		{"a live connection (HTTP/1, HTTP/2 and detached conns all are)", func(w *Worker) { w.liveConns = []int{7} }},
		{"an HTTP/2 connection", func(w *Worker) { w.h2Conns = []int{7} }},
		{"a detached connection", func(w *Worker) { w.detachedCount = 1 }},
		{"a dirty connection", func(w *Worker) { w.dirtyHead = &connState{} }},
		{"a descriptor owed a release", func(w *Worker) { w.closeFDOwed = 1 }},
		{"draining", func(w *Worker) { w.draining = true }},
		{"HTTP/2 pool handlers being waited out", func(w *Worker) { w.h2DrainStart = 1 }},
		{"accept paused", func(w *Worker) { w.acceptPaused.Store(true) }},
		{"a transplant drain set", func(w *Worker) { w.transplant.Store(&transplantTargetHolder{}) }},
		{"residue published and not yet retracted", func(w *Worker) { w.sweepPub[0] = 1 }},
		{"a driver connection registered", func(w *Worker) { w.hasDriverConns.Store(true) }},
		{"a driver action queued (register, write, adopt)", func(w *Worker) { w.driverActionPending.Store(1) }},
		{"the detach queue not empty", func(w *Worker) { w.detachQPending.Store(1) }},
		{"a request count not flushed", func(w *Worker) { w.reqBatch = 1 }},
		{"received bytes not flushed", func(w *Worker) { w.bytesReadBatch = 1 }},
		{"sent bytes not flushed", func(w *Worker) { w.bytesWrittenBatch = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := quiescentWorker1381(t)
			tc.mut(w)
			if w.idleQuiescent() {
				t.Errorf("a worker with %q is quiescent: it would take the long wait", tc.name)
			}
			// The cap stays 25 ms for a worker that holds something with a header
			// deadline; the rows that are not a held conn (no listener: 1 s, a dirty
			// list: 0) have waits of their own, which the cap does not decide.
			switch tc.name {
			case "no listener (paused, standby, stopping)", "a dirty connection", "an HTTP/2 connection":
			default:
				if got := w.baseTimeout(); got != 25*time.Millisecond {
					t.Errorf("a worker with %q waits %v, want the 25 ms cap", tc.name, got)
				}
			}
		})
	}
	t.Run("not yet idle for 100 rounds", func(t *testing.T) {
		w := quiescentWorker1381(t)
		w.emptyIters = 50
		if got := w.baseTimeout(); got != 5*time.Millisecond {
			t.Errorf("a worker 50 rounds idle waits %v, want 5ms", got)
		}
	})
	t.Run("no header timeout", func(t *testing.T) {
		w := quiescentWorker1381(t)
		w.cfg.ReadHeaderTimeout = -1
		if got := w.baseTimeout(); got != 100*time.Millisecond {
			t.Errorf("without a header deadline an idle worker waits %v, want 100ms (as before the change)", got)
		}
		w.connCount = 1
		if got := w.baseTimeout(); got != 100*time.Millisecond {
			t.Errorf("without a header deadline a worker with a connection waits %v, want 100ms (as before the change)", got)
		}
	})
}
