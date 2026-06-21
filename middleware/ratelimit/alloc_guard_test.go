//go:build !race

// Allocation guard for middleware/ratelimit. AllocsPerRun counts are only
// meaningful without the race detector (which adds bookkeeping allocations),
// so this file is excluded from -race builds.

package ratelimit

import (
	"context"
	"testing"
	"time"

	"github.com/goceleris/celeris/celeristest"
)

// TestAllowUnderLimitZeroAlloc locks in the under-limit fast path's
// zero-allocation property: when the request is allowed (the common case)
// and Burst is within the cached-int range, emitting the x-ratelimit-*
// headers resolves through the smallInts cache and SetHeaderTrust appends
// into the Context's inline header buffer — no per-request heap allocation.
func TestAllowUnderLimitZeroAlloc(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mw := New(Config{RPS: 1e9, Burst: 100, CleanupInterval: time.Hour, CleanupContext: ctx})
	opts := []celeristest.Option{celeristest.WithHeader("x-forwarded-for", "1.2.3.4")}

	// Warm the limiter bucket + pooled Context once so the steady-state
	// per-request cost is what AllocsPerRun measures.
	c, _ := celeristest.NewContext("GET", "/", opts...)
	_ = mw(c)
	celeristest.ReleaseContext(c)

	avg := testing.AllocsPerRun(500, func() {
		c, _ := celeristest.NewContext("GET", "/", opts...)
		_ = mw(c)
		celeristest.ReleaseContext(c)
	})
	if avg != 0 {
		t.Fatalf("under-limit fast path must not allocate: got %.2f allocs/op, want 0", avg)
	}
}
