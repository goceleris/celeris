package cache

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/middleware/store"
)

func runOnce(t *testing.T, mw celeris.HandlerFunc, handler celeris.HandlerFunc, method, path string, opts ...celeristest.Option) *celeristest.ResponseRecorder {
	t.Helper()
	all := append(opts, celeristest.WithHandlers(mw, handler))
	ctx, rec := celeristest.NewContextT(t, method, path, all...)
	if err := ctx.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}
	return rec
}

func TestCacheHitMiss(t *testing.T) {
	kv := store.NewMemoryKV()
	defer kv.Close()

	var hits atomic.Int32
	handler := func(c *celeris.Context) error {
		hits.Add(1)
		return c.String(200, "body=%d", hits.Load())
	}
	mw := New(Config{Store: kv, TTL: time.Minute})

	rec := runOnce(t, mw, handler, "GET", "/foo")
	if rec.StatusCode != 200 {
		t.Fatalf("MISS: code=%d", rec.StatusCode)
	}
	if got := rec.Header("x-cache"); got != "MISS" {
		t.Fatalf("MISS header: got %q", got)
	}

	rec2 := runOnce(t, mw, handler, "GET", "/foo")
	if rec2.StatusCode != 200 {
		t.Fatalf("HIT: code=%d", rec2.StatusCode)
	}
	if got := rec2.Header("x-cache"); got != "HIT" {
		t.Fatalf("HIT header: got %q", got)
	}
	if hits.Load() != 1 {
		t.Fatalf("handler should have run once, ran %d times", hits.Load())
	}
}

func TestCacheMethodFilter(t *testing.T) {
	kv := store.NewMemoryKV()
	defer kv.Close()
	handler := func(c *celeris.Context) error { return c.String(201, "ok") }
	mw := New(Config{Store: kv})

	rec := runOnce(t, mw, handler, "POST", "/post")
	// POST is not in default Methods → pass-through, no cache header.
	if rec.Header("x-cache") != "" {
		t.Fatalf("expected no cache header on POST, got %q", rec.Header("x-cache"))
	}
}

func TestCacheStatusFilter(t *testing.T) {
	kv := store.NewMemoryKV()
	defer kv.Close()
	handler := func(c *celeris.Context) error { return c.String(404, "nope") }
	mw := New(Config{Store: kv})

	_ = runOnce(t, mw, handler, "GET", "/x")
	// Second call should still MISS (404 not cached).
	rec := runOnce(t, mw, handler, "GET", "/x")
	if got := rec.Header("x-cache"); got != "MISS" {
		t.Fatalf("404 should not cache; second request %q", got)
	}
}

func TestCacheControlMaxAgeCapsTTL(t *testing.T) {
	kv := store.NewMemoryKV()
	defer kv.Close()
	handler := func(c *celeris.Context) error {
		c.SetHeader("cache-control", "public, max-age=1")
		return c.String(200, "ok")
	}
	mw := New(Config{Store: kv, TTL: time.Hour})

	_ = runOnce(t, mw, handler, "GET", "/cc")
	rec := runOnce(t, mw, handler, "GET", "/cc")
	if got := rec.Header("x-cache"); got != "HIT" {
		t.Fatalf("expected HIT while within 1s max-age, got %q", got)
	}
	// Wait for the cap to elapse; entry should expire even though
	// cfg.TTL=1h.
	time.Sleep(1100 * time.Millisecond)
	rec2 := runOnce(t, mw, handler, "GET", "/cc")
	if got := rec2.Header("x-cache"); got != "MISS" {
		t.Fatalf("expected MISS after max-age cap elapsed, got %q", got)
	}
}

func TestCacheControlNoStore(t *testing.T) {
	kv := store.NewMemoryKV()
	defer kv.Close()
	handler := func(c *celeris.Context) error {
		c.SetHeader("cache-control", "no-store")
		return c.String(200, "ok")
	}
	mw := New(Config{Store: kv})

	_ = runOnce(t, mw, handler, "GET", "/ns")
	rec := runOnce(t, mw, handler, "GET", "/ns")
	if got := rec.Header("x-cache"); got != "MISS" {
		t.Fatalf("no-store should not cache; second request %q", got)
	}
}

