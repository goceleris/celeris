//go:build linux

package iouring

import (
	"sync"
	"testing"

	"github.com/goceleris/celeris/internal/conn"
)

// A detached stream (Server-Sent Events, a WebSocket) whose producer's
// guarded writeFn took only part of its bytes inline, or none (the accepted
// socket is non-blocking), leaves the rest in writeBuf with no SEND in flight
// and the conn on detachQueue, where drainDetachQueue marks it dirty so that
// the loop sends it. The send drain's stopDetachedProducers (celeris#806) ends
// such a conn's writes at the top of a pass; the same pass's drainDetachQueue
// then skips a detachClosed conn before it marks anything dirty, so unless the
// stop does, nothing ever sends the queued bytes, hasPendingSends stays true
// and the drain lasts the whole budget although the client reads (review
// round 2 of the fix, both reviewers). These tests build the state by hand and
// run the real stop and the real drainDetachQueue, as the loop does within
// one pass.

func detachStrandRig806(t *testing.T, mut func(*connState)) (*Worker, *connState) {
	t.Helper()
	w := &Worker{listenFD: -1, conns: make([]*connState, 16)}
	cs := &connState{fd: 7, liveIdx: -1, detachMu: &sync.Mutex{}, h1State: &conn.H1State{}}
	cs.h1State.Detached.Store(true)
	cs.writeBuf = append(cs.writeBuf, make([]byte, 4096)...) // the rest of a partial inline write
	if mut != nil {
		mut(cs)
	}
	w.conns[cs.fd] = cs
	w.addLiveConn(cs)
	w.detachQueue = append(w.detachQueue, cs)
	w.detachQPending.Store(1)
	return w, cs
}

// TestStopDetachedProducersFlushesQueuedBytes806: the stop, then the drain of
// the detach queue, as one loop pass runs them: the conn with queued bytes and
// no SEND outstanding is on the dirty list, where flushDirty sends it. The
// first arm is the control, with no stop: it holds on the code before the
// stop existed, so the other arm's failure is the stop's.
func TestStopDetachedProducersFlushesQueuedBytes806(t *testing.T) {
	t.Run("no-stop-control", func(t *testing.T) {
		w, cs := detachStrandRig806(t, nil)
		w.drainDetachQueue()
		if !cs.dirty {
			t.Fatalf("control: drainDetachQueue did not mark a queued detached conn with %d bytes dirty", len(cs.writeBuf))
		}
	})
	t.Run("stop", func(t *testing.T) {
		w, cs := detachStrandRig806(t, nil)
		w.stopDetachedProducers()
		w.drainDetachQueue()
		if !cs.detachClosed {
			t.Fatal("the stop did not end the conn's writes")
		}
		if !cs.dirty {
			t.Fatalf("STRANDED: detachClosed=%v dirty=%v sending=%v writeBuf=%d hasPendingSends=%v: nothing will send these bytes, and the drain waits for them until its budget ends",
				cs.detachClosed, cs.dirty, cs.sending, len(cs.writeBuf), w.hasPendingSends())
		}
	})
	t.Run("stop-then-nothing-queued", func(t *testing.T) {
		// The stop runs after the producer's unlock but before its append to
		// the queue: the conn is not on detachQueue yet, and must be dirty
		// all the same (the producer's append then still happens: nothing
		// but the stop is left to send the bytes).
		w, cs := detachStrandRig806(t, nil)
		w.detachQueue = nil
		w.detachQPending.Store(0)
		w.stopDetachedProducers()
		if !cs.dirty {
			t.Fatalf("STRANDED: conn not on detachQueue, detachClosed=%v dirty=%v writeBuf=%d", cs.detachClosed, cs.dirty, len(cs.writeBuf))
		}
	})
}

// TestStopDetachedProducersLeavesWhatIsInFlight806: where a completion will
// send the queued bytes, or there are none, the stop does not put the conn on
// the dirty list (a SEND outstanding is re-flushed by its completion; a
// SEND_ZC notification by its notification).
func TestStopDetachedProducersLeavesWhatIsInFlight806(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*connState)
	}{
		{"send-in-flight", func(cs *connState) { cs.sending = true }},
		{"zc-notification-pending", func(cs *connState) { cs.zcNotifPending = true }},
		{"nothing-queued", func(cs *connState) { cs.writeBuf = cs.writeBuf[:0] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, cs := detachStrandRig806(t, tc.mut)
			w.stopDetachedProducers()
			if !cs.detachClosed {
				t.Fatal("the stop did not end the conn's writes")
			}
			if cs.dirty {
				t.Fatal("the stop marked dirty a conn whose bytes a completion is to send")
			}
		})
	}
}

// TestStopDetachedProducersLeavesAnUnfinalisedDetach806: a conn whose async
// Detach has not been finalised by drainDetachQueue (asyncDetachPending) is
// not ended by the stop: that same drain would skip it as detachClosed, and
// closeConn would then not count its celeris#584 window close. It is ended on
// the next pass, once the drain has counted it.
func TestStopDetachedProducersLeavesAnUnfinalisedDetach806(t *testing.T) {
	w, cs := detachStrandRig806(t, func(cs *connState) {
		cs.asyncDetachPending = true
		cs.writeBuf = cs.writeBuf[:0]
	})
	w.stopDetachedProducers()
	if cs.detachClosed {
		t.Fatal("the stop ended a conn whose Detach the loop has not finalised")
	}
	w.drainDetachQueue()
	if cs.asyncDetachPending || !cs.detachCounted || w.detachedCount != 1 {
		t.Fatalf("drainDetachQueue did not finalise the detach: pending=%v counted=%v detachedCount=%d", cs.asyncDetachPending, cs.detachCounted, w.detachedCount)
	}
	w.stopDetachedProducers()
	if !cs.detachClosed {
		t.Fatal("the next stop did not end the finalised conn's writes")
	}
}
