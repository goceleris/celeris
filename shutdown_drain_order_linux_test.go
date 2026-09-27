//go:build linux

package celeris_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// TestShutdownHooksRunAfterTheDrain pins celeris#703: Server.Shutdown's godoc
// promises that the OnShutdown hooks fire after the engine has stopped
// accepting and drained the requests in flight, and that is the order net/http
// teaches. On epoll and io_uring, whose Engine.Shutdown is a no-op and whose
// drain runs in Listen once its context is cancelled, Shutdown used to run
// every hook and return at once, while a request was still being handled; a
// cancel of StartWithContext's context ran the hooks at once too, and only the
// Start call waited for the drain.
//
// Each case holds one request in its handler, starts the shutdown, and asks
// three things: had the handler finished when the first hook started, had the
// hook started before the call that shut the server down returned, and did the
// client get the whole response.
//
// The order is forced, not raced. The handler returns only when the test
// releases it, and the test releases it as soon as a hook starts or the
// shutting-down call returns, whichever comes first, or after releaseAfter if
// neither does. So a Shutdown that does not wait for the drain always reaches
// its hooks (and returns) with the handler still held, and the case fails; a
// Shutdown that waits cannot reach its hooks until releaseAfter has passed and
// the handler has returned. releaseAfter only has to be longer than it takes
// Shutdown to get from its first line to its hook loop when nothing makes it
// wait, which is microseconds.
//
// It is also the deadlock check for the fix. The Start*Context watcher runs
// the cancel's Shutdown, and that Shutdown now waits for Listen; had it waited
// for the Start call instead, which waits for the watcher, the two would wait
// on each other until Config.ShutdownTimeout. Every budget here is 30s and the
// call must return within callCap of the release, so such a wait fails the
// case instead of passing late.
func TestShutdownHooksRunAfterTheDrain(t *testing.T) {
	const releaseAfter = 300 * time.Millisecond
	type engineCase struct {
		name  string
		eng   celeris.EngineType
		async bool
	}
	engines := []engineCase{
		{"std", celeris.Std, false},
		{"epoll", celeris.Epoll, false},
		{"epoll-async", celeris.Epoll, true},
		{"io_uring", celeris.IOUring, false},
		{"io_uring-async", celeris.IOUring, true},
		{"adaptive", celeris.Adaptive, false},
	}
	modes := []drainOrderMode{drainDirectAfterStartWithContext, drainCancelStartWithContext, drainDirectAfterStartWithListener}
	for _, ec := range engines {
		for _, mode := range modes {
			t.Run(ec.name+"/"+mode.String(), func(t *testing.T) {
				runDrainOrderCase(t, ec.eng, ec.async, mode, releaseAfter)
			})
		}
	}
}

const (
	// shutdownBudget703 is every shutdown budget in the test: well above
	// callCap703, so a call that waits out its budget fails the case.
	shutdownBudget703 = 30 * time.Second
	callCap703        = 10 * time.Second
)

// waitDrainOrderReady polls /ping until the server answers 200, or returns
// the error its start returned first.
func waitDrainOrderReady(addr string, startDone <-chan error) error {
	probe := &http.Client{Timeout: 300 * time.Millisecond}
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		select {
		case err := <-startDone:
			if err == nil {
				err = errors.New("start returned nil before the server was ready")
			}
			return err
		default:
		}
		if resp, err := probe.Get("http://" + addr + "/ping"); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("no answer on /ping at %s within 15s", addr)
}

type drainOrderMode int

const (
	// A direct Server.Shutdown on a server started with StartWithContext,
	// whose context is never cancelled.
	drainDirectAfterStartWithContext drainOrderMode = iota
	// Cancelling StartWithContext's context; the call that shuts the server
	// down is StartWithContext itself.
	drainCancelStartWithContext
	// A direct Server.Shutdown on a server started with StartWithListener,
	// which runs Listen on its own path, not through the context watcher.
	drainDirectAfterStartWithListener
)

