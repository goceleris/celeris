package idempotency

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/middleware/store"
)

// TestPanickingHandlerReleasesItsLock921 is celeris#921's family in
// idempotency. A handler that returns an error releases the key's lock, so
// the client can retry; a handler that panicked left the lock until
// LockTimeout (30 s by default), and every retry in that window got 409
// Conflict, although celeris recovers the panic and the process lives on.
// The panic must reach the request unchanged, and the retry must run the
// handler.
func TestPanickingHandlerReleasesItsLock921(t *testing.T) {
	kv := store.NewMemoryKV()
	defer kv.Close()
	var calls atomic.Int32
	handler := func(c *celeris.Context) error {
		if calls.Add(1) == 1 {
			panic("handler panicked")
		}
		return c.String(201, "created")
	}
	mw := New(Config{Store: kv, LockTimeout: time.Hour})

	func() {
		defer func() {
			if p := recover(); p != "handler panicked" {
				t.Errorf("first request: recovered %v, want the handler's panic", p)
			}
		}()
		runOnce(t, mw, handler, "POST", "/orders", celeristest.WithHeader("idempotency-key", "k1"))
	}()

	rec := runOnce(t, mw, handler, "POST", "/orders", celeristest.WithHeader("idempotency-key", "k1"))
	if rec.StatusCode != 201 || string(rec.Body) != "created" {
		t.Errorf("retry after the panic: %d %q, want 201 \"created\" (the lock must be released)", rec.StatusCode, rec.Body)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("handler ran %d times, want 2", n)
	}
}