func TestCacheMaxBodyBytes(t *testing.T) {
	kv := store.NewMemoryKV()
	defer kv.Close()
	big := make([]byte, 1024)
	for i := range big {
		big[i] = 'x'
	}
	handler := func(c *celeris.Context) error {
		return c.Blob(200, "text/plain", big)
	}
	mw := New(Config{Store: kv, MaxBodyBytes: 100})

	_ = runOnce(t, mw, handler, "GET", "/big")
	rec := runOnce(t, mw, handler, "GET", "/big")
	if got := rec.Header("x-cache"); got != "MISS" {
		t.Fatalf("big body should not cache; second request %q", got)
	}
}

func TestCacheVaryHeader(t *testing.T) {
	kv := store.NewMemoryKV()
	defer kv.Close()
	var calls atomic.Int32
	handler := func(c *celeris.Context) error {
		calls.Add(1)
		return c.String(200, "lang=%s", c.Header("accept-language"))
	}
	mw := New(Config{Store: kv, VaryHeaders: []string{"accept-language"}})

	_ = runOnce(t, mw, handler, "GET", "/", celeristest.WithHeader("accept-language", "en"))
	_ = runOnce(t, mw, handler, "GET", "/", celeristest.WithHeader("accept-language", "fr"))
	if calls.Load() != 2 {
		t.Fatalf("expected handler to run twice for distinct Vary values, got %d", calls.Load())
	}
	_ = runOnce(t, mw, handler, "GET", "/", celeristest.WithHeader("accept-language", "en"))
	if calls.Load() != 2 {
		t.Fatalf("repeated Vary=en should HIT; calls=%d", calls.Load())
	}
}

func TestSingleflightCoalesce(t *testing.T) {
	kv := store.NewMemoryKV()
	defer kv.Close()
	var calls atomic.Int32
	ready := make(chan struct{})
	handler := func(c *celeris.Context) error {
		calls.Add(1)
		<-ready
		return c.String(200, "hi")
	}
	mw := New(Config{Store: kv, Singleflight: true})

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = runOnce(t, mw, handler, "GET", "/sf")
		}()
	}
	// Let goroutines pile up, then release.
	time.Sleep(50 * time.Millisecond)
	close(ready)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("singleflight: expected 1 handler call, got %d", calls.Load())
	}
}

func TestInvalidateByKey(t *testing.T) {
	kv := store.NewMemoryKV()
	defer kv.Close()
	var calls atomic.Int32
	handler := func(c *celeris.Context) error {
		calls.Add(1)
		return c.String(200, "v=%d", calls.Load())
	}
	mw := New(Config{Store: kv})
	_ = runOnce(t, mw, handler, "GET", "/k")
	_ = runOnce(t, mw, handler, "GET", "/k")
	if calls.Load() != 1 {
		t.Fatalf("pre-Invalidate: expected 1 call, got %d", calls.Load())
	}
	if err := Invalidate(kv, "GET /k"); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	_ = runOnce(t, mw, handler, "GET", "/k")
	if calls.Load() != 2 {
		t.Fatalf("post-Invalidate: expected 2 calls, got %d", calls.Load())
	}
}

func TestInvalidatePrefix(t *testing.T) {
	kv := store.NewMemoryKV()
	defer kv.Close()
	var calls atomic.Int32
	handler := func(c *celeris.Context) error {
		calls.Add(1)
		return c.String(200, "ok")
	}
	mw := New(Config{Store: kv})
	_ = runOnce(t, mw, handler, "GET", "/api/a")
	_ = runOnce(t, mw, handler, "GET", "/api/b")
	_ = runOnce(t, mw, handler, "GET", "/other")
	base := calls.Load()

	if err := InvalidatePrefix(kv, "GET /api/"); err != nil {
		t.Fatalf("InvalidatePrefix: %v", err)
	}

	_ = runOnce(t, mw, handler, "GET", "/api/a")
	_ = runOnce(t, mw, handler, "GET", "/other")
	if delta := calls.Load() - base; delta != 1 {
		t.Fatalf("expected exactly /api/a to miss (+1 call), got delta=%d", delta)
	}
}

