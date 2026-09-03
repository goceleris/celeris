//go:build linux

package iouring

import (
	"context"
	"testing"
)

// TestConnGenerationUniqueAcrossFreshAllocations is the celeris#470
// regression guard.
//
// The generation stamped into a conn-bound SQE's user_data is the ONLY thing
// distinguishing one occupant of an fd from the next. cancelConnOps submits
// an ASYNC_CANCEL keyed (udRecv, fd, gen); if a recycled fd's new occupant
// draws the same generation, that cancel matches the new connection's recv,
// staleConnCQE accepts it as legitimate, and handleRecv closes a healthy
// connection without reading its request.
//
// The old implementation incremented a field on the POOLED connState. Under
// churn, GC drains connStatePool, so acquire almost always returns a fresh
// object and `cs.generation++` yields 1 every single time. Measured on the
// validation workload before the fix: 973/973 conns carried gen=1.
//
// This models exactly that condition -- acquire without releasing, so every
// object is freshly allocated. Pre-fix this fails on the second iteration.
func TestConnGenerationUniqueAcrossFreshAllocations(t *testing.T) {
	const n = 256
	seen := make(map[uint16]int, n)
	held := make([]*connState, 0, n) // never released: forces fresh allocations
	for i := 0; i < n; i++ {
		cs := acquireConnState(context.Background(), 42, 0, false)
		held = append(held, cs)
		if prev, dup := seen[cs.generation]; dup {
			t.Fatalf("generation %d reused (acquire #%d and #%d) for the same fd: "+
				"a close-path ASYNC_CANCEL from the earlier conn would match the "+
				"later conn's recv verbatim (celeris#470)", cs.generation, prev, i)
		}
		seen[cs.generation] = i
	}
	for _, cs := range held {
		releaseConnState(cs)
	}
}

// TestConnGenerationNeverZero pins the one reserved value: encodeUserDataGen
// collapses gen==0 onto the plain encodeUserData encoding, which would make a
// conn-bound CQE indistinguishable from a non-conn-bound one at dispatch.
func TestConnGenerationNeverZero(t *testing.T) {
	held := make([]*connState, 0, 512)
	for i := 0; i < 512; i++ {
		cs := acquireConnState(context.Background(), 7, 0, false)
		if cs.generation == 0 {
			t.Fatalf("acquire #%d produced generation 0, which encodeUserDataGen "+
				"cannot distinguish from a non-conn-bound op", i)
		}
		held = append(held, cs)
	}
	for _, cs := range held {
		releaseConnState(cs)
	}
}

// TestConnGenerationDistinctAcrossPoolRecycle covers the recycled-object path
// too: even when the pool DOES hand back the same object, consecutive
// occupants of an fd must not share a generation.
func TestConnGenerationDistinctAcrossPoolRecycle(t *testing.T) {
	seen := make(map[uint16]bool, 1024)
	for i := 0; i < 1024; i++ {
		cs := acquireConnState(context.Background(), 99, 0, false)
		if seen[cs.generation] {
			t.Fatalf("generation %d reused on iteration %d across pool recycle", cs.generation, i)
		}
		seen[cs.generation] = true
		releaseConnState(cs)
	}
}
