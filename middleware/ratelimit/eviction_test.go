package ratelimit

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestIdleBucketsAreEvicted is the celeris#510 regression guard.
//
// The sweep predicate used to be `b.lastFill < expiry && b.tokens >= burst`.
// The token clause can never hold for a bucket driven through allow(): it is
// created with burst-1 tokens, refill clamps at burst and allow() then always
// decrements, and refill runs only inside allow() -- so an idle bucket is
// frozen at <= burst-1 forever and NOTHING was ever evicted. Combined with a
// default KeyFunc that falls back to RemoteAddr() (host:port) when no
// X-Forwarded-For is present, the map's cardinality was the process's lifetime
// connection count.
//
// This asserts the eviction is actually reachable. Before the fix it fails
// with every bucket still resident.
func TestIdleBucketsAreEvicted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// rps=100, burst=10 -> expiryDuration = max(10s, 2*10/100 s) = 10s.
	// A 50ms cleanup interval drives the REAL sweep goroutine rather than a
	// copy of its predicate, so this test fails if the shipped predicate is
	// unreachable -- which is precisely the defect.
	l := newShardedLimiter(ctx, 4, 100, 10, 50*time.Millisecond)

	const keys = 500
	// Stamp every bucket as last touched well past the expiry window: the
	// state a real idle client reaches.
	old := time.Now().Add(-30 * time.Second).UnixNano()
	for i := 0; i < keys; i++ {
		l.allow(fmt.Sprintf("10.0.%d.%d:%d", i/256%256, i%256, 40000+i), old)
	}
	if n := countBuckets(l); n != keys {
		t.Fatalf("setup: want %d buckets, got %d", keys, n)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if countBuckets(l) == 0 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Errorf("idle buckets must be evicted by the cleanup sweep, %d of %d still "+
		"resident after 5s: the sweep predicate is unreachable and the map grows "+
		"without bound", countBuckets(l), keys)
}

// TestEvictionPredicateIsReachable pins the specific defect: with the old
// predicate no bucket ever satisfied `tokens >= burst`, so the sweep was dead
// regardless of how idle a bucket became.
func TestEvictionPredicateIsReachable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := newShardedLimiter(ctx, 4, 100, 10, time.Hour)

	now := time.Now().UnixNano()
	for i := 0; i < 200; i++ {
		l.allow(fmt.Sprintf("k%d", i), now)
	}

	atBurst, maxTok := 0, 0.0
	for i := range l.shards {
		l.shards[i].mu.Lock()
		for _, b := range l.shards[i].buckets {
			if b.tokens > maxTok {
				maxTok = b.tokens
			}
			if b.tokens >= float64(l.burst) {
				atBurst++
			}
		}
		l.shards[i].mu.Unlock()
	}
	if atBurst != 0 {
		t.Fatalf("premise changed: %d buckets reached burst; the documented "+
			"reason the old predicate was dead no longer holds", atBurst)
	}
	t.Logf("confirmed: no bucket reaches burst=%d through allow() (highest %.2f), "+
		"which is why the old `tokens >= burst` clause made the sweep dead", l.burst, maxTok)
}

func countBuckets(l *shardedLimiter) int {
	n := 0
	for i := range l.shards {
		l.shards[i].mu.Lock()
		n += len(l.shards[i].buckets)
		l.shards[i].mu.Unlock()
	}
	return n
}