func TestInvalidatePrefixUnsupported(t *testing.T) {
	var s store.KV = nonPrefix{}
	if err := InvalidatePrefix(s, "x"); err == nil {
		t.Fatal("expected ErrNotSupported for KV without PrefixDeleter")
	}
}

type nonPrefix struct{}

func (nonPrefix) Get(context.Context, string) ([]byte, error) {
	return nil, store.ErrNotFound
}
func (nonPrefix) Set(context.Context, string, []byte, time.Duration) error { return nil }
func (nonPrefix) Delete(context.Context, string) error                      { return nil }

func TestMemoryStoreLRU(t *testing.T) {
	m := NewMemoryStore(MemoryStoreConfig{Shards: 1, MaxEntries: 2})
	defer m.Close()
	ctx := context.Background()
	_ = m.Set(ctx, "a", []byte("1"), 0)
	_ = m.Set(ctx, "b", []byte("2"), 0)
	// Touch a so b becomes LRU.
	_, _ = m.Get(ctx, "a")
	_ = m.Set(ctx, "c", []byte("3"), 0) // should evict b
	if _, err := m.Get(ctx, "b"); err == nil {
		t.Fatal("expected b to be evicted (LRU)")
	}
	if _, err := m.Get(ctx, "a"); err != nil {
		t.Fatal("a should survive (was touched)")
	}
	if _, err := m.Get(ctx, "c"); err != nil {
		t.Fatal("c should be present")
	}
}

func TestMemoryStoreTTL(t *testing.T) {
	m := NewMemoryStore()
	defer m.Close()
	ctx := context.Background()
	_ = m.Set(ctx, "k", []byte("v"), 20*time.Millisecond)
	time.Sleep(40 * time.Millisecond)
	if _, err := m.Get(ctx, "k"); err == nil {
		t.Fatal("expected TTL expiry")
	}
}

func TestKeyGeneratorDeterminism(t *testing.T) {
	kv := store.NewMemoryKV()
	defer kv.Close()
	mw := New(Config{Store: kv})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }

	// Query order should not matter for cache key.
	_ = runOnce(t, mw, handler, "GET", "/q", celeristest.WithQuery("a", "1"))
	rec := runOnce(t, mw, handler, "GET", "/q", celeristest.WithQuery("a", "1"))
	if got := rec.Header("x-cache"); got != "HIT" {
		t.Fatalf("same query → expected HIT, got %q", got)
	}
}

func TestSkipPaths(t *testing.T) {
	kv := store.NewMemoryKV()
	defer kv.Close()
	var calls atomic.Int32
	handler := func(c *celeris.Context) error {
		calls.Add(1)
		return c.String(200, "ok")
	}
	mw := New(Config{Store: kv, SkipPaths: []string{"/skip"}})
	_ = runOnce(t, mw, handler, "GET", "/skip")
	_ = runOnce(t, mw, handler, "GET", "/skip")
	if calls.Load() != 2 {
		t.Fatalf("SkipPaths: expected 2 handler calls, got %d", calls.Load())
	}
}

// scanKeys returns all keys in kv that begin with prefix. Used by tests
// that assert insertion.
func scanKeys(t *testing.T, kv store.KV, prefix string) []string {
	t.Helper()
	sc, ok := kv.(store.Scanner)
	if !ok {
		return nil
	}
	keys, err := sc.Scan(context.Background(), prefix)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	return keys
}

// TestConcurrentMemoryStore hammers the MemoryStore from many goroutines
// to validate -race cleanliness.
func TestConcurrentMemoryStore(t *testing.T) {
	m := NewMemoryStore(MemoryStoreConfig{Shards: 4})
	defer m.Close()
	ctx := context.Background()
	const N = 32
	var wg sync.WaitGroup
	for g := 0; g < N; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				k := fmt.Sprintf("k%d", i%16)
				_ = m.Set(ctx, k, []byte("v"), 0)
				_, _ = m.Get(ctx, k)
				_ = m.Delete(ctx, k)
			}
		}(g)
	}
	wg.Wait()
	_ = scanKeys(t, m, "")
}
