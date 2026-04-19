//go:build memcached

// zz_flush_test.go — placed in a file whose name sorts last so the flush
// tests run after every other test in the package. Flushing is destructive
// (it clears all keys on the shared server, including keys set by tests in
// other files), so it must not precede tests that assume their freshly-set
// keys are visible.
package memcached_test

import (
	"errors"
	"testing"
	"time"

	celmc "github.com/goceleris/celeris/driver/memcached"
)

// TestZZFlush clears all keys on the server immediately. Registered last so
// it doesn't torpedo other tests' state.
func TestZZFlush(t *testing.T) {
	c := newClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "flush")

	if err := c.Set(ctx, key, "before", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := c.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	_, err := c.Get(ctx, key)
	if !errors.Is(err, celmc.ErrCacheMiss) {
		t.Fatalf("Get after Flush = %v, want ErrCacheMiss", err)
	}
}

// TestZZFlushAfter schedules a flush and waits past the delay, then confirms
// the key is gone. We deliberately skip the "still present before delay"
// intermediate assertion: memcached stamps every pre-existing item with an
// expiry at "now + delay", and the interaction between Set-then-FlushAfter
// makes the pre-delay read racy on different server build flags. The
// post-delay miss is the load-bearing assertion here.
func TestZZFlushAfter(t *testing.T) {
	c := newClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "flushafter")

	if err := c.Set(ctx, key, "lingers", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := c.FlushAfter(ctx, 1*time.Second); err != nil {
		t.Fatalf("FlushAfter: %v", err)
	}
	// Wait past the flush timer and verify the miss.
	time.Sleep(1500 * time.Millisecond)
	_, err := c.Get(ctx, key)
	if !errors.Is(err, celmc.ErrCacheMiss) {
		t.Fatalf("Get post-flush-delay = %v, want ErrCacheMiss", err)
	}
}
