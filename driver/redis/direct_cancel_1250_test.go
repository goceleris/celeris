package redis

import (
	"bufio"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// celeris#1250: the direct-mode cancel (a deadline in the past on the conn,
// set when the handshake's context ends) has three rules, pinned below:
//
//   - execDirectBuf and execManyDirect check ctx right after they set the
//     socket deadline from it, so a cancel that came first is not overwritten;
//   - a conn whose cancel fired after the handshake succeeded is never returned
//     (the past deadline would be its first command's i/o timeout);
//   - the cancel hook is a child of the handshake's own timeout context, so it
//     runs only once that context reports the cancel, whatever the caller's
//     context type does with its Done channel.

// setDirectHooks installs h for the test and clears it afterwards.
func setDirectHooks(t *testing.T, h *directHooks) {
	t.Helper()
	testHookDirect.Store(h)
	t.Cleanup(func() { testHookDirect.Store(nil) })
}

// startHelloPingNode1250 answers HELLO, PING and anything else with +OK.
func startHelloPingNode1250(t *testing.T) string {
	t.Helper()
	f := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		if len(cmd) == 0 {
			return
		}
		switch strings.ToUpper(cmd[0]) {
		case "HELLO":
			handleHELLO(w, 3)
		case "PING":
			_, _ = w.WriteString("+PONG\r\n")
		default:
			_, _ = w.WriteString("+OK\r\n")
		}
	})
	return f.Addr()
}

// within1250 runs f and fails the test if it does not return: the deadline only
// bounds a hang.
func within1250(t *testing.T, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	awaitClosed1246(t, done, what)
}

