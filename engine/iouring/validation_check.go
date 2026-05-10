//go:build linux && validation

package iouring

import (
	"sync"
	"sync/atomic"

	"github.com/goceleris/celeris/validation"
)

// ringTails maps each *Ring to its own monotonic-tail tracker. Multi-worker
// io_uring (the default) holds one Ring per worker, each with an
// independent SQ tail; a single global tracker would false-positive on
// every cross-ring write. The map is populated lazily on first
// validateSQEWrite call for a given ring.
//
// Memory note: the *Ring keys are leaked here for the process lifetime
// of a validation build. That's acceptable: validation builds are
// soak-test artefacts, not long-running production binaries, and ring
// instances are bounded by worker count.
var ringTails sync.Map

// validateSQEWrite is invoked from GetSQE after it advances the SQ
// tail. The supplied tail is the post-advance value (i.e. the index
// just past the SQE we returned). If a later call observes a lower
// tail than a previous call ON THE SAME RING, the increment was
// non-monotonic — exactly the violation the production-readiness
// audit said the validation harness must catch.
//
// We allow tail < prev when the gap is large (wrap of the 32-bit
// counter). A naive less-than check would false-positive on every
// 2^32 SQEs. Treat any prev > tail with prev-tail < 2^31 as a
// violation.
func validateSQEWrite(r *Ring, tail uint32) {
	v, ok := ringTails.Load(r)
	if !ok {
		var fresh atomic.Uint32
		v, _ = ringTails.LoadOrStore(r, &fresh)
	}
	last := v.(*atomic.Uint32)
	for {
		prev := last.Load()
		if tail >= prev || prev-tail >= 1<<31 {
			if last.CompareAndSwap(prev, tail) {
				return
			}
			continue
		}
		validation.IouringSQECorruptions.Add(1)
		return
	}
}
