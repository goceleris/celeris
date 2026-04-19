package idempotency

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/middleware/store"
)

func runOnce(t *testing.T, mw, handler celeris.HandlerFunc, method, path string, opts ...celeristest.Option) *celeristest.ResponseRecorder {
	t.Helper()
	all := append(opts, celeristest.WithHandlers(mw, handler))
	ctx, rec := celeristest.NewContextT(t, method, path, all...)
	_ = ctx.Next()
	return rec
}

func TestFirstRequestStoresAndReplays(t *testing.T) {
	kv := store.NewMemoryKV()
	defer kv.Close()
	var calls atomic.Int32
	handler := func(c *celeris.Context) error {
		calls.Add(1)
		return c.String(201, "created-%d", calls.Load())
	}
	mw := New(Config{Store: kv})

	rec := runOnce(t, mw, handler, "POST", "/orders", celeristest.WithHeader("idempotency-key", "abc"))
	if rec.StatusCode != 201 {
		t.Fatalf("first call status: got %d want 201", rec.StatusCode)
	}
	if calls.Load() != 1 {
		t.Fatalf("first call: expected 1 handler run, got %d", calls.Load())
	}
	if got := string(rec.Body); got != "created-1" {
		t.Fatalf("first body: %q", got)
	}

	rec2 := runOnce(t, mw, handler, "POST", "/orders", celeristest.WithHeader("idempotency-key", "abc"))
	if rec2.StatusCode != 201 {
		t.Fatalf("replay status: got %d", rec2.StatusCode)
	}
	if string(rec2.Body) != "created-1" {
		t.Fatalf("replay body: %q", rec2.Body)
	}
	if calls.Load() != 1 {
		t.Fatalf("handler should not run on replay; calls=%d", calls.Load())
	}
}

func TestNoKeyHeaderPassesThrough(t *testing.T) {
	kv := store.NewMemoryKV()
	defer kv.Close()
	var calls atomic.Int32
	handler := func(c *celeris.Context) error {
		calls.Add(1)
		return c.String(200, "ok")
	}
	mw := New(Config{Store: kv})

	_ = runOnce(t, mw, handler, "POST", "/")
	_ = runOnce(t, mw, handler, "POST", "/")
	if calls.Load() != 2 {
		t.Fatalf("no key: expected 2 runs, got %d", calls.Load())
	}
}

func TestInvalidKeyRejected(t *testing.T) {
	kv := store.NewMemoryKV()
	defer kv.Close()
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	mw := New(Config{Store: kv, MaxKeyLength: 5})

	// Too long.
	rec := runOnce(t, mw, handler, "POST", "/", celeristest.WithHeader("idempotency-key", "toolong"))
	if rec.StatusCode != 400 {
		t.Fatalf("long key: got %d want 400", rec.StatusCode)
	}
	// Non-printable.
	rec2 := runOnce(t, mw, handler, "POST", "/", celeristest.WithHeader("idempotency-key", "a\x00b"))
	if rec2.StatusCode != 400 {
		t.Fatalf("control char: got %d want 400", rec2.StatusCode)
	}
}

func TestMethodFilter(t *testing.T) {
	kv := store.NewMemoryKV()
	defer kv.Close()
	var calls atomic.Int32
	handler := func(c *celeris.Context) error {
		calls.Add(1)
		return c.String(200, "ok")
	}
	mw := New(Config{Store: kv})

	// GET is not in default Methods — should pass through.
	_ = runOnce(t, mw, handler, "GET", "/", celeristest.WithHeader("idempotency-key", "k"))
	_ = runOnce(t, mw, handler, "GET", "/", celeristest.WithHeader("idempotency-key", "k"))
	if calls.Load() != 2 {
		t.Fatalf("GET should not be idempotent by default; calls=%d", calls.Load())
	}
}

