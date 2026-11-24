package postgres

import (
	"context"
	"testing"
	"time"
)

// Nothing is armed for a context that cannot end; a context that can end arms
// one wake per statement and leaves none behind. Each case has a pool, and so a
// conn, of its own: the wake is built once per conn, so a shared conn would make
// every case after the first pass on the first one's work.
func TestWakeIsArmedOnlyForAContextWithADoneChannel(t *testing.T) {
	type kv struct{}
	for _, tc := range []struct {
		name     string
		mk       func() (context.Context, context.CancelFunc)
		wantWake bool
	}{
		{"background", func() (context.Context, context.CancelFunc) { return context.Background(), func() {} }, false},
		{"background with a value", func() (context.Context, context.CancelFunc) {
			return context.WithValue(context.Background(), kv{}, 1), func() {}
		}, false},
		{"cancel only", func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) }, true},
		{"deadline", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), time.Hour)
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := startSilentPG(t, 0)
			p := openDirectPool(t, f.addr)
			ctx, cancel := tc.mk()
			defer cancel()
			if _, err := p.ExecContext(ctx, "SELECT 1"); err != nil {
				t.Fatal(err)
			}
			c, err := p.acquire(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer p.release(c)
			if got := c.wakeFn != nil; got != tc.wantWake {
				t.Fatalf("wake built = %v, want %v", got, tc.wantWake)
			}
			if c.wakeStop != nil {
				t.Fatal("a wake is still armed after the statement")
			}
		})
	}
}

// What a statement pays: a context without a Done channel allocates exactly what
// a context with a value does (nothing for the wake), and a context that can end
// allocates more (the registration).
func TestAContextWithoutADoneChannelAllocatesNoWake(t *testing.T) {
	f := startSilentPG(t, 0)
	p := openDirectPool(t, f.addr)
	run := func(ctx context.Context) func() {
		return func() {
			if _, err := p.ExecContext(ctx, "SELECT 1"); err != nil {
				t.Fatal(err)
			}
		}
	}
	type kv struct{}
	run(context.Background())() // dial, warm the pools
	bg := testing.AllocsPerRun(200, run(context.Background()))
	val := testing.AllocsPerRun(200, run(context.WithValue(context.Background(), kv{}, 1)))
	cctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelOnly := testing.AllocsPerRun(200, run(cctx))
	if val > bg+1 { // the WithValue context itself is allocated once, outside the loop
		t.Fatalf("allocs per statement: background %v, with a value %v: a context without Done must cost the same", bg, val)
	}
	if cancelOnly <= bg {
		t.Fatalf("allocs per statement: background %v, cancellable %v: the registration is not visible, the test no longer measures the wake", bg, cancelOnly)
	}
}

// A context that ends after the statement finished must not leave a deadline in
// the past on the socket: the next statement on the conn works.
func TestAWakeThatRacesTheEndOfTheStatementLeavesNothingBehind(t *testing.T) {
	f := startSilentPG(t, 0)
	p := openDirectPool(t, f.addr)
	for i := range 300 {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(time.Duration(i%7) * 100 * time.Microsecond)
			cancel()
		}()
		_, _ = p.ExecContext(ctx, "SELECT 1") // may or may not be cut by the cancel
		cancel()
		if _, err := p.ExecContext(context.Background(), "SELECT 2"); err != nil {
			t.Fatalf("iteration %d: the next statement failed: %v", i, err)
		}
	}
}
