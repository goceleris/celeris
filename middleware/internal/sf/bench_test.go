package sf

import (
	"strconv"
	"testing"
)

// BenchmarkDoMiss exercises the common single-request path: no other
// caller holds the key, so the leader runs fn and no waiter ever
// reads the result. Dominant path for per-key unique traffic (typical
// HTTP cache misses).
func BenchmarkDoMiss(b *testing.B) {
	g := New[int]()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		// Unique key each iter keeps the leader path hot without
		// accumulating state in g.calls.
		_, _, _ = g.Do(strconv.Itoa(i), func() (int, error) { return i, nil })
	}
}
