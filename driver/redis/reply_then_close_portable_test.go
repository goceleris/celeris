//go:build !linux

package redis

// The strict variants of the reply-then-close tests: where the portable loop
// is the path (macOS, Windows), a reply delivered whole and then a close at
// once is reported as the real result with a nil error, and a null reply
// before the close keeps each command's result. The event loop (Linux) does
// not guarantee this (celeris#1460, celeris#1465): see reply_then_close_test.go.

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestReplyThenCloseReportsTheRealResult(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Run("pipeline", func(t *testing.T) {
		c := newCloseAfterReplyClient(t)
		for i := 0; i < 40; i++ {
			p := c.Pipeline()
			a := p.Get("a")
			b := p.Get("b")
			l := p.Get("last")
			if err := p.Exec(ctx); err != nil {
				if i == 0 {
					t.Fatalf("Exec of the call whose reply was delivered whole = %v, want nil", err)
				}
				p.Release()
				continue
			}
			for name, g := range map[string]*StringCmd{"a": a, "b": b, "last": l} {
				if v, err := g.Result(); err != nil || v != "val-"+name {
					t.Fatalf("round %d: %s = %q, %v", i, name, v, err)
				}
			}
			p.Release()
		}
	})

	t.Run("single command", func(t *testing.T) {
		c := newCloseAfterReplyClient(t)
		for i := 0; i < 40; i++ {
			v, err := c.Get(ctx, "last")
			if err != nil {
				if i == 0 {
					t.Fatalf("Get of the call whose reply was delivered whole = %v, want nil", err)
				}
				continue
			}
			if v != "val-last" {
				t.Fatalf("round %d: Get = %q", i, v)
			}
		}
	})

	t.Run("tx", func(t *testing.T) {
		c := newCloseAfterReplyClient(t)
		for i := 0; i < 40; i++ {
			tx, err := c.TxPipeline(ctx)
			if err == nil {
				s := tx.Set("k", "v", 0)
				if err = tx.Exec(ctx); err == nil {
					if v, e := s.Result(); e != nil || v != "OK" {
						t.Fatalf("round %d: Set = %q, %v", i, v, e)
					}
					continue
				}
			}
			if i == 0 {
				t.Fatalf("Tx of the call whose reply was delivered whole = %v, want nil", err)
			}
		}
	})
}

func TestReplyThenCloseWithANullReplyKeepsEachResult(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := newCloseAfterReplyClient(t)
	for i := 0; i < 40; i++ {
		p := c.Pipeline()
		a := p.Get("aaaa")
		m := p.Get("nilclose")
		err := p.Exec(ctx)
		if i > 0 && !errors.Is(err, ErrNil) {
			p.Release() // the pool handed out the closed connection: the loss, not a result
			continue
		}
		if !errors.Is(err, ErrNil) {
			t.Fatalf("Exec = %v, want ErrNil (the null reply)", err)
		}
		if v, e := a.Result(); e != nil || v != "val-aaaa" {
			t.Fatalf("round %d: aaaa = %q, %v; want val-aaaa, nil", i, v, e)
		}
		if v, e := m.Result(); !errors.Is(e, ErrNil) || v != "" {
			t.Fatalf("round %d: the null reply = %q, %v; want \"\", ErrNil", i, v, e)
		}
		p.Release()
	}
}
