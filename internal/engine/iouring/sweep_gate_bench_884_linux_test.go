//go:build linux

package iouring

import (
	"testing"

	"github.com/goceleris/celeris/internal/mono"
)

// BenchmarkSweepGate884 is the sweep gate of Worker.run, one iteration's
// worth, in its two forms (celeris#884 items 65 and 75). "old" is the
// expression the iteration count alone made (tickCounter&gate == 0 ||
// sweepWatchDue), "new" is what replaced it: the batch's clock (an iteration
// with CQEs) or a fresh read (an idle one), sweepDue, and sweepWatchDue. The
// loop is never due, so this is the cost of not sweeping, which is what every
// iteration but a few pays. "new/empty-spin" is an empty iteration in a
// zero-wait spin (a clock read on every eighth), "new/empty-idle" one after a
// wait of 5 ms or more (a clock read every time), and "new/fresh-clock" a clock
// read plus the gate, for comparison.
func BenchmarkSweepGate884(b *testing.B) {
	newW := func() *Worker {
		w := &Worker{}
		w.lastSweep = mono.Now()
		w.cachedNow = w.lastSweep
		return w
	}
	b.Run("old", func(b *testing.B) {
		w := newW()
		gate := uint32(0x1F)
		var sink int
		for b.Loop() {
			w.tickCounter++
			if w.tickCounter&gate == 0 || w.sweepWatchDue() {
				sink++
			}
		}
		_ = sink
	})
	b.Run("new/batch-clock", func(b *testing.B) {
		w := newW()
		gate := uint32(0x1F)
		var sink int
		for b.Loop() {
			w.tickCounter++
			if w.sweepDue(w.cachedNow, w.tickCounter&gate == 0) || w.sweepWatchDue() {
				sink++
			}
		}
		_ = sink
	})
	// An empty iteration that is a zero-wait spin (the dirty list non-empty):
	// sweepClock takes the batch clock except on every eighth.
	b.Run("new/empty-spin", func(b *testing.B) {
		w := newW()
		w.dirtyHead = &connState{}
		w.emptyIters = 100
		gate := uint32(0x1F)
		var sink int
		for b.Loop() {
			w.tickCounter++
			gateHit := w.tickCounter&gate == 0
			if w.sweepDue(w.sweepClock(false, gateHit), gateHit) || w.sweepWatchDue() {
				sink++
			}
		}
		_ = sink
	})
	// An empty iteration after a wait of 5 ms or more: always a fresh read.
	b.Run("new/empty-idle", func(b *testing.B) {
		w := newW()
		w.emptyIters = 100
		gate := uint32(0x1F)
		var sink int
		for b.Loop() {
			w.tickCounter++
			gateHit := w.tickCounter&gate == 0
			if w.sweepDue(w.sweepClock(false, gateHit), gateHit) || w.sweepWatchDue() {
				sink++
			}
		}
		_ = sink
	})
	b.Run("new/fresh-clock", func(b *testing.B) {
		w := newW()
		gate := uint32(0x1F)
		var sink int
		for b.Loop() {
			w.tickCounter++
			if w.sweepDue(mono.Now(), w.tickCounter&gate == 0) || w.sweepWatchDue() {
				sink++
			}
		}
		_ = sink
	})
}
