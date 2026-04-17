package memcached_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	celmc "github.com/goceleris/celeris/driver/memcached"
)

// ---------------------------------------------------------------------------
// celeris setup
// ---------------------------------------------------------------------------

func openCeleris(b *testing.B) *celmc.Client {
	b.Helper()
	c, err := celmc.NewClient(addr(b),
		celmc.WithMaxOpen(64),
		celmc.WithDialTimeout(5*time.Second),
		celmc.WithTimeout(3*time.Second),
	)
	if err != nil {
		b.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		_ = c.Close()
		b.Skipf("PING: %v", err)
	}
	b.Cleanup(func() { _ = c.Close() })
	return c
}

// populateCeleris seeds benchKeyCount short keys used by all read-only
// benchmarks. Only one driver writes — avoids head-to-head write pollution
// during a bench sweep that re-runs back to back.
func populateCeleris(b *testing.B, c *celmc.Client) []string {
	b.Helper()
	ctx := context.Background()
	keys := make([]string, benchKeyCount)
	for i := 0; i < benchKeyCount; i++ {
		keys[i] = fmt.Sprintf("%s:k:%d", benchKeyPrefix, i)
		if err := c.Set(ctx, keys[i], benchValue, 0); err != nil {
			b.Fatalf("Set: %v", err)
		}
	}
	return keys
}

// ---------------------------------------------------------------------------
// gomemcache setup
// ---------------------------------------------------------------------------

func openGoMemcache(b *testing.B) *memcache.Client {
	b.Helper()
	c := memcache.New(addr(b))
	c.MaxIdleConns = 64
	c.Timeout = 3 * time.Second
	if err := c.Ping(); err != nil {
		b.Skipf("gomemcache Ping: %v", err)
	}
	b.Cleanup(func() { _ = c.Close() })
	return c
}

// ---------------------------------------------------------------------------
// Celeris benchmarks
// ---------------------------------------------------------------------------

func BenchmarkGet_Celeris(b *testing.B) {
	c := openCeleris(b)
	keys := populateCeleris(b, c)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		_, _ = c.Get(ctx, keys[0])
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.Get(ctx, keys[0]); err != nil {
			b.Fatalf("Get: %v", err)
		}
	}
}

func BenchmarkSet_Celeris(b *testing.B) {
	c := openCeleris(b)
	ctx := context.Background()
	key := benchKeyPrefix + ":set"
	for i := 0; i < 10; i++ {
		_ = c.Set(ctx, key, benchValue, 0)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := c.Set(ctx, key, benchValue, 0); err != nil {
			b.Fatalf("Set: %v", err)
		}
	}
}

func BenchmarkGetMulti_10_Celeris(b *testing.B) {
	c := openCeleris(b)
	keys := populateCeleris(b, c)[:10]
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_, _ = c.GetMulti(ctx, keys...)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m, err := c.GetMulti(ctx, keys...)
		if err != nil {
			b.Fatalf("GetMulti: %v", err)
		}
		if len(m) != 10 {
			b.Fatalf("GetMulti returned %d entries, want 10", len(m))
		}
	}
}

func BenchmarkParallel_Get_Celeris(b *testing.B) {
	c := openCeleris(b)
	keys := populateCeleris(b, c)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := c.Get(ctx, keys[0]); err != nil {
				b.Fatalf("Get: %v", err)
			}
		}
	})
}

// BenchmarkPoolAcquireRelease_Celeris is the celeris-only equivalent of
// "acquire + noop + release" — Ping is the cheapest command.
func BenchmarkPoolAcquireRelease_Celeris(b *testing.B) {
	c := openCeleris(b)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		_ = c.Ping(ctx)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := c.Ping(ctx); err != nil {
			b.Fatalf("Ping: %v", err)
		}
	}
}

// ---------------------------------------------------------------------------
// gomemcache benchmarks — mirror the same workload on celeris-written keys.
// ---------------------------------------------------------------------------

// ensureGomemcacheReadOnlyKeysExist primes the read-only keys via celeris if
// gomemcache was somehow started before celeris had a chance to seed. It is
// a no-op when a previous celeris benchmark ran first, which is the typical
// case because alphabetical order puts BenchmarkGet_Celeris before
// BenchmarkGet_GoMemcache.
func ensureGomemcacheReadOnlyKeysExist(b *testing.B) []string {
	b.Helper()
	keys := make([]string, benchKeyCount)
	for i := 0; i < benchKeyCount; i++ {
		keys[i] = fmt.Sprintf("%s:k:%d", benchKeyPrefix, i)
	}
	// Peek: if the first key is missing, seed via celeris to keep the
	// "only one driver writes" invariant.
	g := openGoMemcache(b)
	if _, err := g.Get(keys[0]); err == memcache.ErrCacheMiss {
		c, err := celmc.NewClient(addr(b),
			celmc.WithMaxOpen(8),
			celmc.WithDialTimeout(5*time.Second))
		if err != nil {
			b.Fatalf("seed NewClient: %v", err)
		}
		defer func() { _ = c.Close() }()
		ctx := context.Background()
		for _, k := range keys {
			if err := c.Set(ctx, k, benchValue, 0); err != nil {
				b.Fatalf("seed Set: %v", err)
			}
		}
	}
	return keys
}

func BenchmarkGet_GoMemcache(b *testing.B) {
	g := openGoMemcache(b)
	keys := ensureGomemcacheReadOnlyKeysExist(b)
	for i := 0; i < 10; i++ {
		_, _ = g.Get(keys[0])
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := g.Get(keys[0]); err != nil {
			b.Fatalf("Get: %v", err)
		}
	}
}

func BenchmarkSet_GoMemcache(b *testing.B) {
	g := openGoMemcache(b)
	item := &memcache.Item{Key: benchKeyPrefix + ":set_gomemcache", Value: []byte(benchValue)}
	for i := 0; i < 10; i++ {
		_ = g.Set(item)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := g.Set(item); err != nil {
			b.Fatalf("Set: %v", err)
		}
	}
}

func BenchmarkGetMulti_10_GoMemcache(b *testing.B) {
	g := openGoMemcache(b)
	keys := ensureGomemcacheReadOnlyKeysExist(b)[:10]
	for i := 0; i < 5; i++ {
		_, _ = g.GetMulti(keys)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m, err := g.GetMulti(keys)
		if err != nil {
			b.Fatalf("GetMulti: %v", err)
		}
		if len(m) != 10 {
			b.Fatalf("GetMulti returned %d entries, want 10", len(m))
		}
	}
}

func BenchmarkParallel_Get_GoMemcache(b *testing.B) {
	g := openGoMemcache(b)
	keys := ensureGomemcacheReadOnlyKeysExist(b)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := g.Get(keys[0]); err != nil {
				b.Fatalf("Get: %v", err)
			}
		}
	})
}
