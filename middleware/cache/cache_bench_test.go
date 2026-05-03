package cache

import (
	"strconv"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// BenchmarkCacheHit covers the HIT path — the key exists in the
// MemoryStore, the middleware decodes and replays without running the
// downstream handler. Dominant path for highly-cacheable endpoints
// (catalog / config / static HTML).
func BenchmarkCacheHit(b *testing.B) {
	store := NewMemoryStore()
	defer store.Close()
	mw := New(Config{
		Store:        store,
		TTL:          5 * time.Minute,
		Singleflight: false,
	})
	handler := func(c *celeris.Context) error {
		return c.Blob(200, "text/plain", []byte("hello world"))
	}
	// Prime the cache.
	opts := []celeristest.Option{celeristest.WithHandlers(mw, handler)}
	ctx, _ := celeristest.NewContext("GET", "/api/users", opts...)
	_ = ctx.Next()
	celeristest.ReleaseContext(ctx)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		c, _ := celeristest.NewContext("GET", "/api/users", opts...)
		_ = c.Next()
		celeristest.ReleaseContext(c)
	}
}

// BenchmarkCacheMissSingleflight covers the common MISS path with
// singleflight enabled (the default). Each request uses a unique key
// so the leader always runs and the sf.Group entry is never observed
// by a follower — the pool-recycle path landed in R78.
func BenchmarkCacheMissSingleflight(b *testing.B) {
	store := NewMemoryStore()
	defer store.Close()
	// Key generator returns a fresh key per call so every request is a
	// unique-key MISS (not a Store hit, not a follower).
	var counter int
	mw := New(Config{
		Store:        store,
		TTL:          time.Hour,
		Singleflight: true,
		KeyGenerator: func(_ *celeris.Context) string {
			counter++
			return "k" + strconv.Itoa(counter)
		},
	})
	handler := func(c *celeris.Context) error {
		return c.Blob(200, "text/plain", []byte("hello"))
	}
	opts := []celeristest.Option{celeristest.WithHandlers(mw, handler)}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		c, _ := celeristest.NewContext("GET", "/api/users", opts...)
		_ = c.Next()
		celeristest.ReleaseContext(c)
	}
}

// BenchmarkCacheMissNoSingleflight is the same miss path without
// singleflight. Direct-store-write only, no sf.Group allocation.
func BenchmarkCacheMissNoSingleflight(b *testing.B) {
	store := NewMemoryStore()
	defer store.Close()
	var counter int
	mw := New(Config{
		Store:        store,
		TTL:          time.Hour,
		Singleflight: false,
		KeyGenerator: func(_ *celeris.Context) string {
			counter++
			return "k" + strconv.Itoa(counter)
		},
	})
	handler := func(c *celeris.Context) error {
		return c.Blob(200, "text/plain", []byte("hello"))
	}
	opts := []celeristest.Option{celeristest.WithHandlers(mw, handler)}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		c, _ := celeristest.NewContext("GET", "/api/users", opts...)
		_ = c.Next()
		celeristest.ReleaseContext(c)
	}
}
