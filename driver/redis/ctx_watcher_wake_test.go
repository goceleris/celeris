package redis

import (
	"bufio"
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// The watcher itself (wake.go): when it is armed, what a late callback may do.

func startPingNode(t testing.TB, commands *atomic.Int64) string {
	t.Helper()
	f := startFakeRedisTB(t, func(cmd []string, w *bufio.Writer) {
		if len(cmd) == 0 {
			return
		}
		switch strings.ToUpper(cmd[0]) {
		case "HELLO":
			handleHELLO(w, 3)
		case "PING":
			commands.Add(1)
			writeSimple(w, "PONG")
		case "GET":
			commands.Add(1)
			writeBulk(w, "v")
		case "BLPOP":
			commands.Add(1)
			writeArrayHeader(w, 2)
			writeBulk(w, "k")
			writeBulk(w, "v")
		default:
			writeSimple(w, "OK")
		}
	})
	return f.Addr()
}

func dialDirectForWake(t *testing.T) (*redisConn, *atomic.Int64) {
	t.Helper()
	var n atomic.Int64
	c, err := dialDirectRedisConn(context.Background(), Config{Addr: startPingNode(t, &n), DialTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, &n
}

func pingOn(ctx context.Context, t *testing.T, c *redisConn) error {
	t.Helper()
	req, err := c.exec(ctx, "PING")
	if err != nil {
		return err
	}
	c.releaseResult(req)
	return nil
}

func blpopOn(ctx context.Context, t *testing.T, c *redisConn) error {
	t.Helper()
	req, err := c.exec(ctx, "BLPOP", "k", "0")
	if err != nil {
		return err
	}
	c.releaseResult(req)
	return nil
}

// Nothing is watched but a blocking command under a context that can be canceled and
// has no deadline: not the handshake (it has its own wake), not a command under
// context.Background, not an ordinary command, a pipeline or a Tx under a context
// that can end, and not a blocking command under a context with a deadline (the
// socket's deadline ends it). The one case arms the watcher for the command and
// disarms it when the command ends.
func TestTheWatcherIsArmedOnlyForABlockingCommandUnderAContextThatCanEnd(t *testing.T) {
	c, _ := dialDirectForWake(t)
	if c.wake.fn != nil {
		t.Fatal("the handshake armed the command watcher")
	}
	ends := []struct {
		name string
		mk   func() (context.Context, context.CancelFunc)
		// armsBlocking: a blocking command under it arms the watcher.
		armsBlocking bool
	}{
		{"cancel", func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) }, true},
		{"deadline", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), time.Hour)
		}, false},
		{"deadline and cancel", func() (context.Context, context.CancelFunc) {
			c, cancel := context.WithCancel(context.Background())
			d, cancelD := context.WithTimeout(c, time.Hour)
			return d, func() { cancelD(); cancel() }
		}, false},
	}
	for i := 0; i < 3; i++ {
		if err := pingOn(context.Background(), t, c); err != nil {
			t.Fatal(err)
		}
		if err := blpopOn(context.Background(), t, c); err != nil {
			t.Fatal(err)
		}
	}
	if err := blpopOn(context.TODO(), t, c); err != nil {
		t.Fatal(err)
	}
	if c.wake.fn != nil || c.wake.stop != nil {
		t.Fatal("a context that cannot end armed the watcher")
	}
	for _, e := range ends {
		ctx, cancel := e.mk()
		// Ordinary commands (exec, exec2 and exec3 paths) and a pipeline.
		if err := pingOn(ctx, t, c); err != nil {
			t.Fatal(err)
		}
		req, err := c.exec2(ctx, "GET", "k")
		if err != nil {
			t.Fatal(err)
		}
		c.releaseResult(req)
		req, err = c.exec3(ctx, "SET", "k", "v")
		if err != nil {
			t.Fatal(err)
		}
		c.releaseResult(req)
		reqs, err := c.execMany(ctx, []byte("*2\r\n$3\r\nGET\r\n$1\r\nk\r\n"), 1)
		if err != nil {
			t.Fatal(err)
		}
		c.releaseResult(reqs[0])
		if c.wake.fn != nil {
			t.Fatalf("%s context: an ordinary command or a pipeline armed the watcher", e.name)
		}
		if err := blpopOn(ctx, t, c); err != nil {
			t.Fatal(err)
		}
		cancel()
		if armed := c.wake.fn != nil; armed != e.armsBlocking {
			t.Fatalf("%s context: a blocking command armed the watcher: %v, want %v", e.name, armed, e.armsBlocking)
		}
		if c.wake.stop != nil {
			t.Fatalf("%s context: the watcher is still armed after the command", e.name)
		}
		c.wake.fn = nil
	}
	// And a command on the conn after all that is not affected by the past contexts.
	if err := blpopOn(context.Background(), t, c); err != nil {
		t.Fatal(err)
	}
}

