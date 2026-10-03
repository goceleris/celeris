//go:build linux && validation

package iouring

import (
	"testing"

	"github.com/goceleris/celeris/validation"
)

// TestValidateSQEWriteNonMonotonicSameRingBumps exercises the
// non-monotonic SQE tail detection: a tail value lower than a
// previously observed tail on the SAME ring must bump the counter.
func TestValidateSQEWriteNonMonotonicSameRingBumps(t *testing.T) {
	before := validation.IouringSQECorruptions.Load()

	r := &Ring{}
	validateSQEWrite(r, 10)
	validateSQEWrite(r, 5) // regression — ring tail moved backwards

	after := validation.IouringSQECorruptions.Load()
	if after < before+1 {
		t.Fatalf("IouringSQECorruptions same-ring regression: got %d, want >=%d (before=%d)", after, before+1, before)
	}
}

// TestValidateSQEWriteCrossRingQuiet verifies CR1: writes to
// independent rings have independent monotonic histories. Writing
// tail=5 on ring B after tail=10 on ring A is NOT a violation —
// each ring tracks its own tail.
func TestValidateSQEWriteCrossRingQuiet(t *testing.T) {
	before := validation.IouringSQECorruptions.Load()

	rA := &Ring{}
	rB := &Ring{}
	validateSQEWrite(rA, 10)
	validateSQEWrite(rB, 5) // ring B has its own history; not a regression
	validateSQEWrite(rB, 6) // monotonic on B
	validateSQEWrite(rA, 11)

	after := validation.IouringSQECorruptions.Load()
	if after != before {
		t.Fatalf("IouringSQECorruptions cross-ring: got %d, want %d (no bump)", after, before)
	}
}

// TestValidateSQEWriteWrapTolerated confirms a 32-bit wrap (large
// negative gap) is treated as a wrap rather than a regression — the
// counter is uint32, so prev=0xFFFFFFFE → tail=2 is a legitimate
// advance, not a corruption.
func TestValidateSQEWriteWrapTolerated(t *testing.T) {
	before := validation.IouringSQECorruptions.Load()

	r := &Ring{}
	validateSQEWrite(r, 0xFFFFFFFE)
	validateSQEWrite(r, 2) // wrap; gap > 2^31

	after := validation.IouringSQECorruptions.Load()
	if after != before {
		t.Fatalf("IouringSQECorruptions wrap: got %d, want %d (no bump)", after, before)
	}
}
