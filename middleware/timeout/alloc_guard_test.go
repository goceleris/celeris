//go:build !race

// Allocation guard for middleware/timeout. AllocsPerRun counts are only
// meaningful without the race detector (which adds bookkeeping allocations),
// so this file is excluded from -race builds.

package timeout

import (
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// TestNoTimeoutCommonPathSingleAlloc locks in the cooperative middleware's
// common-path allocation floor: a synchronous handler that returns inside the
// deadline without ever observing ctx.Done() allocates exactly one object —
// the lazyDeadlineCtx struct that wraps the request context. The eager
// context.WithTimeout path it replaced cost ~8 allocations (timerCtx, cancel
// closure, scheduled timer, ...). The lazy struct itself cannot be pooled
// because the "ctx held past middleware return" contract (see
// TestContextCancelCalled) requires a retained handle to keep observing this
// request's cancellation after release — a reused instance would expose the
// next request's state to that handle. So one alloc is the structural floor;
// this test fails loudly if a refactor reintroduces the eager timer path.
func TestNoTimeoutCommonPathSingleAlloc(t *testing.T) {
	mw := New(Config{Timeout: 5 * time.Second})
	noop := func(_ *celeris.Context) error { return nil }

	// Warm the pooled Context once.
	c, _ := celeristest.NewContext("GET", "/", celeristest.WithHandlers(mw, noop))
	_ = c.Next()
	celeristest.ReleaseContext(c)

	avg := testing.AllocsPerRun(500, func() {
		c, _ := celeristest.NewContext("GET", "/", celeristest.WithHandlers(mw, noop))
		_ = c.Next()
		celeristest.ReleaseContext(c)
	})
	if avg > 1 {
		t.Fatalf("no-timeout common path regressed past the lazy-ctx floor: got %.2f allocs/op, want <=1", avg)
	}
}
