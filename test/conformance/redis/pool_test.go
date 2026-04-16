//go:build redis

package redis_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	celredis "github.com/goceleris/celeris/driver/redis"
)

// TestPool_WorkerAffinity smoke-tests that WithWorker(ctx, id) routes commands
// to a per-worker slot without erroring — the underlying pool accepts the hint
// and commands succeed for every ID in [0, NumWorkers).
func TestPool_WorkerAffinity(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "aff")
	cleanupKeys(t, c, key)

	if err := c.Set(ctx, key, "v", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// Route the same Get through worker 0, 1, ... up to 7 (the pool uses
	// round-robin when the hint is out of range).
	for w := 0; w < 8; w++ {
		got, err := c.Get(celredis.WithWorker(ctx, w), key)
		if err != nil {
			t.Fatalf("Get via worker %d: %v", w, err)
		}
		if got != "v" {
			t.Fatalf("Get via worker %d = %q, want v", w, got)
		}
	}
}

// TestPool_MaxOpenEnforcement verifies that PoolSize caps concurrent conns.
// We open a small pool, launch more than PoolSize goroutines that race to
// acquire, and confirm Open never exceeds the cap.
func TestPool_MaxOpenEnforcement(t *testing.T) {
	c := connectClient(t, celredis.WithPoolSize(2), celredis.WithMaxIdlePerWorker(1))
	// We don't have BLPOP exposed typed; emulate long work by using SET inside
	// goroutines that deliberately race. Instead, verify stats track inside
	// expectations.
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "cap")
	cleanupKeys(t, c, key)

	// Warm up two conns.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = c.Set(ctx, key, fmt.Sprintf("v%d", i), 0)
		}(i)
	}
	wg.Wait()

	stats := c.Stats()
	if stats.Open > 2 {
		t.Fatalf("stats.Open = %d, want <= 2 (PoolSize cap)", stats.Open)
	}
}

// TestPool_IdleCleanup forces a short MaxIdleTime and confirms that idle conns
// are reaped. The test just exercises the code path; it does not assert an
// exact Open count because eviction is async.
func TestPool_IdleCleanup(t *testing.T) {
	c := connectClient(t, celredis.WithMaxIdleTime(200*time.Millisecond))
	ctx := ctxWithTimeout(t, 5*time.Second)

	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	time.Sleep(400 * time.Millisecond)
	// A subsequent command must still succeed — the pool either reuses a warm
	// conn or dials a fresh one.
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping after idle: %v", err)
	}
}

// TestPool_Overflow confirms that when MaxOpen is exceeded, Acquire blocks
// until another goroutine releases. With the wait-queue, all goroutines
// eventually succeed — no ErrPoolExhausted is expected.
func TestPool_Overflow(t *testing.T) {
	c := connectClient(t, celredis.WithPoolSize(2))
	ctx := ctxWithTimeout(t, 10*time.Second)

	var wg sync.WaitGroup
	const N = 32
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Ping(ctx); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestPool_AcquireCanceled validates that a canceled context short-circuits
// the acquire path cleanly.
func TestPool_AcquireCanceled(t *testing.T) {
	c := connectClient(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := c.Ping(ctx)
	if err == nil {
		t.Fatalf("Ping with cancelled ctx = nil, want non-nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
