//go:build linux && validation

package iouring

import (
	"sync/atomic"

	"github.com/goceleris/celeris/validation"
)

// lastSQEWriteTail tracks the highest SQE tail value previously
// observed inside GetSQE. The wave-7 invariant is: the ring's tail
// is strictly monotonic from any one issuer thread (single-issuer
// rings) and never decreases overall (multi-issuer rings, modulo
// wrap). A drop in the observed tail means another goroutine raced
// with the issuer that owns this ring — exactly the failure mode
// that PR #36's send-queue corruption bug exhibited.
//
// We use a single global atomic rather than per-Ring state so the
// counter increments on any cross-thread anomaly regardless of which
// Ring instance hit it. The validator-checker downstream maps this
// counter to the "no SQE corruption" property predicate.
var lastSQEWriteTail atomic.Uint32

// validateSQEWrite is invoked from GetSQE after it advances the SQ
// tail. The supplied tail is the post-advance value (i.e. the index
// just past the SQE we returned). If a later call observes a lower
// tail than a previous call, the increment was non-monotonic
// — exactly the violation the production-readiness audit said the
// validation harness must catch.
//
// We allow tail < prev when the gap is large (wrap of the 32-bit
// counter). A naive less-than check would false-positive on every
// 2^32 SQEs. The wave-7 builds run for hours, not weeks, so a real
// wrap is unreachable; treat any prev > tail with prev-tail < 2^31
// as a violation.
func validateSQEWrite(_ *Ring, tail uint32) {
	for {
		prev := lastSQEWriteTail.Load()
		if tail >= prev || prev-tail >= 1<<31 {
			if lastSQEWriteTail.CompareAndSwap(prev, tail) {
				return
			}
			continue
		}
		validation.IouringSQECorruptions.Add(1)
		return
	}
}
