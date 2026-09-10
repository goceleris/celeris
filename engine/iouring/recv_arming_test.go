//go:build linux

package iouring

import (
	"sync/atomic"
	"testing"
)

// TestPrepareRecvRefusesSecondArm guards the corruption found in celeris#484.
//
// The WebSocket backpressure pause submits an ASYNC_CANCEL for the armed recv
// and marks the connection paused; the cancel has not landed yet. A handler
// that drains below the low watermark before it does makes drainDetachQueue
// take the resume branch, which used to call prepareRecv unconditionally. That
// left two recvs in flight on one socket, both targeting cs.buf, so the
// kernel's second write landed on top of the first one's unread bytes and the
// WebSocket parser found a frame boundary at the wrong offset.
//
// The invariant is one recv per connection. prepareRecv reports success on the
// second call because "a recv is armed for this conn" is the postcondition its
// callers act on — a false would make them set needsRecv and retry forever.
func TestPrepareRecvRefusesSecondArm(t *testing.T) {
	ring := newTestRing(t)
	w := &Worker{ring: ring, errCount: &atomic.Uint64{}}

	cs := &connState{fd: 7, generation: 3, buf: make([]byte, 4096)}

	if !w.prepareRecv(cs, cs.buf) {
		t.Fatal("first arm was refused on an empty SQ ring")
	}
	if !cs.recvArmed {
		t.Fatal("first arm did not set recvArmed")
	}
	pendingAfterFirst := ring.Pending()
	inflightAfterFirst := cs.kernelInflight

	if !w.prepareRecv(cs, cs.buf) {
		t.Fatal("second arm reported failure; callers would set needsRecv and " +
			"retry from the dirty list forever")
	}
	if got := ring.Pending(); got != pendingAfterFirst {
		t.Fatalf("a second recv SQE was submitted for one connection: "+
			"ring pending %d→%d (both recvs target cs.buf, celeris#484)",
			pendingAfterFirst, got)
	}
	if cs.kernelInflight != inflightAfterFirst {
		t.Fatalf("kernelInflight counted a recv that was never submitted: %d→%d",
			inflightAfterFirst, cs.kernelInflight)
	}
}
