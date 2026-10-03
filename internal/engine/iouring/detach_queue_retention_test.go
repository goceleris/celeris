//go:build linux

package iouring

import (
	"runtime"
	"testing"
)

// TestDrainDetachQueueDropsSlotRefs guards the hand-off queue against the
// retention hazard drainPendingRelease already guards against: truncating a
// []*connState with [:0] reuses the backing array but leaves every pointer in
// it reachable, so the queue pins its own high-water mark worth of
// connStates until some later drain happens to overwrite each slot.
//
// That matters here more than it would for a plain buffer. A detached
// connState carries its read and write buffers, its H1State, and — on the
// WebSocket path — whatever the middleware hung off that state, so a queue
// that once peaked deep holds all of it for as long as the worker lives.
//
// The test enqueues a deep batch of already-closed conns (detachClosed short-
// circuits the drain body, so no ring is needed), drains, and then reads the
// spare slice out to its capacity. Every slot past the new length must be nil.
func TestDrainDetachQueueDropsSlotRefs(t *testing.T) {
	const depth = 64

	w := &Worker{}
	for range depth {
		w.detachQueue = append(w.detachQueue, &connState{fd: -1, detachClosed: true})
	}
	w.detachQPending.Store(1)

	w.drainDetachQueue()

	if got := len(w.detachQSpare); got != 0 {
		t.Fatalf("spare queue not truncated: len = %d, want 0", got)
	}
	full := w.detachQSpare[:cap(w.detachQSpare)]
	if len(full) < depth {
		t.Fatalf("backing array shrank to %d, expected it to keep at least the %d enqueued slots", len(full), depth)
	}
	held := 0
	for _, cs := range full[:depth] {
		if cs != nil {
			held++
		}
	}
	if held != 0 {
		t.Fatalf("drainDetachQueue left %d of %d connStates reachable in the reused backing array; "+
			"the queue pins its high-water mark (celeris#571 sibling)", held, depth)
	}
}

// TestDrainDetachQueueLetsDrainedConnsBeCollected is the same guarantee stated
// as an observable outcome rather than as a property of the slice: once the
// queue has drained a conn and the caller drops its own reference, the
// connState must become unreachable. A finalizer on one of the enqueued
// conns proves it, where reading the backing array only proves the slot was
// cleared.
func TestDrainDetachQueueLetsDrainedConnsBeCollected(t *testing.T) {
	w := &Worker{}
	collected := make(chan struct{})

	func() {
		// Enqueued inside a function literal so the local goes out of scope
		// before the GC below; a live stack slot would keep it reachable
		// regardless of what the queue does.
		cs := &connState{fd: -1, detachClosed: true}
		runtime.AddCleanup(cs, func(ch chan struct{}) { close(ch) }, collected)
		w.detachQueue = append(w.detachQueue, cs)
		// Pad the queue so the drained conn is not the only slot; a
		// single-element array is the easiest case to get right by accident.
		for range 15 {
			w.detachQueue = append(w.detachQueue, &connState{fd: -1, detachClosed: true})
		}
	}()
	w.detachQPending.Store(1)
	w.drainDetachQueue()

	for range 5 {
		runtime.GC()
		select {
		case <-collected:
			runtime.KeepAlive(w)
			return
		default:
		}
	}
	// KeepAlive is load-bearing, not defensive. Without it the compiler is
	// free to treat w as dead after its last use above, the whole Worker
	// (queues included) becomes collectable, and the cleanup runs no matter
	// what the queue did -- the test then passes against the unfixed code.
	runtime.KeepAlive(w)
	t.Fatal("a connState the detach queue already drained is still reachable after 5 GC cycles: " +
		"the queue is holding it in its reused backing array")
}