func TestConcurrentDuplicateReturns409(t *testing.T) {
	kv := store.NewMemoryKV()
	defer kv.Close()
	var calls atomic.Int32
	ready := make(chan struct{})
	release := make(chan struct{})
	handler := func(c *celeris.Context) error {
		n := calls.Add(1)
		if n == 1 {
			// First caller waits on release so the rest pile up.
			close(ready)
			<-release
		}
		return c.String(200, "ok")
	}
	mw := New(Config{Store: kv})

	var wg sync.WaitGroup
	leaderDone := make(chan struct{})
	var conflictCount atomic.Int32

	// Leader.
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = runOnce(t, mw, handler, "POST", "/", celeristest.WithHeader("idempotency-key", "dup"))
		close(leaderDone)
	}()
	<-ready

	// Followers.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := runOnce(t, mw, handler, "POST", "/", celeristest.WithHeader("idempotency-key", "dup"))
			if rec.StatusCode == 409 {
				conflictCount.Add(1)
			}
		}()
	}

	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if conflictCount.Load() != 5 {
		t.Fatalf("expected 5 conflicts, got %d", conflictCount.Load())
	}
	if calls.Load() != 1 {
		t.Fatalf("handler should run once, ran %d times", calls.Load())
	}
	<-leaderDone
}

func TestBodyHashMismatch(t *testing.T) {
	kv := store.NewMemoryKV()
	defer kv.Close()
	handler := func(c *celeris.Context) error { return c.String(201, "ok") }
	mw := New(Config{Store: kv, BodyHash: true})

	rec1 := runOnce(t, mw, handler, "POST", "/",
		celeristest.WithHeader("idempotency-key", "bh"),
		celeristest.WithBody([]byte("payload-a")))
	if rec1.StatusCode != 201 {
		t.Fatalf("first: got %d", rec1.StatusCode)
	}
	// Replay with different body.
	rec2 := runOnce(t, mw, handler, "POST", "/",
		celeristest.WithHeader("idempotency-key", "bh"),
		celeristest.WithBody([]byte("payload-b")))
	if rec2.StatusCode != 422 {
		t.Fatalf("mismatch: got %d want 422", rec2.StatusCode)
	}
	// Replay with same body → 201 replay.
	rec3 := runOnce(t, mw, handler, "POST", "/",
		celeristest.WithHeader("idempotency-key", "bh"),
		celeristest.WithBody([]byte("payload-a")))
	if rec3.StatusCode != 201 {
		t.Fatalf("matching body replay: got %d", rec3.StatusCode)
	}
}

func TestLockExpiryAllowsRetry(t *testing.T) {
	kv := store.NewMemoryKV()
	defer kv.Close()
	var runs atomic.Int32
	handler := func(c *celeris.Context) error {
		if runs.Add(1) == 1 {
			// First invocation "hangs" by crashing — we simulate via
			// handler error to leave the lock in place.
			return celeris.NewHTTPError(503, "transient")
		}
		return c.String(201, "ok")
	}
	mw := New(Config{Store: kv, LockTimeout: 20 * time.Millisecond, TTL: time.Minute})

	_ = runOnce(t, mw, handler, "POST", "/", celeristest.WithHeader("idempotency-key", "retry"))
	time.Sleep(60 * time.Millisecond)
	rec := runOnce(t, mw, handler, "POST", "/", celeristest.WithHeader("idempotency-key", "retry"))
	if rec.StatusCode != 201 {
		t.Fatalf("after expiry: got %d", rec.StatusCode)
	}
}

func TestSkipPaths(t *testing.T) {
	kv := store.NewMemoryKV()
	defer kv.Close()
	var runs atomic.Int32
	handler := func(c *celeris.Context) error {
		runs.Add(1)
		return c.String(200, "ok")
	}
	mw := New(Config{Store: kv, SkipPaths: []string{"/skip"}})
	_ = runOnce(t, mw, handler, "POST", "/skip", celeristest.WithHeader("idempotency-key", "x"))
	_ = runOnce(t, mw, handler, "POST", "/skip", celeristest.WithHeader("idempotency-key", "x"))
	if runs.Load() != 2 {
		t.Fatalf("skip: expected 2 runs, got %d", runs.Load())
	}
}
