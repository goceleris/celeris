package cache_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/middleware/cache"
)

// countingStore is a user's store in another package: it embeds
// *cache.MemoryStore and overrides Set (metrics, a write-through to a second
// tier, value compression...). Config.Store promises that any store other than
// MemoryStore itself gets plain Set calls for a fill, so the override must run
// (celeris#888 review of item 30: the ordered write of a fill must not be
// reachable through a method that embedding promotes).
type countingStore struct {
	*cache.MemoryStore
	sets atomic.Int32
}

func (s *countingStore) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	s.sets.Add(1)
	return s.MemoryStore.Set(ctx, key, value, ttl)
}

func TestCacheStoreEmbeddingMemoryStoreGetsItsOwnSet888(t *testing.T) {
	for _, noSF := range []bool{false, true} {
		name := "singleflight"
		if noSF {
			name = "no-singleflight"
		}
		t.Run(name, func(t *testing.T) {
			kv := &countingStore{MemoryStore: cache.NewMemoryStore()}
			defer kv.Close()
			mw := cache.New(cache.Config{Store: kv, TTL: time.Minute, DisableSingleflight: noSF})
			do := func() *celeristest.ResponseRecorder {
				c, rec := celeristest.NewContextT(t, "GET", "/w", celeristest.WithHandlers(mw, func(c *celeris.Context) error {
					return c.String(200, "v")
				}))
				if err := c.Next(); err != nil {
					t.Fatal(err)
				}
				return rec
			}
			if rec := do(); rec.Header("x-cache") != "MISS" {
				t.Fatalf("first request: x-cache %q, want MISS", rec.Header("x-cache"))
			}
			if n := kv.sets.Load(); n != 1 {
				t.Errorf("the wrapper's Set ran %d times for one fill, want 1 (the fill bypassed the override)", n)
			}
			if rec := do(); rec.Header("x-cache") != "HIT" {
				t.Errorf("second request: x-cache %q, want HIT", rec.Header("x-cache"))
			}
		})
	}
}