func (m drainOrderMode) String() string {
	switch m {
	case drainDirectAfterStartWithContext:
		return "Shutdown"
	case drainCancelStartWithContext:
		return "cancel"
	case drainDirectAfterStartWithListener:
		return "StartWithListener+Shutdown"
	}
	return "unknown"
}

func runDrainOrderCase(t *testing.T, engType celeris.EngineType, async bool, mode drainOrderMode, releaseAfter time.Duration) {
	t.Helper()
	// One sequence for every event, so the order is read from numbers, not
	// from clocks. Zero means "has not happened".
	var seq, handlerDone, hookStart, callReturn atomic.Int64
	var hookSawHandlerDone atomic.Bool
	var t0 atomic.Pointer[time.Time]
	since := func() time.Duration {
		if p := t0.Load(); p != nil {
			return time.Since(*p)
		}
		return 0
	}
	var handlerDoneAt, hookStartAt, callReturnAt atomic.Int64 // ns since t0

	handlerEntered := make(chan struct{})
	release := make(chan struct{})
	hookStarted := make(chan struct{})

	// build makes the server for one start attempt. The default worker
	// count: the drain is over only when every worker has stopped, and the
	// request in flight is on one of them.
	build := func(addr string) *celeris.Server {
		s := celeris.New(celeris.Config{
			Engine:          engType,
			AsyncHandlers:   async,
			ShutdownTimeout: shutdownBudget703,
			// With a supplied listener, adaptive wants Addr to be the
			// listener's own address (see start_shutdown_return_linux_test.go);
			// for the others it is the address to bind.
			Addr: addr,
		})
		s.GET("/ping", func(c *celeris.Context) error { return c.String(http.StatusOK, "ok") })
		s.GET("/slow", func(c *celeris.Context) error {
			close(handlerEntered)
			<-release
			handlerDoneAt.Store(int64(since()))
			handlerDone.Store(seq.Add(1))
			return c.String(http.StatusOK, "done")
		})
		s.OnShutdown(func(context.Context) {
			hookSawHandlerDone.Store(handlerDone.Load() != 0)
			hookStartAt.Store(int64(since()))
			hookStart.Store(seq.Add(1))
			close(hookStarted)
		})
		return s
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start, failing (never skipping) if the engine cannot start: a skipped
	// engine would read as a pass in CI's root step, which runs without -v.
	// An io_uring start that fails only with ENOMEM is retried with a new
	// server for up to 10 s, as startC714DetachServer does: the kernel
	// charges ring memory to RLIMIT_MEMLOCK per UID and returns it only after
	// a ring closes, so at CI's 8 MiB a start right after the previous case,
	// or while another test binary holds rings, can fail with nothing leaked.
	var s *celeris.Server
	var startDone chan error
	var addr string
	retryUntil := time.Now().Add(10 * time.Second)
	for tries := 1; ; tries++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		addr = ln.Addr().String()
		if mode != drainDirectAfterStartWithListener {
			if cerr := ln.Close(); cerr != nil {
				t.Fatalf("close probe listener: %v", cerr)
			}
		}
		s = build(addr)
		startDone = make(chan error, 1)
		go func(s *celeris.Server, ln net.Listener, done chan<- error) {
			switch mode {
			case drainDirectAfterStartWithListener:
				done <- s.StartWithListener(ln)
			default:
				done <- s.StartWithContext(ctx)
			}
		}(s, ln, startDone)
		err = waitDrainOrderReady(addr, startDone)
		if err == nil {
			if tries > 1 {
				t.Logf("%s: server start retried on ring ENOMEM: %d tries", engType, tries)
			}
			break
		}
		_ = ln.Close()
		if strings.Contains(err.Error(), "cannot allocate memory") && time.Now().Before(retryUntil) {
			time.Sleep(2 * time.Millisecond)
			continue
		}
		t.Fatalf("%s: the server did not start: %v", engType, err)
	}

	type result struct {
		status int
		body   string
		err    error
	}
	res := make(chan result, 1)
	go func() {
		resp, gerr := (&http.Client{Timeout: 15 * time.Second}).Get("http://" + addr + "/slow")
		if gerr != nil {
			res <- result{err: gerr}
			return
		}
		body, rerr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		res <- result{status: resp.StatusCode, body: string(body), err: rerr}
	}()
	select {
	case <-handlerEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("the /slow handler never started")
	}

	// Start the shutdown with the request held in its handler.
	callReturned := make(chan error, 1)
	now := time.Now()
	t0.Store(&now)
	switch mode {
	case drainCancelStartWithContext:
		cancel()
		go func() {
			err := <-startDone
			callReturnAt.Store(int64(since()))
			callReturn.Store(seq.Add(1))
			callReturned <- err
		}()
	default:
		go func() {
			shutCtx, shutCancel := context.WithTimeout(context.Background(), shutdownBudget703)
			defer shutCancel()
			err := s.Shutdown(shutCtx)
			callReturnAt.Store(int64(since()))
			callReturn.Store(seq.Add(1))
			callReturned <- err
		}()
	}

	var callDone bool
	var callErr error
	select {
	case <-hookStarted:
	case callErr = <-callReturned:
		callDone = true
	case <-time.After(releaseAfter):
	}
	close(release)

	if !callDone {
		select {
		case callErr = <-callReturned:
		case <-time.After(callCap703):
			t.Fatalf("%s: the %s call did not return within %v of the handler's release (its budget is %v)", engType, mode, callCap703, shutdownBudget703)
		}
	}
	if callErr != nil {
		t.Errorf("%s: the %s call returned %v, want nil", engType, mode, callErr)
	}
	var r result
	select {
	case r = <-res:
	case <-time.After(15 * time.Second):
		t.Fatalf("%s: the in-flight request never completed", engType)
	}
	if mode != drainCancelStartWithContext {
		// The direct modes: Start is still to return.
		select {
		case err := <-startDone:
			if err != nil {
				t.Errorf("%s: Start returned %v after Shutdown, want nil", engType, err)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("%s: Start did not return within 20s of Shutdown", engType)
		}
	}
	select {
	case <-hookStarted:
	default:
		t.Fatalf("%s: the OnShutdown hook never ran", engType)
	}

	ms := func(ns int64) float64 { return float64(ns) / 1e6 }
	t.Logf("RESULT engine=%s async=%v mode=%s handler_done_ms=%.1f hook_start_ms=%.1f call_return_ms=%.1f order(handler,hook,call)=(%d,%d,%d) response=%d/%q err=%v",
		engType, async, mode, ms(handlerDoneAt.Load()), ms(hookStartAt.Load()), ms(callReturnAt.Load()),
		handlerDone.Load(), hookStart.Load(), callReturn.Load(), r.status, r.body, r.err)

	if !hookSawHandlerDone.Load() {
		t.Errorf("%s/%s: the OnShutdown hook started while the request was still in its handler (celeris#703: hooks must run after the drain)", engType, mode)
	}
	if hs, cr := hookStart.Load(), callReturn.Load(); hs == 0 || cr == 0 || cr < hs {
		t.Errorf("%s/%s: the %s call returned (event %d) before the OnShutdown hook started (event %d)", engType, mode, mode, cr, hs)
	}
	if hd, cr := handlerDone.Load(), callReturn.Load(); hd == 0 || cr < hd {
		t.Errorf("%s/%s: the %s call returned (event %d) before the in-flight handler finished (event %d)", engType, mode, mode, cr, hd)
	}
	if r.err != nil || r.status != http.StatusOK || r.body != "done" {
		t.Errorf("%s/%s: the in-flight request got status %d body %q err %v, want 200 %q", engType, mode, r.status, r.body, r.err, "done")
	}
}
