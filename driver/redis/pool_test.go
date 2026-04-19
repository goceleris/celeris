package redis

import (
	"context"
	"sync"
	"testing"
)

func TestPoolConcurrency(t *testing.T) {
	c := newTestClient(t, nil, WithPoolSize(16))
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if err := c.Set(ctx, "k", "v", 0); err != nil {
					t.Error(err)
					return
				}
				if _, err := c.Get(ctx, "k"); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	stats := c.Stats()
	if stats.Open == 0 {
		t.Fatal("no conns opened")
	}
	if stats.InUse != 0 {
		t.Fatalf("in-use conns leaked: %d", stats.InUse)
	}
}

func TestPoolReleaseOnClose(t *testing.T) {
	c := newTestClient(t, nil)
	ctx := context.Background()
	_ = c.Set(ctx, "k", "v", 0)
	_ = c.Close()
	// After close, commands must fail.
	err := c.Ping(ctx)
	if err == nil {
		t.Fatal("expected error after close")
	}
}
