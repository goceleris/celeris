//go:build linux

package iouring

import (
	"testing"

	"github.com/goceleris/celeris/internal/conn"
)

// TestDetachedCountOnlyDecrementsWhatItIncremented pins celeris#549.
//
// detachedCount is incremented on two schedules: directly in OnDetach in sync
// mode, and deferred to drainDetachQueue in async mode, where OnDetach runs on
// the dispatch goroutine and may not touch worker-owned state. The decrement
// used to infer eligibility from h1State.Detached, which the dispatch
// goroutine sets in OnDetach — before the deferred increment has run.
//
// So an async conn that closed inside that window decremented without ever
// having incremented, and drainDetachQueue then skipped its increment on the
// detachClosed guard, making the loss permanent. detachedCount gates the idle
// sweep, so drift silently removes idle enforcement from detached WS/SSE
// connections.
func TestDetachedCountOnlyDecrementsWhatItIncremented(t *testing.T) {
	t.Run("counted conn decrements", func(t *testing.T) {
		w := &Worker{detachedCount: 3}
		cs := &connState{detachCounted: true}

		w.releaseDetachedCount(cs)

		if w.detachedCount != 2 {
			t.Errorf("detachedCount = %d, want 2", w.detachedCount)
		}
		if cs.detachCounted {
			t.Error("detachCounted should be cleared so a second close cannot double-decrement")
		}
	})

	t.Run("uncounted conn does not decrement", func(t *testing.T) {
		// The async window: OnDetach ran on the dispatch goroutine, so the
		// conn looks detached, but drainDetachQueue has not incremented yet.
		// Model the window exactly: OnDetach ran on the dispatch goroutine,
		// so h1State.Detached is already true — which is what the old code
		// keyed on — while drainDetachQueue has not incremented yet.
		w := &Worker{detachedCount: 3}
		cs := &connState{h1State: &conn.H1State{}}
		cs.h1State.Detached.Store(true)
		cs.asyncDetachPending = true

		w.releaseDetachedCount(cs)

		if w.detachedCount != 3 {
			t.Errorf("detachedCount = %d, want 3 — a conn that never incremented "+
				"must not decrement; the drift is permanent because "+
				"drainDetachQueue skips its increment once the conn is closed",
				w.detachedCount)
		}
	})

	t.Run("double close decrements once", func(t *testing.T) {
		w := &Worker{detachedCount: 1}
		cs := &connState{detachCounted: true}

		w.releaseDetachedCount(cs)
		w.releaseDetachedCount(cs)

		if w.detachedCount != 0 {
			t.Errorf("detachedCount = %d, want 0", w.detachedCount)
		}
	})
}
