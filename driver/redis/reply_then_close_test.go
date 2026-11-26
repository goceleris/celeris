package redis

// "Full reply delivered, then the connection closed at once (no pause)": the
// first call, whose reply was delivered whole, must report its real result
// with a nil error. The calls after it may report the connection loss (an
// error: the pool can hand out the closed connection until it notices), but
// never a wrong value, a hang or a panic. Not covered: whether the pool stops
// handing out closed connections after a bounded number of calls, and a close
// by RST (the fake closes with a FIN).

import (
	"bufio"
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// closeAfterReply serves GET (the key "last" closes the connection right
// after its reply was flushed), SET, and MULTI/EXEC with one queued command
// (EXEC's reply is flushed and the connection closed).
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
			writeBulk(w, "val-"+cmd[1])
			if cmd[1] == "last" {
				closeAll(w)
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