// A cancel() during an ordinary command on a direct conn does not end it, as in
// v1.5.11: the command waits for the answer and succeeds.
func TestACancelDoesNotEndAnOrdinaryCommandOnADirectConn(t *testing.T) {
	f := startFakeRedis(t, slowThenFast(300*time.Millisecond))
	c, err := dialDirectRedisConn(context.Background(), Config{Addr: f.Addr(), DialTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	req, err := c.exec2(ctx, "GET", "slow")
	if err != nil {
		t.Fatalf("an ordinary command under a cancel-only context ended with %v, want the server's answer", err)
	}
	c.releaseResult(req)
	if time.Since(start) < 250*time.Millisecond {
		t.Fatal("returned before the server answered")
	}
}

// A context that has ended before a blocking command starts sends nothing.
func TestAContextThatEndedSendsNothing(t *testing.T) {
	c, n := dialDirectForWake(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := blpopOn(ctx, t, c); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := n.Load(); got != 0 {
		t.Fatalf("the server received %d commands from a cancelled context, want 0", got)
	}
}

func TestIsBlockingCommand(t *testing.T) {
	yes := [][]string{
		{"BLPOP", "k", "0"}, {"blpop", "k", "0"}, {"BRPOP", "k", "0"}, {"BRPOPLPUSH", "a", "b", "0"},
		{"BLMOVE", "a", "b", "LEFT", "RIGHT", "0"}, {"BLMPOP", "0", "1", "k", "LEFT"}, {"BZPOPMIN", "k", "0"},
		{"BZPOPMAX", "k", "0"}, {"BZMPOP", "0", "1", "k", "MIN"}, {"WAIT", "1", "0"}, {"WAITAOF", "1", "0", "0"},
		{"XREAD", "BLOCK", "0", "STREAMS", "k", "$"}, {"xread", "COUNT", "2", "block", "5", "STREAMS", "k", "0"},
		{"XREADGROUP", "GROUP", "g", "c", "BLOCK", "0", "STREAMS", "k", ">"},
		// names that look like keywords
		{"XREADGROUP", "GROUP", "STREAMS", "c", "BLOCK", "0", "STREAMS", "k", ">"},
		{"XREADGROUP", "GROUP", "g", "STREAMS", "BLOCK", "0", "STREAMS", "k", ">"},
		{"XREADGROUP", "GROUP", "g", "c", "COUNT", "5", "BLOCK", "0", "STREAMS", "k", ">"},
		{"XREADGROUP", "GROUP", "g", "c", "NOACK", "BLOCK", "100", "STREAMS", "k", ">"},
	}
	no := [][]string{
		nil, {}, {"GET", "k"}, {"SET", "k", "v"}, {"PING"}, {"LPOP", "k"}, {"BGSAVE"}, {"EVAL", "return 1", "0"},
		{"XREAD", "STREAMS", "k", "0"}, {"XREAD", "STREAMS", "BLOCK", "0"}, {"XREADGROUP", "GROUP", "g", "c", "STREAMS", "k", ">"},
		// group and consumer names are names, whatever they are called
		{"XREADGROUP", "GROUP", "g", "BLOCK", "STREAMS", "k", ">"}, {"XREADGROUP", "GROUP", "BLOCK", "BLOCK", "STREAMS", "k", ">"},
		{"XREADGROUP", "GROUP", "g"}, {"XREADGROUP", "g", "c", "BLOCK", "0", "STREAMS", "k", ">"},
		{"XREAD", "COUNT", "BLOCK", "STREAMS", "k", "0"}, {"XREADGROUP", "GROUP", "g", "c", "COUNT", "5", "STREAMS", "k", ">"},
		{"XADD", "k", "*", "BLOCK", "1"}, {"MULTI"}, {"EXEC"},
	}
	for _, a := range yes {
		if !isBlockingCommand(a) {
			t.Errorf("%v is a blocking command", a)
		}
	}
	for _, a := range no {
		if isBlockingCommand(a) {
			t.Errorf("%v is not a blocking command", a)
		}
	}
}

// A context that ends as its command finishes leaves the conn without a deadline:
// the wake callback is waited for, and undone, by disarmWake.
func TestAWakeThatFiredAfterTheReplyLeavesNoDeadline(t *testing.T) {
	c, _ := dialDirectForWake(t)
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		c.writerMu.Lock()
		c.armWake(ctx)
		cancel() // the command is over, the callback has started or is about to
		c.disarmWake()
		c.writerMu.Unlock()
		if err := pingOn(context.Background(), t, c); err != nil {
			t.Fatalf("round %d: a live command after a context that ended with the last: %v", i, err)
		}
	}
}

// The wake callback of a command that ended as its context was cancelled can still
// be in flight when the next command starts on the same conn; it must not give that
// command, whose context is alive, a deadline of "now". The hook holds the callback
// of the first command for 100 ms so that it lands after the second one has armed
// its own wake.
func TestALateWakeDoesNotFailTheNextCommand(t *testing.T) {
	d := 100 * time.Millisecond
	testHookWakeDelay.Store(&d)
	t.Cleanup(func() { testHookWakeDelay.Store(nil) })
	c, _ := dialDirectForWake(t)
	ctx1, cancel1 := context.WithCancel(context.Background())
	c.writerMu.Lock()
	c.armWake(ctx1)
	cancel1()
	c.disarmWake()
	c.writerMu.Unlock()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	done := make(chan error, 1)
	go func() { done <- pingOn(ctx2, t, c) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a live command after a cancelled one: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("hung")
	}
}

// RESP2 (no HELLO) takes the same paths.
func TestCtxEndsACommandOnARESP2Conn(t *testing.T) {
	for _, c := range ctxWatcherCases() {
		kind, op := c.kind, c.op
		t.Run(kind+"/"+op.name, func(t *testing.T) {
			fake := startFakeRedis(t, func([]string, *bufio.Writer) {})
			cl, err := NewClient(fake.Addr(), ctxWatcherClientOpts([]Option{
				WithForceRESP2(),
				WithEngine(celeris.New(celeris.Config{Addr: "127.0.0.1:0", Engine: celeris.Std, AsyncHandlers: true})),
			})...)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cl.Close() }()
			ctx, cancel := ctxEndsAfter(kind, 100*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- op.run(ctx, cl) }()
			select {
			case err := <-done:
				if !errors.Is(err, wantCtxErr(kind)) {
					t.Fatalf("err = %v, want one that matches %v", err, wantCtxErr(kind))
				}
			case <-time.After(3 * time.Second):
				t.Fatal("not returned")
			}
		})
	}
}

// BenchmarkWatcher is the watcher alone, with no network: what arming and disarming
// it costs a blocking command per kind of context, and what the decision to arm
// costs: "decide-" is the test in execDirect, for a context that cannot end
// (Background) and for one that can, with an ordinary command (GET) and a blocking
// one (BLPOP).
func BenchmarkWatcher(b *testing.B) {
	var n atomic.Int64
	c, err := dialDirectRedisConn(context.Background(), Config{Addr: startPingNode(b, &n), DialTimeout: time.Hour})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = c.Close() })
	wc, cancel := context.WithCancel(context.Background())
	defer cancel()
	tc, cancel2 := context.WithTimeout(context.Background(), time.Hour)
	defer cancel2()
	get, blpop := []string{"GET", "k"}, []string{"BLPOP", "k", "0"}
	var sink bool
	for _, d := range []struct {
		name string
		ctx  context.Context
		args []string
	}{
		{"decide-background-get", context.Background(), get},
		{"decide-cancel-get", wc, get},
		{"decide-cancel-blpop", wc, blpop},
	} {
		b.Run(d.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				sink = d.ctx.Done() != nil && isBlockingCommand(d.args)
			}
		})
	}
	_ = sink
	for _, k := range []struct {
		name string
		ctx  context.Context
	}{{"cancel", wc}, {"timeout", tc},
		// An HTTP/2 stream's context (c.Context() of a celeris handler): not a
		// standard library context, so context.AfterFunc runs a goroutine for it.
		{"h2-stream-context", stream.NewStream(1).Context()}} {
		b.Run(k.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				c.armWake(k.ctx)
				c.disarmWake()
			}
		})
	}
}

