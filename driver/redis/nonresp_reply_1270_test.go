package redis

import (
	"bufio"
	"context"
	"errors"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	celeris "github.com/goceleris/celeris"
	"github.com/goceleris/celeris/internal/driver/redis/protocol"
)

// celeris#1270: on the default path on Linux (NewClient, the sync poll) a reply
// that is not valid RESP did not end a Pipeline.Exec. The parse error closed the
// conn, but the Pipeline's requests sat in the sync dispatch table, not in the
// bridge the close drains, so nothing finished them: Exec ran until its
// context ended (for ever with context.Background). The tests send replies the
// parser rejects and expect the protocol error at once, the conn closed and
// its pool slot back. They run on the default path (the sync poll on Linux, the
// event-loop bridge elsewhere) and on the direct one (AsyncHandlers).

// nonRESP1270 are replies to a GET that protocol.Reader rejects (not "needs more
// bytes"): a line with no RESP type byte, a bulk header with no length, and a
// bulk body that is not followed by CRLF.
var nonRESP1270 = []struct{ name, reply string }{
	{"garbage line", "garbage\r\n"},
	{"truncated bulk length", "$\r\n"},
	{"bulk body without CRLF", "$3\r\nabcde\r\n"},
}

func pathName1270(direct bool) string {
	if direct {
		return "direct"
	}
	return "default"
}

// newNonRESPClient1270 is a PoolSize-1 client on the default path (direct:
// false) or the direct one. It talks to a fake server whose answer to GET is
// *reply (raw bytes) once one is stored, and which answers the handshake and
// every other command as a healthy server.
func newNonRESPClient1270(t *testing.T, direct bool) (*Client, *atomic.Pointer[string]) {
	t.Helper()
	reply := new(atomic.Pointer[string])
	srv := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		if len(cmd) == 0 {
			return
		}
		switch strings.ToUpper(cmd[0]) {
		case "HELLO":
			handleHELLO(w, 3)
		case "GET":
			if raw := reply.Load(); raw != nil {
				_, _ = w.WriteString(*raw)
				return
			}
			writeBulk(w, "v")
		default:
			writeSimple(w, "OK")
		}
	})
	opts := []Option{WithPoolSize(1)}
	if direct {
		opts = append(opts, WithEngine(celeris.New(celeris.Config{
			Addr:          "127.0.0.1:0",
			Engine:        celeris.Std,
			AsyncHandlers: true,
		})))
	}
	cl, err := NewClient(srv.Addr(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := cl.pool.acquireCmd(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if c.useDirect != direct {
		t.Fatalf("fixture: the pooled conn has useDirect = %v, want %v", c.useDirect, direct)
	}
	if runtime.GOOS == "linux" && !direct && c.syncMulti == nil {
		t.Fatal("fixture: the default path on Linux is the sync poll, and this conn has none")
	}
	cl.pool.releaseCmd(c)
	return cl, reply
}

// wantProtocolErr1270 runs run (which makes the failing call and returns the
// call's error and the error of its command) and checks that both are a
// protocol error, returned within a second, not the context's deadline; that
// the conn is gone from the pool (slot released); and that a later command
// works. A call that never returns fails the test instead of hanging it: run
// is bounded by the select below, it reports only through the channel (so
// nothing touches t after the test ends), and a stuck run is unblocked by the
// client's Close in the test's cleanup.
func wantProtocolErr1270(t *testing.T, cl *Client, reply *atomic.Pointer[string], run func() (callErr, cmdErr error)) {
	t.Helper()
	type result struct{ callErr, cmdErr error }
	res := make(chan result, 1)
	go func() {
		c, m := run()
		res <- result{c, m}
	}()
	select {
	case r := <-res:
		if !errors.Is(r.callErr, protocol.ErrProtocol) {
			t.Errorf("call err = %v, want a protocol error (not the context's deadline)", r.callErr)
		}
		if !errors.Is(r.cmdErr, protocol.ErrProtocol) {
			t.Errorf("command err = %v, want a protocol error", r.cmdErr)
		}
	case <-time.After(time.Second):
		t.Fatal("the call is still blocked after 1s: a reply that is not RESP did not end it")
	}

	// The conn is closed and dropped; its slot is free.
	if st := cl.Stats(); st.Open != 0 || st.Idle != 0 || st.InUse != 0 {
		t.Errorf("pool Open/Idle/InUse = %d/%d/%d after the failed call, want 0/0/0", st.Open, st.Idle, st.InUse)
	}
	reply.Store(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if v, err := cl.Get(ctx, "a"); err != nil || v != "v" {
		t.Errorf("Get after the failed call = %q, %v; want \"v\", nil (the slot is not released)", v, err)
	}
}

func TestPipelineExecNonRESPReplyReturnsProtocolError1270(t *testing.T) {
	for _, direct := range []bool{false, true} {
		for _, tc := range nonRESP1270 {
			t.Run(pathName1270(direct)+"/"+tc.name, func(t *testing.T) {
				cl, reply := newNonRESPClient1270(t, direct)
				reply.Store(&tc.reply)
				// The deadline is a safety net: the error must come long before it.
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				wantProtocolErr1270(t, cl, reply, func() (error, error) {
					p := cl.Pipeline()
					defer p.Release()
					g := p.Get("a")
					p.Get("b")
					err := p.Exec(ctx)
					_, gerr := g.Result()
					return err, gerr
				})
			})
		}
	}
}

// Tx.Exec sends MULTI, the commands and EXEC in one pipeline, so the bad reply
// lands on the sync pipeline too (it arrives as the reply of the queued GET).
func TestTxExecNonRESPReplyReturnsProtocolError1270(t *testing.T) {
	for _, direct := range []bool{false, true} {
		for _, tc := range nonRESP1270 {
			t.Run(pathName1270(direct)+"/"+tc.name, func(t *testing.T) {
				cl, reply := newNonRESPClient1270(t, direct)
				reply.Store(&tc.reply)
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				wantProtocolErr1270(t, cl, reply, func() (error, error) {
					tx, err := cl.TxPipeline(ctx)
					if err != nil {
						return err, err
					}
					g := tx.Get("a")
					err = tx.Exec(ctx)
					_, gerr := g.Result()
					return err, gerr
				})
			})
		}
	}
}

func TestPipelineExecNonRESPReplyBackgroundCtx1270(t *testing.T) {
	for _, direct := range []bool{false, true} {
		t.Run(pathName1270(direct), func(t *testing.T) {
			cl, reply := newNonRESPClient1270(t, direct)
			raw := "garbage\r\n"
			reply.Store(&raw)
			wantProtocolErr1270(t, cl, reply, func() (error, error) {
				p := cl.Pipeline()
				defer p.Release()
				g := p.Get("a")
				err := p.Exec(context.Background())
				_, gerr := g.Result()
				return err, gerr
			})
		})
	}
}

// A plain command is not a pipeline: its request is in the bridge, which the
// close drains. It passes on main and guards that path.
func TestGetNonRESPReplyReturnsProtocolError1270(t *testing.T) {
	for _, direct := range []bool{false, true} {
		for _, tc := range nonRESP1270 {
			t.Run(pathName1270(direct)+"/"+tc.name, func(t *testing.T) {
				cl, reply := newNonRESPClient1270(t, direct)
				reply.Store(&tc.reply)
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				wantProtocolErr1270(t, cl, reply, func() (error, error) {
					_, err := cl.Get(ctx, "a")
					return err, err
				})
			})
		}
	}
}
