//go:build !race

// Not under -race: the race detector inflates allocation counts and sync.Pool
// drops Puts at random there (the memcached driver keeps its allocation budgets
// out of -race for the same reason).

package redis

import (
	"context"
	"math"
	"testing"
	"time"
)

// What a command pays for its context on a direct conn: nothing for an ordinary
// command whatever the context (Background, WithCancel, WithTimeout), and for a
// blocking command under a context that can end context.AfterFunc's two
// allocations (the callback is built once per conn). The pin is the difference on
// the same conn, in turns.
func TestTheWatcherCostsNothingForAnOrdinaryCommandAndAtMostTwoAllocationsForABlockingOne(t *testing.T) {
	c, _ := dialDirectForWake(t)
	wc, cancel := context.WithCancel(context.Background())
	defer cancel()
	tc, cancel2 := context.WithTimeout(context.Background(), time.Hour)
	defer cancel2()
	get := func(ctx context.Context) func() {
		return func() {
			req, err := c.exec2(ctx, "GET", "k")
			if err != nil {
				t.Fatal(err)
			}
			c.releaseResult(req)
		}
	}
	blpop := func(ctx context.Context) func() {
		return func() {
			if err := blpopOn(ctx, t, c); err != nil {
				t.Fatal(err)
			}
		}
	}
	least := func(a, b func()) (float64, float64) {
		for i := 0; i < 50; i++ {
			a()
			b()
		}
		const rounds = 7
		am, bm := math.Inf(1), math.Inf(1)
		for r := range rounds {
			if r%2 == 0 {
				am = min(am, testing.AllocsPerRun(200, a))
				bm = min(bm, testing.AllocsPerRun(200, b))
			} else {
				bm = min(bm, testing.AllocsPerRun(200, b))
				am = min(am, testing.AllocsPerRun(200, a))
			}
		}
		return am, bm
	}
	bg, withCancel := least(get(context.Background()), get(wc))
	_, withTimeout := least(get(context.Background()), get(tc))
	t.Logf("GET on a direct conn: %.1f allocs/op with Background, %.1f with WithCancel, %.1f with WithTimeout", bg, withCancel, withTimeout)
	if withCancel != bg || withTimeout != bg {
		t.Errorf("an ordinary command costs allocations for its context: %.1f / %.1f against %.1f", withCancel, withTimeout, bg)
	}
	bbg, bcancel := least(blpop(context.Background()), blpop(wc))
	_, btimeout := least(blpop(context.Background()), blpop(tc))
	t.Logf("BLPOP on a direct conn: %.1f allocs/op with Background, %.1f with WithCancel, %.1f with WithTimeout", bbg, bcancel, btimeout)
	if bcancel-bbg > 2 {
		t.Errorf("a blocking command under a context that can be canceled costs %.1f allocations, want at most 2", bcancel-bbg)
	}
	if btimeout != bbg {
		t.Errorf("a blocking command under a context with a deadline costs %.1f allocations, want none", btimeout-bbg)
	}
	// A fresh context per command, through Do (the variadic path that looks at the
	// command): an ordinary command does not ask a cancel-only context for its Done
	// channel, which a stdlib cancelCtx makes on the first call.
	fresh := func(mk func() (context.Context, context.CancelFunc)) func() {
		return func() {
			ctx, cancel := mk()
			req, err := c.exec(ctx, "GET", "k")
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			c.releaseResult(req)
		}
	}
	fbg, fcancel := least(fresh(func() (context.Context, context.CancelFunc) { return context.Background(), func() {} }),
		fresh(func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) }))
	mk := testing.AllocsPerRun(200, func() {
		_, cancel := context.WithCancel(context.Background())
		cancel()
	})
	t.Logf("fresh context, Do(GET): %.1f allocs/op with Background, %.1f with WithCancel (making and canceling one is %.1f)", fbg, fcancel, mk)
	if fcancel-fbg > mk {
		t.Errorf("an ordinary command under a fresh WithCancel context allocates %.1f more than under Background; the context itself is %.1f", fcancel-fbg, mk)
	}
}
