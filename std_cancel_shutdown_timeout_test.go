package celeris_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// TestStartWithContextCancelKeepsShutdownTimeoutOnStd pins celeris#753 where
// a user meets it. On std, a cancel of StartWithContext's context ignored
// Config.ShutdownTimeout: a handler still running when the budget ran out
// held the drain, the OnShutdown hooks and the StartWithContext call until it
// returned, however long that was. A direct Shutdown kept to its ctx.
//
// The handler here ignores cancellation and is held until every assertion
// has run, so a shutdown that waits for it fails every time rather than
// passing late. Each shape (a handler that ignores its context, and one that
// waits on c.Context(), which std's HTTP/1.1 path never cancels) must see the
// hook start once the budget has run out, and StartWithContext return, within
// bound of the budget, with the handler still held. The request itself is
// left to finish, as http.Server.Shutdown leaves it.
func TestStartWithContextCancelKeepsShutdownTimeoutOnStd(t *testing.T) {
	for _, shape := range []string{"ignores-ctx", "waits-on-c.Context"} {
		t.Run(shape, func(t *testing.T) {
			runStdCancelBudgetCase(t, shape == "waits-on-c.Context")
		})
	}
}

func runStdCancelBudgetCase(t *testing.T, watchCtx bool) {
	const budget = 300 * time.Millisecond
	const bound = 3 * time.Second

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHandler := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseHandler()
	var handlerDone atomic.Bool

	s := celeris.New(celeris.Config{Engine: celeris.Std, Addr: addr, ShutdownTimeout: budget})
	s.GET("/ping", func(c *celeris.Context) error { return c.String(http.StatusOK, "ok") })
	s.GET("/held", func(c *celeris.Context) error {
		close(entered)
		if watchCtx {
			select {
			case <-c.Context().Done():
			case <-release:
			case <-time.After(20 * time.Second):
			}
		} else {
			select {
			case <-release:
			case <-time.After(20 * time.Second):
			}
		}
		handlerDone.Store(true)
		return c.String(http.StatusOK, "done")
	})
	var t0 atomic.Pointer[time.Time]
	hookAt := make(chan time.Duration, 1)
	s.OnShutdown(func(context.Context) {
		if p := t0.Load(); p != nil {
			hookAt <- time.Since(*p)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startDone := make(chan error, 1)
	go func() { startDone <- s.StartWithContext(ctx) }()
	probe := &http.Client{Timeout: 300 * time.Millisecond}
	for deadline := time.Now().Add(15 * time.Second); ; {
		if resp, err := probe.Get("http://" + addr + "/ping"); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("server not ready within 15s")
		}
		time.Sleep(20 * time.Millisecond)
	}

	type result struct {
		body string
		err  error
	}
	respCh := make(chan result, 1)
	cl := &http.Client{Timeout: 30 * time.Second}
	defer cl.CloseIdleConnections()
	go func() {
		resp, err := cl.Get("http://" + addr + "/held")
		if err != nil {
			respCh <- result{err: err}
			return
		}
		b, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		respCh <- result{string(b), err}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not start within 5s")
	}

	now := time.Now()
	t0.Store(&now)
	cancel()

	select {
	case at := <-hookAt:
		if at < budget {
			t.Errorf("the hook ran %v after the cancel, before the %v budget ran out, with the handler still running", at, budget)
		}
	case <-time.After(budget + bound):
		t.Fatalf("no OnShutdown hook %v after the cancel with ShutdownTimeout %v: the drain waits for the handler (celeris#753)", budget+bound, budget)
	}
	select {
	case err := <-startDone:
		if err != nil {
			t.Errorf("StartWithContext: %v, want nil", err)
		}
	case <-time.After(bound):
		t.Fatalf("StartWithContext had not returned %v after the hook ran: it waits for the handler (celeris#753)", bound)
	}
	if handlerDone.Load() {
		t.Fatal("precondition: the handler returned before its release")
	}

	releaseHandler()
	select {
	case r := <-respCh:
		if r.err != nil || r.body != "done" {
			t.Errorf("the held request answered %q, %v; want \"done\"", r.body, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Error("the held request did not complete within 5s of its release")
	}
}
