package async

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestPoolExhaustedErrorIncludesMaxOpen verifies that when the pool semaphore
// blocks until context expiry, the returned error wraps ctx.Err() AND includes
// the MaxOpen value for debuggability. Bug 9: previously, Acquire returned
// bare ctx.Err() with no pool context.
func TestPoolExhaustedErrorIncludesMaxOpen(t *testing.T) {
	cfg := PoolConfig{NumWorkers: 1, MaxOpen: 2}
	p := NewPool(cfg, func(ctx context.Context, w int) (*fakeConn, error) {
		return newFake(w), nil
	})
	defer p.Close()

	// Exhaust both slots.
	c1, err := p.Acquire(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := p.Acquire(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}

	// Third acquire should timeout and include MaxOpen in the error.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = p.Acquire(ctx, 0)
	if err == nil {
		t.Fatal("expected error when pool exhausted")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded in chain, got %v", err)
	}
	if !strings.Contains(err.Error(), "MaxOpen=2") {
		t.Fatalf("error missing MaxOpen context: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "celeris/pool") {
		t.Fatalf("error missing package prefix: %q", err.Error())
	}
	p.Release(c1)
	p.Release(c2)
}
