package sf

import (
	"context"
	"strconv"
	"testing"
	"time"
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
		_, _, _ = g.Do(context.Background(), strconv.Itoa(i), func() (int, time.Duration, error) { return i, 0, nil }, nil)
	}
}

// BenchmarkDoFollowerOfPublishedCall is a caller that arrives while then
// still holds the key, with a context that has a Done channel (std, HTTP/2,
// the timeout middleware): it takes the published result (celeris#888
// item 25).
func BenchmarkDoFollowerOfPublishedCall(b *testing.B) {
	g := New[int]()
	inThen, gate := make(chan struct{}), make(chan struct{})
	go func() {
		_, _, _ = g.Do(context.Background(), "k", func() (int, time.Duration, error) { return 7, 0, nil },
			func(int, error) { close(inThen); <-gate })
	}()
	<-inThen
	defer close(gate)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fn := func() (int, time.Duration, error) { return 0, 0, nil }
	b.ReportAllocs()
	for b.Loop() {
		_, _, _ = g.Do(ctx, "k", fn, nil)
	}
}
