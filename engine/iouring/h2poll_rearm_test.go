//go:build linux

package iouring

import (
	"testing"
	"unsafe"
)

// fullSQRing returns a Ring that always reports a full submission queue:
// sqEntries is 0, so GetSQE's "tail-head >= sqEntries" check is immediately
// true and it returns nil before touching the SQ array or the SQE memory.
// That is the condition prepareH2Poll must not swallow.
func fullSQRing() *Ring {
	var head, tail uint32
	return &Ring{
		singleIssuer: true,
		sqHead:       unsafe.Pointer(&head),
		sqTail:       unsafe.Pointer(&tail),
	}
}

// TestDrainDriverActionsDoesNotSwallowAFullSQRing pins celeris#537.
//
// prepareH2Poll reports whether the arm was actually placed, and its docstring
// is explicit that a full SQ ring must not be swallowed: the poll is
// single-shot and w.h2PollArmed is never cleared anywhere else, so a dropped
// arm leaves the eventfd deaf for the life of the worker — handler goroutines
// writing it stop waking the ring. drainDriverActions discarded the result and
// set h2PollArmed unconditionally, which is the same defect celeris#523 fixed
// at the other seven call sites.
func TestDrainDriverActionsDoesNotSwallowAFullSQRing(t *testing.T) {
	w := &Worker{ring: fullSQRing(), h2EventFD: 1}
	w.driverActionPending.Store(1)

	if got := w.ring.GetSQE(); got != nil {
		t.Fatal("setup: the stand-in ring must report a full SQ")
	}

	w.drainDriverActions()

	if w.h2PollArmed {
		t.Fatal("h2PollArmed set although the arm was never placed: the " +
			"eventfd is now deaf for the life of the worker")
	}
}
