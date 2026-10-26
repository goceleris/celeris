//go:build linux

package iouring

import (
	"testing"
	"time"
)

// BenchmarkSweepGate884 is the sweep gate of Worker.run, one iteration's
// worth, in its two forms (celeris#884 items 65 and 75). "old" is the
// expression the iteration count alone made (tickCounter&gate == 0 ||
// sweepWatchDue), "new" is what replaced it: the batch's clock (an iteration
// with CQEs) or a fresh read (an idle one), sweepDue, and sweepWatchDue. The
// loop is never due, so this is the cost of not sweeping, which is what every
// iteration but a few pays. "new/fresh-clock" is the idle iteration's form.
func BenchmarkSweepGate884(b *testing.B) {
	newW := func() *Worker {
		w := &Worker{}
		w.lastSweep = time.Now().UnixNano()
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
	b.Run("new/fresh-clock", func(b *testing.B) {
		w := newW()
		gate := uint32(0x1F)
		var sink int
		for b.Loop() {
			w.tickCounter++
			if w.sweepDue(time.Now().UnixNano(), w.tickCounter&gate == 0) || w.sweepWatchDue() {
				sink++
			}
		}
		_ = sink
	})
}
