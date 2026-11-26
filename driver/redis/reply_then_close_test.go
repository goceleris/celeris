package redis

// A reply delivered whole and the connection closed right behind it (no
// pause), and a null reply before a close or a drop. The event-loop path
// (the standalone client on Linux) does not guarantee that the call whose
// reply arrived whole reports it with a nil error: a FIN right behind the
// reply can make the call return "file descriptor not registered" instead
// (celeris#1460, celeris#1465, v1.6.1). So the tests here assert what every
// path guarantees: a nil error always carries the command's own value, a
// failure is the connection's error and never a null reply's ErrNil, and no
// command takes another command's result. The strict variants (a delivered
// reply reports its real result with a nil error) are in
// reply_then_close_portable_test.go, which builds where the portable loop
// is the path.

import (
	"bufio"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// closeAfterReply serves GET (the key "last" closes the connection right
// after its reply was flushed; "nil" is a null reply, "nilclose" a null reply
// then the connection closes, "stall" no reply and the connection drops), SET,
// and MULTI/EXEC with one queued command (EXEC's reply is flushed and the
// connection closed).
func closeAfterReply(t *testing.T) *fakeRedis {
	var fp atomic.Pointer[fakeRedis]
	closeAll := func(w *bufio.Writer) {
		_ = w.Flush()
		f := fp.Load()
		f.mu.Lock()
		conns := f.conns
		f.mu.Unlock()
		for _, cn := range conns {
			_ = cn.Close()
		}
	}
	f := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		switch strings.ToUpper(cmd[0]) {
		case "HELLO":
			handleHELLO(w, 3)
		case "GET":
			switch cmd[1] {
			case "nil":
				_, _ = w.WriteString("$-1\r\n")
			case "nilclose":
				_, _ = w.WriteString("$-1\r\n")
				closeAll(w)
			case "stall":
				closeAll(w)
			default:
				writeBulk(w, "val-"+cmd[1])
				if cmd[1] == "last" {
					closeAll(w)
				}
			}
		case "MULTI":
			writeSimple(w, "OK")
		case "SET":
			writeSimple(w, "QUEUED")
		case "EXEC":
			writeArrayHeader(w, 1)
			writeSimple(w, "OK")
			closeAll(w)
		}
	})
	fp.Store(f)
	return f
}

func newCloseAfterReplyClient(t *testing.T) *Client {
	fake := closeAfterReply(t)
	c, err := NewClient(fake.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestReplyThenCloseNeverReportsAWrongResult: whatever the call reports, a
// nil error carries the command's own value.
func TestReplyThenCloseNeverReportsAWrongResult(t *testing.T) {
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
				for name, g := range map[string]*StringCmd{"a": a, "b": b, "last": l} {
					if v, e := g.Result(); e == nil || v != "" {
						t.Fatalf("round %d: Exec failed (%v) but %s = %q, %v", i, err, name, v, e)
					}
				}
				p.Release()
				continue
			}
			for name, g := range map[string]*StringCmd{"a": a, "b": b, "last": l} {
				if v, e := g.Result(); e != nil || v != "val-"+name {
					t.Fatalf("round %d: %s = %q, %v", i, name, v, e)
				}
			}
			p.Release()
		}
	})

	t.Run("single command", func(t *testing.T) {
		c := newCloseAfterReplyClient(t)
		for i := 0; i < 40; i++ {
			if v, err := c.Get(ctx, "last"); err == nil && v != "val-last" {
				t.Fatalf("round %d: Get = %q, nil", i, v)
			}
		}
	})

	t.Run("tx", func(t *testing.T) {
		c := newCloseAfterReplyClient(t)
		for i := 0; i < 40; i++ {
			tx, err := c.TxPipeline(ctx)
			if err != nil {
				continue
			}
			s := tx.Set("k", "v", 0)
			if err := tx.Exec(ctx); err == nil {
				if v, e := s.Result(); e != nil || v != "OK" {
					t.Fatalf("round %d: Set = %q, %v", i, v, e)
				}
			}
		}
	})
}

// TestNullReplyThenCloseNeverMixesResults: all replies arrive, the last a null,
// then the server closes. Either the batch completed (Exec reports the null
// reply's ErrNil, every command its own result) or it failed (Exec reports a
// connection error that is not ErrNil, and so does every command). A command
// that got a real value never reports ErrNil.
func TestNullReplyThenCloseNeverMixesResults(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := newCloseAfterReplyClient(t)
	for i := 0; i < 40; i++ {
		p := c.Pipeline()
		a := p.Get("aaaa")
		m := p.Get("nilclose")
		err := p.Exec(ctx)
		av, ae := a.Result()
		mv, me := m.Result()
		switch {
		case errors.Is(err, ErrNil):
			if ae != nil || av != "val-aaaa" || !errors.Is(me, ErrNil) || mv != "" {
				t.Fatalf("round %d: Exec = ErrNil but aaaa = %q, %v; null = %q, %v", i, av, ae, mv, me)
			}
		case err != nil:
			if ae == nil || errors.Is(ae, ErrNil) || me == nil || errors.Is(me, ErrNil) || av != "" || mv != "" {
				t.Fatalf("round %d: Exec failed (%v) but aaaa = %q, %v; null = %q, %v", i, err, av, ae, mv, me)
			}
		default:
			t.Fatalf("round %d: Exec = nil for a batch ending in a null reply", i)
		}
		p.Release()
	}
}

// A null reply, then the connection drops before the last reply: the command
// that never got a reply must not report the null reply's ErrNil ("key does not
// exist"); every command reports the connection's error, and so does Exec.
func TestNullReplyThenDropReportsTheLossToEveryCommand(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := newCloseAfterReplyClient(t)
	p := c.Pipeline()
	defer p.Release()
	a := p.Get("aaaa")
	m := p.Get("nil")
	s := p.Get("stall")
	err := p.Exec(ctx)
	if err == nil || errors.Is(err, ErrNil) {
		t.Fatalf("Exec = %v, want the connection's error (not nil, not ErrNil)", err)
	}
	for name, g := range map[string]*StringCmd{"aaaa": a, "nil": m, "stall": s} {
		v, e := g.Result()
		if e == nil || errors.Is(e, ErrNil) || v != "" || e.Error() != err.Error() {
			t.Errorf("%s = %q, %v; want \"\" and the Exec's error %v", name, v, e, err)
		}
	}
}
