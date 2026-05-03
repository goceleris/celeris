//go:build !race

// Allocation-budget guard for middleware/cache. See the driver/redis
// alloc_guard_test.go commentary for the rationale (logs by default,
// fails under TESTING_STRICT_ALLOC_BUDGETS=1).

package cache

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/goceleris/celeris/middleware/store"
)

func TestAllocBudgets(t *testing.T) {
	strict := os.Getenv("TESTING_STRICT_ALLOC_BUDGETS") == "1"

	type guard struct {
		name   string
		budget int64
		bench  func(b *testing.B)
	}

	hitPayload := store.EncodedResponse{
		Status:  200,
		Headers: [][2]string{{"content-type", "text/plain"}, {"x-cache", "HIT"}},
		Body:    []byte("hello"),
	}.Encode()

	guards := []guard{
		{
			// MemoryStore.Get: 1 defensive copy of the stored bytes +
			// 1 heap alloc for the returned slice.
			name:   "MemoryStore_Get",
			budget: 1,
			bench: func(b *testing.B) {
				m := NewMemoryStore()
				defer m.Close()
				ctx := context.Background()
				_ = m.Set(ctx, "k", hitPayload, 0)
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					_, _ = m.Get(ctx, "k")
				}
			},
		},
		{
			// MemoryStore.Set: defensive copy + lruNode.
			name:   "MemoryStore_Set",
			budget: 2,
			bench: func(b *testing.B) {
				m := NewMemoryStore()
				defer m.Close()
				ctx := context.Background()
				v := []byte("v")
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					_ = m.Set(ctx, "k", v, time.Minute)
				}
			},
		},
		{
			// Wire-format encode: one allocation for the output slice.
			name:   "EncodedResponse_Encode",
			budget: 1,
			bench: func(b *testing.B) {
				enc := store.EncodedResponse{
					Status:  200,
					Headers: [][2]string{{"content-type", "text/plain"}},
					Body:    []byte("ok"),
				}
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					_ = enc.Encode()
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
