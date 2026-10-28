//go:build !race

package postgres

import (
	"testing"
	"time"
)

// celeris#1104. The Query_1col_1row guard's budget is a number for one path:
// the sync fast path, where the poll on the caller's goroutine takes the whole
// reply. It takes the whole reply only if the reply is there when the poll
// reads, and the fake backend wrote it as four separate segments, so whether it
// was there was a race between the backend's four writes and the poll. A poll
// that reads the reply in pieces falls through to the async wait (simpleQuery
// allocates colsCh there, "Partial or EAGAIN") and the query costs one
// allocation more, for as long as the pieces keep arriving that way: that is
// 15 against the budget of 14, with syncLoop set (the budget follows
// syncLoop). The guard must not depend on that race.
func TestQuery1col1rowGuardDoesNotDependOnTheReplyArrivingInPieces1104(t *testing.T) {
	for _, gap := range []time.Duration{0, 300 * time.Microsecond} {
		t.Run(gap.String(), func(t *testing.T) {
			fakePGWriteGap.Store(int64(gap))
			t.Cleanup(func() { fakePGWriteGap.Store(0) })
			res := testing.Benchmark(benchQuery1col1row)
			// The budget of the path the bench's conn took, as the guard has it.
			budget := int64(15)
			if query1col1rowSyncPath.Load() {
				budget = 14
			}
			got := res.AllocsPerOp()
			t.Logf("gap %v: allocs/op=%d (budget %d) -- %s", gap, got, budget, res)
			if got > budget {
				t.Errorf("allocs/op=%d, budget %d: the guard's count depends on how the backend's writes were scheduled", got, budget)
			}
		})
	}
}