// (a) A command whose ctx is already cancelled does not run on a direct conn: the
// check after the socket deadline is set reports the cancel. Without it the
// deadline set from ctx overwrites a cancel's past deadline and the command waits
// for the deadline of ctx.
func TestDirectExecWithACancelledCtxReportsTheCancel1250(t *testing.T) {
	addr := startHelloPingNode1250(t)
	dial := func(t *testing.T) *redisConn {
		c, err := dialDirectRedisConn(context.Background(), Config{Addr: addr, DialTimeout: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	cancelled := func() context.Context {
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		cancel()
		return ctx
	}
	t.Run("exec", func(t *testing.T) {
		c := dial(t)
		var req *redisRequest
		var err error
		within1250(t, "exec", func() { req, err = c.exec(cancelled(), "PING") })
		if err == nil {
			c.releaseResult(req)
			t.Fatal("PING ran with a cancelled ctx")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
	t.Run("execMany", func(t *testing.T) {
		c := dial(t)
		var err error
		within1250(t, "execMany", func() { _, err = c.execMany(cancelled(), []byte("*1\r\n$4\r\nPING\r\n"), 1) })
		if err == nil {
			t.Fatal("the pipeline ran with a cancelled ctx")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
}

// (b) The caller's ctx is cancelled after the handshake's commands succeeded and
// before the dial returns: the cancel hook has fired, the conn carries (or will
// carry) a deadline in the past, and the dial must not return it.
func TestDirectDialNeverReturnsAConnWhoseCancelFiredAfterTheHandshake1250(t *testing.T) {
	addr := startHelloPingNode1250(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	setDirectHooks(t, &directHooks{afterHandshake: func(c *redisConn) {
		if c.cfg.Addr == addr {
			cancel()
		}
	}})
	var c *redisConn
	var err error
	within1250(t, "the dial", func() {
		c, err = dialDirectRedisConn(ctx, Config{Addr: addr, DialTimeout: time.Hour})
	})
	if c != nil {
		_ = c.Close()
		t.Fatal("the dial returned a conn whose cancel had fired")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// A conn the dial returns carries no deadline: its first command, whose ctx has
// none, does not fail with an i/o timeout, whenever the caller's ctx is cancelled
// afterwards.
func TestDirectDialReturnsAConnWithNoDeadline1250(t *testing.T) {
	addr := startHelloPingNode1250(t)
	ctx, cancel := context.WithCancel(context.Background())
	c, err := dialDirectRedisConn(ctx, Config{Addr: addr, DialTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	cancel()
	var req *redisRequest
	within1250(t, "PING", func() { req, err = c.exec(context.Background(), "PING") })
	if err != nil {
		t.Fatalf("the first command after a cancel of the dial's ctx: %v", err)
	}
	c.releaseResult(req)
}

// orderedCtx is a cancellable Context that implements AfterFunc (which
// context.WithCancel and context.AfterFunc honour): on cancel it runs the funcs
// registered on it, the first at once and the rest after hold, in the order they
// were registered. The order of the children of a context is unspecified, and
// this is one legal order: a hook registered on the caller's ctx runs a full
// hold before the handshake's own timeout context hears of the cancel.
type orderedCtx struct {
	context.Context
	mu    sync.Mutex
	funcs []func()
	done  chan struct{}
	err   error
}

func newOrderedCtx() *orderedCtx {
	return &orderedCtx{Context: context.Background(), done: make(chan struct{})}
}

func (o *orderedCtx) Done() <-chan struct{} { return o.done }

func (o *orderedCtx) Err() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.err
}

func (o *orderedCtx) AfterFunc(f func()) func() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	i := len(o.funcs)
	o.funcs = append(o.funcs, f)
	return func() bool {
		o.mu.Lock()
		defer o.mu.Unlock()
		if o.funcs[i] == nil || o.err != nil {
			return false
		}
		o.funcs[i] = nil
		return true
	}
}

func (o *orderedCtx) cancel(hold time.Duration) {
	o.mu.Lock()
	o.err = context.Canceled
	close(o.done)
	fs := append([]func(){}, o.funcs...)
	o.mu.Unlock()
	ran := 0
	for _, f := range fs {
		if f == nil {
			continue
		}
		if ran == 1 {
			time.Sleep(hold)
		}
		f()
		ran++
	}
}

// (c) The caller's ctx has its own Done channel and runs its children in an order
// of its own. The cancel comes just before the second handshake command (SELECT)
// sets its socket deadline, and the test goes on only once the cancel hook has set
// the past deadline: that command's deadline then overwrites it, and only the
// check of ctx after that, which sees the cancel through the handshake's own
// context, ends the dial. A hook registered on the caller's ctx, not on the
// handshake's, runs before that context hears of the cancel (the hold), the check
// finds nothing, and the dial waits for its DialTimeout (an hour here).
func TestDirectDialCancelOrderWithACtxOfItsOwn1250(t *testing.T) {
	f := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		if len(cmd) > 0 && strings.EqualFold(cmd[0], "HELLO") {
			handleHELLO(w, 3)
		}
		// SELECT is never answered.
	})
	addr := f.Addr()
	octx := newOrderedCtx()
	var commands atomic.Int32
	interrupted := make(chan struct{}, 8)
	cancelDone := make(chan struct{})
	setDirectHooks(t, &directHooks{
		beforeDeadline: func(c *redisConn) {
			if c.cfg.Addr != addr || commands.Add(1) != 2 {
				return
			}
			go func() {
				defer close(cancelDone)
				octx.cancel(300 * time.Millisecond)
			}()
			select {
			case <-interrupted:
			case <-time.After(30 * time.Second):
			}
		},
		interrupted: func(c *redisConn) {
			if c.cfg.Addr == addr {
				select {
				case interrupted <- struct{}{}:
				default:
				}
			}
		},
	})
	var c *redisConn
	var err error
	within1250(t, "the dial", func() {
		c, err = dialDirectRedisConn(octx, Config{Addr: addr, DB: 1, DialTimeout: time.Hour})
	})
	if c != nil {
		_ = c.Close()
		t.Fatal("the dial returned a conn")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	awaitClosed1246(t, cancelDone, "the cancel to finish")
}
