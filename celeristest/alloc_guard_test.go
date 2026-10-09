//go:build !race

// Allocation guard for celeristest. AllocsPerRun counts are only meaningful
// without the race detector (which adds bookkeeping allocations), so this
// file is excluded from -race builds.

package celeristest

import (
	"testing"

	"github.com/goceleris/celeris"
)

// TestNewContextZeroAlloc locks in that building a pooled Context costs no
// allocation, with or without a handler chain that fits the inline buffers
// (4 in celeristest's config, 8 in the Context). The middleware alloc
// guards (timeout, idempotency, overload, ...) and the ./middleware
// BenchmarkChain* numbers count from this floor. The chain reaches the root
// package through internal/testhooks.SetHandlers; a hook that takes the
// chain as one interface value boxes it and costs 2 allocs/op (celeris#938
// review).
func TestNewContextZeroAlloc(t *testing.T) {
	h := func(*celeris.Context) error { return nil }
	cases := []struct {
		name string
		opts []Option
	}{
		{"no options", nil},
		{"two handlers", []Option{WithHandlers(h, h)}},
		{"four handlers", []Option{WithHandlers(h, h, h, h)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Warm the pools once.
			ctx, _ := NewContext("GET", "/x", tc.opts...)
			ReleaseContext(ctx)

			avg := testing.AllocsPerRun(1000, func() {
				ctx, _ := NewContext("GET", "/x", tc.opts...)
				ReleaseContext(ctx)
			})
			if avg != 0 {
				t.Fatalf("NewContext+ReleaseContext: got %.2f allocs/op, want 0", avg)
			}
		})
	}
}
