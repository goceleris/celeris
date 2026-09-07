package store

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// TestMemoryKV_SweepNeverHoldsShardLockLong is the celeris#493 regression
// guard. The cleanup sweep used to range a whole shard under its lock; on the
// 2026-09-04 soak that held a multi-million-entry shard for seconds and every
// io_uring worker, calling Set inline from the session middleware, stalled
// behind it. The sweep must now release the lock every sweepBatch entries so a
// concurrent Set is never blocked for more than a few milliseconds.
//
// Worst case for the sweep: a large shard where nothing is expired, so every
// round scans a full batch and deletes nothing.
func TestMemoryKV_SweepNeverHoldsShardLockLong(t *testing.T) {
	if testing.Short() {
		t.Skip("fills a 600k-entry shard")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewMemoryKV(MemoryKVConfig{Shards: 1, CleanupInterval: time.Hour, CleanupContext: ctx})
	const n = 600_000
	far := time.Now().Add(time.Hour).UnixNano()
	s := &m.shards[0]
	s.mu.Lock()
	for i := 0; i < n; i++ {
		s.items["k"+strconv.Itoa(i)] = &memItem{value: []byte("v"), expiry: far}
	}
	s.mu.Unlock()

	// Concurrent writer: one Set every 200µs, recording the worst latency.
	var worst atomic.Int64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			t0 := time.Now()
			_ = m.Set(ctx, "w"+strconv.Itoa(i%64), []byte("x"), time.Hour)
			if d := time.Since(t0).Nanoseconds(); d > worst.Load() {
				worst.Store(d)
			}
			i++
			time.Sleep(200 * time.Microsecond)
		}
	}()
	time.Sleep(20 * time.Millisecond) // baseline
	sweepStart := time.Now()
	deleted := m.sweepShard(s, time.Now().UnixNano())
	sweepDur := time.Since(sweepStart)
	time.Sleep(20 * time.Millisecond)
	close(stop)
	<-done

	const bound = 15 * time.Millisecond
	t.Logf("sweep over %d live entries took %v, deleted %d; worst concurrent Set latency %v (bound %v)",
		n, sweepDur.Round(time.Microsecond), deleted, time.Duration(worst.Load()).Round(time.Microsecond), bound)
	if deleted != 0 {
		t.Fatalf("sweep deleted %d live entries", deleted)
	}
	if w := time.Duration(worst.Load()); w > bound {
		t.Fatalf("a concurrent Set waited %v behind the sweep (bound %v): the shard lock is held across too many entries", w, bound)
	}
}

// TestMemoryKV_SweepExpiresAcrossRounds checks the bounded sweep still expires
// a shard where most entries are stale: bounded rounds keep going while they
// find expired entries, so a single pass over a mostly-expired shard clears
// the bulk of it, and repeated passes converge on the rest.
func TestMemoryKV_SweepExpiresAcrossRounds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewMemoryKV(MemoryKVConfig{Shards: 1, CleanupInterval: time.Hour, CleanupContext: ctx})
	const n = 100_000
	past := time.Now().Add(-time.Hour).UnixNano()
	s := &m.shards[0]
	s.mu.Lock()
	for i := 0; i < n; i++ {
		s.items["e"+strconv.Itoa(i)] = &memItem{value: []byte("v"), expiry: past}
	}
	s.mu.Unlock()
	total := 0
	for pass := 0; pass < 8 && len(s.items) > 0; pass++ {
		total += m.sweepShard(s, time.Now().UnixNano())
	}
	if total != n || len(s.items) != 0 {
		t.Fatalf("expired entries left after 8 passes: deleted=%d remaining=%d", total, len(s.items))
	}
}
