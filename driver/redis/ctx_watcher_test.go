package redis

import (
	"bufio"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// A context that ends while a command waits on the server ends the call, with the
// context's error, in every mode of the driver, for a deadline and, for a command
// that blocks on the server (BLPOP 0, BZPOPMIN, XREAD BLOCK 0, ...), for cancel().
// A direct-mode conn (a Client on an async engine, on a started epoll or io_uring
// server, and off Linux on an engine that has not started) waits in a socket read
// that looks only at the deadline set from ctx.Deadline(): before this change a
// cancel() was not seen at all (BLPOP 0 never returned) and a deadline surfaced as
// the raw "i/o timeout", which is neither context.DeadlineExceeded nor
// context.Canceled. An ordinary command or a pipeline under a context with no
// deadline is not watched (it waits for the server's answer, as in v1.5.11; the
// loop modes end it at the cancel all the same).

// ctxWatcherModes is every way a Client can run, by name.
func ctxWatcherModes(t *testing.T) map[string][]Option {
	m := map[string][]Option{
		"standalone":                nil,
		"engine-async-std (direct)": {WithEngine(celeris.New(celeris.Config{Addr: "127.0.0.1:0", Engine: celeris.Std, AsyncHandlers: true}))},
	}
	for k, v := range ctxWatcherStartedModes(t) {
		m[k] = v
	}
	return m
}

// ctxWatcherClientOpts are the options every client of these tests has: one conn
// in the pool, so that a conn that was reused shows in the next command.
func ctxWatcherClientOpts(opts []Option) []Option {
	return append([]Option{WithPoolSize(1), WithDialTimeout(2 * time.Second)}, opts...)
}

// ctxEndsAfter returns a context that ends after d, by deadline or by cancel().
func ctxEndsAfter(kind string, d time.Duration) (context.Context, context.CancelFunc) {
	if kind == "deadline" {
		return context.WithTimeout(context.Background(), d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(d, cancel)
	return ctx, func() { timer.Stop(); cancel() }
}

func wantCtxErr(kind string) error {
	if kind == "deadline" {
		return context.DeadlineExceeded
	}
	return context.Canceled
}

// silentAfterHello answers HELLO and nothing else: every command waits forever.
func silentAfterHello(cmd []string, w *bufio.Writer) {
	if len(cmd) > 0 && strings.EqualFold(cmd[0], "HELLO") {
		handleHELLO(w, 3)
	}
}

type ctxWatcherOp struct {
	name string
	// blocking: the command blocks on the server, so cancel() ends it in every mode.
	blocking bool
	run      func(ctx context.Context, cl *Client) error
}

func doOp(name string, args ...any) ctxWatcherOp {
	return ctxWatcherOp{name: name, blocking: true, run: func(ctx context.Context, cl *Client) error {
		_, err := cl.Do(ctx, args...)
		return err
	}}
}

func ctxWatcherOps() []ctxWatcherOp {
	return []ctxWatcherOp{
		{"GET", false, func(ctx context.Context, cl *Client) error { _, err := cl.Get(ctx, "k"); return err }},
		{"SET", false, func(ctx context.Context, cl *Client) error { return cl.Set(ctx, "k", "v", 0) }},
		doOp("BLPOP 0", "BLPOP", "k", "0"),
		doOp("blpop 0 (lower case)", "blpop", "k", "0"),
		doOp("BRPOP 0", "BRPOP", "k", "0"),
		doOp("BZPOPMIN 0", "BZPOPMIN", "k", "0"),
		doOp("BLMOVE 0", "BLMOVE", "k", "d", "LEFT", "RIGHT", "0"),
		doOp("XREAD BLOCK 0", "XREAD", "BLOCK", "0", "STREAMS", "k", "$"),
		doOp("XREADGROUP BLOCK 0", "XREADGROUP", "GROUP", "g", "c", "BLOCK", "0", "STREAMS", "k", ">"),
		doOp("WAIT 1 0", "WAIT", "1", "0"),
		{"pipeline", false, func(ctx context.Context, cl *Client) error {
			p := cl.Pipeline()
			p.Get("a")
			p.Get("b")
			return p.Exec(ctx)
		}},
		{"MULTI/EXEC", false, func(ctx context.Context, cl *Client) error {
			tx, err := cl.TxPipeline(context.Background())
			if err != nil {
				return err
			}
			defer func() { _ = tx.Discard() }()
			tx.Get("a")
			tx.Get("b")
			return tx.Exec(ctx)
		}},
	}
}

// ctxWatcherCases are the (kind, op) pairs whose call the context must end: a
// deadline ends every op, a cancel() the blocking ones.
func ctxWatcherCases() [](struct {
	kind string
	op   ctxWatcherOp
}) {
	var out [](struct {
		kind string
		op   ctxWatcherOp
	})
	for _, kind := range []string{"deadline", "cancel"} {
		for _, op := range ctxWatcherOps() {
			if kind == "cancel" && !op.blocking {
				continue
			}
			out = append(out, struct {
				kind string
				op   ctxWatcherOp
			}{kind, op})
		}
	}
	return out
}

func TestCtxEndsACommandWaitingOnTheServer(t *testing.T) {
	const after = 150 * time.Millisecond
	for name, opts := range ctxWatcherModes(t) {
		for _, c := range ctxWatcherCases() {
			kind, op := c.kind, c.op
			t.Run(name+"/"+kind+"/"+op.name, func(t *testing.T) {
				fake := startFakeRedis(t, silentAfterHello)
				cl, err := NewClient(fake.Addr(), ctxWatcherClientOpts(opts)...)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = cl.Close() }()
				ctx, cancel := ctxEndsAfter(kind, after)
				defer cancel()
				done := make(chan error, 1)
				start := time.Now()
				go func() { done <- op.run(ctx, cl) }()
				select {
				case err := <-done:
					el := time.Since(start)
					if !errors.Is(err, wantCtxErr(kind)) {
						t.Fatalf("err = %v, want one that matches %v", err, wantCtxErr(kind))
					}
					if el < after-20*time.Millisecond || el > 2*time.Second {
						t.Fatalf("returned after %v, the context ends after %v", el, after)
					}
				case <-time.After(4 * time.Second):
					t.Fatalf("not returned 4 s after the context ended (%v)", after)
				}
			})
		}
	}
}

// slowThenFast answers HELLO, answers GET fast at once and every reply the test
// abandons (GET slow, BLPOP, the EXEC of a MULTI) only after slowDelay, with the
// bulk "SLOW".
func slowThenFast(slowDelay time.Duration) func(cmd []string, w *bufio.Writer) {
	return func(cmd []string, w *bufio.Writer) {
		if len(cmd) == 0 {
			return
		}
		switch strings.ToUpper(cmd[0]) {
		case "HELLO":
			handleHELLO(w, 3)
		case "MULTI":
			writeSimple(w, "OK")
		case "EXEC":
			_ = w.Flush()
			time.Sleep(slowDelay)
			writeArrayHeader(w, 1)
			writeBulk(w, "SLOW")
		case "BLPOP", "BZPOPMIN":
			_ = w.Flush()
			time.Sleep(slowDelay)
			writeArrayHeader(w, 2)
			writeBulk(w, "k")
			writeBulk(w, "SLOW")
		case "GET":
			switch cmd[1] {
			case "fast":
				writeBulk(w, "FAST")
			case "queued":
				writeSimple(w, "QUEUED")
			default:
				_ = w.Flush()
				time.Sleep(slowDelay)
				writeBulk(w, "SLOW")
			}
		case "SET":
			_ = w.Flush()
			time.Sleep(slowDelay)
			writeSimple(w, "SLOW")
		}
	}
}

// A command the context abandoned leaves its reply on the way. The conn that
// carried it is discarded, so the next command, on a pool of one, gets its own
// reply and never the abandoned command's.
func TestAnAbandonedCommandsReplyNeverReachesTheNextCommand(t *testing.T) {
	const slow = 400 * time.Millisecond
	ops := []ctxWatcherOp{
		{"GET", false, func(ctx context.Context, cl *Client) error { _, err := cl.Get(ctx, "slow"); return err }},
		{"SET", false, func(ctx context.Context, cl *Client) error { return cl.Set(ctx, "slow", "v", 0) }},
		doOp("BLPOP 0", "BLPOP", "k", "0"),
		doOp("BZPOPMIN 0", "BZPOPMIN", "k", "0"),
		{"pipeline", false, func(ctx context.Context, cl *Client) error {
			p := cl.Pipeline()
			p.Get("slow")
			p.Get("slow")
			return p.Exec(ctx)
		}},
		{"MULTI/EXEC", false, func(ctx context.Context, cl *Client) error {
			tx, err := cl.TxPipeline(context.Background())
			if err != nil {
				return err
			}
			defer func() { _ = tx.Discard() }()
			tx.Get("queued")
			return tx.Exec(ctx)
		}},
	}
	for name, opts := range ctxWatcherModes(t) {
		for _, kind := range []string{"deadline", "cancel"} {
			for _, op := range ops {
				if kind == "cancel" && !op.blocking {
					continue
				}
				t.Run(name+"/"+kind+"/"+op.name, func(t *testing.T) {
					fake := startFakeRedis(t, slowThenFast(slow))
					cl, err := NewClient(fake.Addr(), ctxWatcherClientOpts(opts)...)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = cl.Close() }()
					ctx, cancel := ctxEndsAfter(kind, 50*time.Millisecond)
					defer cancel()
					done := make(chan error, 1)
					go func() { done <- op.run(ctx, cl) }()
					select {
					case err := <-done:
						if !errors.Is(err, wantCtxErr(kind)) {
							t.Fatalf("err = %v, want one that matches %v", err, wantCtxErr(kind))
						}
					case <-time.After(3 * time.Second):
						t.Fatal("the abandoned command did not return")
					}
					// The server still holds the abandoned reply. Whatever conn the next
					// command runs on must not be the one it will arrive on.
					next, stopNext := context.WithTimeout(context.Background(), 5*time.Second)
					defer stopNext()
					v, err := cl.Get(next, "fast")
					if err != nil {
						t.Fatalf("next command: %v", err)
					}
					if v != "FAST" {
						t.Fatalf("next command got %q, want \"FAST\" (the abandoned command's reply, or none of its own)", v)
					}
					time.Sleep(slow + 100*time.Millisecond) // the abandoned reply has been sent by now
					v, err = cl.Get(next, "fast")
					if err != nil || v != "FAST" {
						t.Fatalf("after the abandoned reply was sent: %q, %v", v, err)
					}
				})
			}
		}
	}
}
