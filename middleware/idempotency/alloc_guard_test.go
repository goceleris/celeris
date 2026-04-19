//go:build !race

package idempotency

import (
	"context"
	"os"
	"sync/atomic"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/middleware/store"
)

// TestAllocBudgets tracks the allocation footprint of the idempotency
// hot paths. Logs by default; fails under TESTING_STRICT_ALLOC_BUDGETS=1.
func TestAllocBudgets(t *testing.T) {
	strict := os.Getenv("TESTING_STRICT_ALLOC_BUDGETS") == "1"

	type guard struct {
		name   string
		budget int64
		bench  func(b *testing.B)
	}

	guards := []guard{
		{
			// Replay of a completed entry: payload decode + response writer.
			// One allocation for the decoded response body, one for the
			// header slice.
			name:   "Replay",
			budget: 12,
			bench: func(b *testing.B) {
				kv := store.NewMemoryKV()
				defer kv.Close()

				var ran atomic.Int32
				handler := func(c *celeris.Context) error {
					ran.Add(1)
					return c.String(200, "created")
				}
				mw := New(Config{Store: kv})

				// Warm the entry so every iteration replays.
				opts := []celeristest.Option{
					celeristest.WithHeader("idempotency-key", "hot"),
					celeristest.WithHandlers(mw, handler),
				}
				ctx, _ := celeristest.NewContext("POST", "/", opts...)
				_ = ctx.Next()
				celeristest.ReleaseContext(ctx)

				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					ctx, _ := celeristest.NewContext("POST", "/", opts...)
					_ = ctx.Next()
					celeristest.ReleaseContext(ctx)
				}
			},
		},
		{
			// Concurrent duplicate path (follower returns 409): two
			// allocations for the HTTPError wrapping + one for the
			// cookie chain.
			name:   "Conflict",
			budget: 11,
			bench: func(b *testing.B) {
				kv := store.NewMemoryKV()
				defer kv.Close()

				_, _ = kv.SetNX(context.Background(), "locked", []byte{entryLocked}, 0)
				handler := func(c *celeris.Context) error {
					return c.String(200, "impossible")
				}
				mw := New(Config{Store: kv})
				opts := []celeristest.Option{
					celeristest.WithHeader("idempotency-key", "locked"),
					celeristest.WithHandlers(mw, handler),
				}

				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					ctx, _ := celeristest.NewContext("POST", "/", opts...)
					_ = ctx.Next()
					celeristest.ReleaseContext(ctx)
				}
			},
		},
	}

	for _, g := range guards {
		t.Run(g.name, func(t *testing.T) {
			result := testing.Benchmark(g.bench)
			got := result.AllocsPerOp()
			msg := "allocs/op=" + itoa(got) + " (budget " + itoa(g.budget) + ")"
			if got > g.budget {
				if strict {
					t.Fatalf("%s: exceeds budget — %s", g.name, msg)
				}
				t.Logf("%s: OVER BUDGET %s", g.name, msg)
			} else {
				t.Logf("%s: %s", g.name, msg)
			}
		})
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
