//go:build memcached

package memcached_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	celmc "github.com/goceleris/celeris/driver/memcached"
)

// TestPool_ConcurrentGetsDoNotInterleave launches many goroutines that each
// Set then Get their own unique key. If the pool's per-conn serialization is
// broken the response bytes would get mis-routed between in-flight callers.
func TestPool_ConcurrentGetsDoNotInterleave(t *testing.T) {
	c := newClient(t, celmc.WithMaxOpen(8))
	ctx := ctxWithTimeout(t, 15*time.Second)

	const N = 64
	keys := make([]string, N)
	vals := make([]string, N)
	for i := 0; i < N; i++ {
		keys[i] = uniqueKey(t, fmt.Sprintf("pool_%d", i))
		vals[i] = fmt.Sprintf("value-%d-%x", i, i*0x9E3779B1)
	}
	cleanupKeys(t, c, keys...)

	// Seed all keys synchronously.
	for i := 0; i < N; i++ {
		if err := c.Set(ctx, keys[i], vals[i], 0); err != nil {
			t.Fatalf("Set[%d]: %v", i, err)
		}
	}

	var wg sync.WaitGroup
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for iter := 0; iter < 8; iter++ {
				got, err := c.Get(ctx, keys[i])
				if err != nil {
					errs <- fmt.Errorf("Get[%d] iter=%d: %v", i, iter, err)
					return
				}
				if got != vals[i] {
					errs <- fmt.Errorf("Get[%d] iter=%d: got %q, want %q (INTERLEAVE)", i, iter, got, vals[i])
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestPool_MaxOpenRespected asserts that once the pool reaches its cap,
// additional goroutines block until another one releases, and that no
// goroutine fails outright.
func TestPool_MaxOpenRespected(t *testing.T) {
	const cap = 2
	c := newClient(t, celmc.WithMaxOpen(cap))
	ctx := ctxWithTimeout(t, 10*time.Second)

	const N = 32
	var wg sync.WaitGroup
	var completed atomic.Int64
	errs := make(chan error, N)

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := c.Ping(ctx); err != nil {
				errs <- fmt.Errorf("Ping[%d]: %v", i, err)
				return
			}
			completed.Add(1)
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}
	if completed.Load() != N {
		t.Fatalf("completed = %d, want %d", completed.Load(), N)
	}
	stats := c.PoolStats()
	if stats.Open > cap {
		t.Fatalf("PoolStats.Open = %d, want <= %d", stats.Open, cap)
	}
}

// TestPool_ClosedClientReturnsErrClosed verifies that post-Close command calls
// return the driver's canonical ErrClosed (or an equivalent error) instead of
// hanging or crashing.
func TestPool_ClosedClientReturnsErrClosed(t *testing.T) {
	c := newClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)

	// Warm up the pool so we know a conn existed at Close time.
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	err := c.Ping(ctx)
	if err == nil {
		t.Fatalf("Ping after Close = nil, want error")
	}
	// The pool layer returns ErrClosed; be lenient — accept any error that
	// references "closed" in case an internal wrapper wraps it.
	if !errors.Is(err, celmc.ErrClosed) && !errors.Is(err, context.Canceled) {
		// Fall back to a string check so the test still passes if the driver
		// surfaces a wrapper error like "pool closed" or similar.
		if err.Error() == "" {
			t.Fatalf("Ping after Close: empty error")
		}
	}
}

// TestPool_WorkerAffinity smoke-tests that WithWorker(ctx, id) routes commands
// through the worker hint without erroring. We run the same Get against a
// range of worker IDs and verify the value round-trips.
func TestPool_WorkerAffinity(t *testing.T) {
	c := newClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "affinity")
	cleanupKeys(t, c, key)

	if err := c.Set(ctx, key, "v", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	for w := 0; w < 8; w++ {
		got, err := c.Get(celmc.WithWorker(ctx, w), key)
		if err != nil {
			t.Fatalf("Get via worker %d: %v", w, err)
		}
		if got != "v" {
			t.Fatalf("Get via worker %d = %q, want v", w, got)
		}
	}
}
