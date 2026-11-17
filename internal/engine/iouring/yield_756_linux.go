//go:build linux

package iouring

import (
	"runtime"
	"time"

	"github.com/goceleris/celeris/internal/mono"
)

// yieldMinGap is the shortest time between two idle-wait yields of one worker
// (celeris#756). See the epoll engine's constant of the same name: a yield
// costs a thread handoff each way, and the gap bounds it for a worker that
// goes idle and busy again at a high rate.
const yieldMinGap = 100 * time.Microsecond

// yieldBeforeWait hands the worker's P to the runtime when the worker has
// readied a handler goroutine since its last wait and is about to block on
// the ring (celeris#756). The worker thread is locked to its OS thread and
// waits in io_uring_enter holding its P; a handler it readied sits in that
// P's run queue until another P steals it, which needs a free P (the GC's
// mark workers can hold them). runtime.Gosched on a locked goroutine runs
// the stoplockedm / handoffp path: another M takes the P and runs the
// handler now, and the worker resumes when its goroutine is picked up again.
//
// Taken only when the wait would block: no completion is ready (a busy
// worker never pays the handoff). SQEs the worker queued are submitted first
// so the yield does not delay them. Not under SQPOLL (the kernel thread
// submits, and the worker's wait is a poll there). At most one yield per
// yieldMinGap.
func (w *Worker) yieldBeforeWait() {
	w.yieldReady = false
	if w.sqpoll {
		return
	}
	if head, tail := w.ring.BeginCQ(); head != tail {
		return
	}
	now := mono.Now()
	if now-w.yieldLastNs < int64(yieldMinGap) {
		return
	}
	if w.ring.Pending() > 0 {
		if _, err := w.ring.Submit(); err != nil {
			return // the wait below retries the submit and owns the error
		}
	}
	w.yieldLastNs = now
	runtime.Gosched()
}
