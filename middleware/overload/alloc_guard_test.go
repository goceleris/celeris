//go:build !race

package overload

import (
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/observe"
)

// TestAllocBudgets gates the overload-middleware hot path. Normal (the
// common case) must stay zero-alloc; Reject must allocate at most the
// HTTPError that comes back with the 503.
func TestAllocBudgets(t *testing.T) {
	strict := os.Getenv("TESTING_STRICT_ALLOC_BUDGETS") == "1"

	type guard struct {
		name   string
		budget int64
		bench  func(b *testing.B)
	}

	makeHandlerCtx := func(level float64) (celeris.HandlerFunc, *Controller, *fixedMonitor) {
		m := &fixedMonitor{}
		m.val.Store(level)
		col := observe.NewCollector()
		col.SetCPUMonitor(m)
		mw, ctrl := NewWithController(Config{
			CollectorProvider: func() *observe.Collector { return col },
			PollInterval:      2 * time.Millisecond,
			RetryAfter:        5 * time.Second,
		})
		// Wait for the first sample so the stage is populated.
		time.Sleep(10 * time.Millisecond)
		return mw, ctrl, m
	}

	guards := []guard{
		{
			name:   "Normal_PassThrough",
			budget: 4, // NewContext + handler response path; no overload allocs
			bench: func(b *testing.B) {
				mw, ctrl, _ := makeHandlerCtx(0.10)
				defer ctrl.Stop()
				var ran atomic.Int32
				handler := func(c *celeris.Context) error {
					ran.Add(1)
					return nil
				}
				opts := []celeristest.Option{
					celeristest.WithHandlers(mw, handler),
				}
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					ctx, _ := celeristest.NewContext("GET", "/normal", opts...)
					_ = ctx.Next()
					celeristest.ReleaseContext(ctx)
				}
			},
		},
		{
			name:   "Reject_503",
			budget: 5, // + AbortWithStatus allocates the HTTPError
			bench: func(b *testing.B) {
				mw, ctrl, _ := makeHandlerCtx(0.99)
				defer ctrl.Stop()
				handler := func(c *celeris.Context) error { return nil }
				opts := []celeristest.Option{
					celeristest.WithHandlers(mw, handler),
				}
				// Give the driver one more tick to enter Reject.
				time.Sleep(20 * time.Millisecond)
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					ctx, _ := celeristest.NewContext("GET", "/load", opts...)
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

type fixedMonitor struct {
	val atomic.Value // float64
}

func (m *fixedMonitor) Sample() (float64, error) {
	v, _ := m.val.Load().(float64)
	return v, nil
}
func (m *fixedMonitor) Close() error { return nil }

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