// The error of a read that the socket's deadline ended: what the context says if it
// says anything, else a deadline for a context that has one; anything else as it is.
func TestCtxIOError(t *testing.T) {
	timeout := os.ErrDeadlineExceeded
	other := errors.New("connection reset")
	if got := ctxIOError(context.Background(), other); got != other {
		t.Errorf("another error was changed: %v", got)
	}
	if got := ctxIOError(context.Background(), timeout); got != timeout {
		t.Errorf("a timeout under a context with no deadline was changed: %v", got)
	}
	// A deadline context whose timer has not reported yet: the socket fired a hair early.
	dctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	if got := ctxIOError(dctx, timeout); !errors.Is(got, context.DeadlineExceeded) || !errors.Is(got, os.ErrDeadlineExceeded) {
		t.Errorf("early socket timeout: %v, want DeadlineExceeded and the socket's", got)
	}
	// Cancelled before its deadline: a cancel is a cancel.
	cancel()
	if got := ctxIOError(dctx, timeout); !errors.Is(got, context.Canceled) || errors.Is(got, context.DeadlineExceeded) {
		t.Errorf("deadline context canceled early: %v, want Canceled", got)
	}
	// Expired.
	ectx, ecancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer ecancel()
	if got := ctxIOError(ectx, timeout); !errors.Is(got, context.DeadlineExceeded) {
		t.Errorf("expired: %v", got)
	}
	// The watcher's end: no deadline, canceled.
	cctx, ccancel := context.WithCancel(context.Background())
	ccancel()
	if got := ctxIOError(cctx, timeout); !errors.Is(got, context.Canceled) {
		t.Errorf("canceled: %v", got)
	}
}
