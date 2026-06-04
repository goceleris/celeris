//go:build linux

package iouring

import (
	"testing"
	"time"
)

// TestPendingReleaseIsTimeBased is the regression guard for the churn-close
// memory blowup. The deferred connState-release window (which holds a closed
// connState alive past io_uring's recv-cancellation latency to avoid the #256
// kernel-write use-after-free) MUST be wall-clock based, not event-loop
// iteration based.
//
// An iteration count is meaningless across load regimes:
//   - under churn-close the loop spins so fast that N iterations elapse in
//     microseconds (potentially SHORTER than the cancel latency → reopening the
//     #256 UAF) while the high close rate piles tens of thousands of connStates
//     into the queue at ~16 KB each;
//   - when the loop is idle (SubmitAndWaitTimeout up to ~100 ms/iter) N
//     iterations stretch to minutes, pinning that memory long after the load
//     drops. Observed: RSS ~35 MB → ~1.9 GB under churn, retained for ~8 min.
//
// The fix records a wall-clock deadline (now + pendingReleaseHoldNanos) at
// enqueue and drains against w.cachedNow, so the queue depth is bounded by
// close_rate × hold and drains promptly once churn stops.
func TestPendingReleaseIsTimeBased(t *testing.T) {
	w := &Worker{}
	start := time.Now().UnixNano()

	w.queuePendingRelease(&connState{})         // pooled-recycle path
	w.queuePendingReleaseDetached(&connState{}) // detached path (drops the ref)
	if got := len(w.pendingRelease); got != 2 {
		t.Fatalf("queued = %d, want 2", got)
	}

	// Each entry's release deadline is a future wall-clock timestamp, not an
	// iteration counter.
	for i, e := range w.pendingRelease {
		if e.releaseAtNanos < start+pendingReleaseHoldNanos {
			t.Fatalf("entry %d releaseAtNanos=%d < start+hold=%d: window is not wall-clock based",
				i, e.releaseAtNanos, start+pendingReleaseHoldNanos)
		}
	}

	// While cachedNow is still before the deadline, NO number of event-loop
	// iterations may drain an entry. The old iteration-based code drained once
	// iterCount advanced past releaseAtIter — exactly the bug this guards.
	w.cachedNow = start // well before start+hold
	for i := 0; i < 100_000; i++ {
		w.drainPendingRelease()
	}
	if got := len(w.pendingRelease); got != 2 {
		t.Fatalf("drained before the wall-clock hold elapsed: %d left, want 2 "+
			"(iteration-based-release regression)", got)
	}

	// Once wall-clock advances past the hold, both entries drain (the detached
	// one drops its ref; the pooled one is recycled to the sync.Pool).
	w.cachedNow = start + pendingReleaseHoldNanos + int64(time.Second)
	w.drainPendingRelease()
	if got := len(w.pendingRelease); got != 0 {
		t.Fatalf("not drained after the hold elapsed: %d left, want 0", got)
	}
}

// TestPendingReleaseDrainsFIFOByDeadline verifies entries drain in enqueue order
// as the wall clock crosses each deadline, and that a partial advance drains only
// the entries whose hold has elapsed (the queue is not all-or-nothing).
func TestPendingReleaseDrainsFIFOByDeadline(t *testing.T) {
	w := &Worker{}

	// Enqueue three entries ~10 ms apart in wall-clock terms by pinning cachedNow
	// is not enough (queue uses time.Now()), so capture each deadline.
	w.queuePendingRelease(&connState{})
	d0 := w.pendingRelease[0].releaseAtNanos
	time.Sleep(2 * time.Millisecond)
	w.queuePendingRelease(&connState{})
	time.Sleep(2 * time.Millisecond)
	w.queuePendingRelease(&connState{})
	d2 := w.pendingRelease[2].releaseAtNanos

	if !(d0 < d2) {
		t.Fatalf("deadlines not monotonic: d0=%d d2=%d", d0, d2)
	}

	// Advance the clock to just past the first deadline only.
	w.cachedNow = d0
	w.drainPendingRelease()
	if got := len(w.pendingRelease); got != 2 {
		t.Fatalf("after crossing first deadline: %d left, want 2", got)
	}

	// Advance past the last deadline → everything drains.
	w.cachedNow = d2 + 1
	w.drainPendingRelease()
	if got := len(w.pendingRelease); got != 0 {
		t.Fatalf("after crossing last deadline: %d left, want 0", got)
	}
}
