package postgres

import (
	"context"
	"testing"
	"time"
)

// Nothing is armed for a context that cannot end; a context that can end arms
// one wake per statement and leaves none behind.
func TestWakeIsArmedOnlyForAContextWithADoneChannel(t *testing.T) {
	f := startSilentPG(t, 0)
	p := openDirectPool(t, f.addr)
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
