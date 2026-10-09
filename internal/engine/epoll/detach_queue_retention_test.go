//go:build linux

package epoll

import (
	"runtime"
	"testing"
)

// TestDrainDetachQueueDropsSlotRefs is the epoll half of the same guard the
// io_uring worker carries. Truncating the hand-off queue with [:0] reuses the
// backing array but leaves every *connState in it reachable, so the queue
// pins its own high-water mark worth of conns — buffers, H1State and, on the
// WebSocket path, whatever the middleware hung off that state — until some
// later drain overwrites each slot.
//
// detachClosed short-circuits the drain body, so the queue can be exercised
// end to end without an epoll fd or a live connection.
func TestDrainDetachQueueDropsSlotRefs(t *testing.T) {
	const depth = 64

	l := &Loop{}
	for range depth {
		l.detachQueue = append(l.detachQueue, &connState{fd: -1, detachClosed: true})
	}
	l.detachQPending.Store(1)

	l.drainDetachQueue()

	if got := len(l.detachQSpare); got != 0 {
		t.Fatalf("spare queue not truncated: len = %d, want 0", got)
	}
	full := l.detachQSpare[:cap(l.detachQSpare)]
	if len(full) < depth {
		t.Fatalf("backing array shrank to %d, expected at least the %d enqueued slots", len(full), depth)
	}
	held := 0
	for _, cs := range full[:depth] {
		if cs != nil {
			held++
		}
	}
	if held != 0 {
		t.Fatalf("drainDetachQueue left %d of %d connStates reachable in the reused backing array", held, depth)
	}
}

// TestDrainDetachQueueLetsDrainedConnsBeCollected states the same guarantee
// as an observable outcome: a conn the queue has already drained must become
// unreachable once the caller drops its own reference.
func TestDrainDetachQueueLetsDrainedConnsBeCollected(t *testing.T) {
	l := &Loop{}
	collected := make(chan struct{})

	func() {
		cs := &connState{fd: -1, detachClosed: true}
		runtime.AddCleanup(cs, func(ch chan struct{}) { close(ch) }, collected)
		l.detachQueue = append(l.detachQueue, cs)
		for range 15 {
			l.detachQueue = append(l.detachQueue, &connState{fd: -1, detachClosed: true})
		}
	}()
	l.detachQPending.Store(1)
	l.drainDetachQueue()

	for range 5 {
		runtime.GC()
		select {
		case <-collected:
			runtime.KeepAlive(l)
			return
		default:
		}
	}
	// KeepAlive is load-bearing: without it the compiler may treat l as dead
	// after its last use, the Loop and its queues become collectable, and the
	// cleanup runs regardless of what the queue did — passing against the
	// unfixed code.
	runtime.KeepAlive(l)
	t.Fatal("a connState the detach queue already drained is still reachable after 5 GC cycles")
}
