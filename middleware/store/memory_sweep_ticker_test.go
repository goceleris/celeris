package store

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// TestMemoryKV_CleanupTickerDoesNotBlockSet drives the real cleanup ticker
// against a large single shard and asserts that a concurrent Set never waits
// long behind a sweep (celeris#493). Public-API shape so it doubles as the
// fail-first control against the unbounded sweep: there, each tick ranged all
// 1.2M entries under the shard lock and a Set waited for the whole range.
func TestMemoryKV_CleanupTickerDoesNotBlockSet(t *testing.T) {
	if testing.Short() {
		t.Skip("fills a 1.2M-entry shard and samples for 1s")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewMemoryKV(MemoryKVConfig{Shards: 1, CleanupInterval: 25 * time.Millisecond, CleanupContext: ctx})
	const n = 1_200_000
	far := time.Now().Add(time.Hour).UnixNano()
	s := &m.shards[0]
	s.mu.Lock()
	for i := 0; i < n; i++ {
		s.items["k"+strconv.Itoa(i)] = &memItem{value: []byte("v"), expiry: far}
	}
	s.mu.Unlock()

	var worst atomic.Int64
	deadline := time.Now().Add(1 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		t0 := time.Now()
		_ = m.Set(ctx, "w"+strconv.Itoa(i%64), []byte("x"), time.Hour)
		if d := time.Since(t0).Nanoseconds(); d > worst.Load() {
			worst.Store(d)
		}
		time.Sleep(100 * time.Microsecond)
	}
	const bound = 5 * time.Millisecond
	t.Logf("worst Set latency while the cleanup ticker swept a %d-entry shard every 25ms: %v (bound %v)",
		n, time.Duration(worst.Load()).Round(time.Microsecond), bound)
	if w := time.Duration(worst.Load()); w > bound {
		t.Fatalf("a Set waited %v behind the cleanup sweep (bound %v)", w, bound)
	}
}
