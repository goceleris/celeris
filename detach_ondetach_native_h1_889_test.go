package celeris

import (
	"bytes"
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/conn"
)

// The native HTTP/1 engines (epoll, io_uring) keep one stream, and its
// CachedCtx, for the life of a keep-alive connection. A panic out of OnDetach
// before the engine published H1State.Detached leaves that connection open
// (sync mode, and async mode before the connection is promoted), so the
// Context the panic leaves behind is still the connection's: it must not go
// to contextPool while s.CachedCtx points at it (#889 item 33, review of
// PR #1014; the celeris#512 class).

type nativeH1Rig struct {
	adapter *routerAdapter
	state   *conn.H1State
	out     bytes.Buffer
	ctxs    chan *Context
	entry   chan bool // c.detached at the entry of /ok
}

func newNativeH1Rig(onDetach func(*conn.H1State)) *nativeH1Rig {
	r := &nativeH1Rig{ctxs: make(chan *Context, 8), entry: make(chan bool, 8)}
	srv := New(Config{})
	srv.GET("/d", func(c *Context) error {
		r.ctxs <- c
		c.Detach()
		return nil
	})
	srv.GET("/ok", func(c *Context) error {
		r.entry <- c.detached
		r.ctxs <- c
		return c.String(200, "ok")
	})
	r.adapter = &routerAdapter{server: srv}
	r.state = conn.NewH1State()
	r.state.OnDetach = func() {
		if onDetach != nil {
			onDetach(r.state)
		}
		panic("engine fault in OnDetach")
	}
	return r
}

func (r *nativeH1Rig) feed(t *testing.T, data string) {
	t.Helper()
	err := conn.ProcessH1(context.Background(), []byte(data), r.state, r.adapter,
		func(b []byte) { r.out.Write(b) })
	if err != nil {
		t.Fatalf("ProcessH1: %v", err)
	}
}

// TestDetachOnDetachPanicKeepAliveConn889: request 1 panics out of OnDetach and
// the connection stays open; request 2 arrives on the same stream.
func TestDetachOnDetachPanicKeepAliveConn889(t *testing.T) {
	r := newNativeH1Rig(nil)
	r.feed(t, "GET /d HTTP/1.1\r\nHost: x\r\n\r\n")
	c1 := <-r.ctxs
	// The release goroutine of the old code runs asynchronously; give it the
	// time to Put the Context (it must not exist, or must not release).
	time.Sleep(100 * time.Millisecond)

	if !strings.Contains(r.out.String(), "HTTP/1.1 500") {
		t.Errorf("no 500 response for the request whose Detach panicked; got %q", r.out.String())
	}
	snap := r.adapter.server.Collector().Snapshot()
	if snap.RequestsTotal != 1 || snap.ErrorsTotal != 1 {
		t.Errorf("request 1 recorded as requests=%d errors=%d, want 1 and 1 (a 500)", snap.RequestsTotal, snap.ErrorsTotal)
	}
	if c1.pooled {
		t.Fatal("ALIAS: the Context is in contextPool while the open connection still caches it (s.CachedCtx)")
	}

	r.feed(t, "GET /ok HTTP/1.1\r\nHost: x\r\n\r\n")
	if stale := <-r.entry; stale {
		t.Error("request 2 ran on a Context still marked detached")
	}
	c2 := <-r.ctxs
	if c2.pooled {
		t.Error("request 2's Context is marked pooled while in use")
	}
	if !strings.Contains(r.out.String(), "HTTP/1.1 200") {
		t.Errorf("request 2 got no 200: %q", r.out.String())
	}

	// The connection closes: the Context the stream cached (request 2's, the
	// same object as request 1's) is released to the pool, and only now.
	conn.CloseH1(r.state)
	if !c1.pooled {
		t.Error("the connection closed and its cached Context was not released to the pool")
	}
	if c2 != c1 {
		t.Error("request 2 did not reuse the connection's cached Context")
	}
}

// TestDetachOnDetachPanicPipelinedRace889: both requests in one ProcessH1
// call, so request 2's acquireContext runs while a release goroutine, if the
// code starts one, resets the same Context. Meaningful under -race.
func TestDetachOnDetachPanicPipelinedRace889(t *testing.T) {
	r := newNativeH1Rig(nil)
	r.feed(t, "GET /d HTTP/1.1\r\nHost: x\r\n\r\nGET /ok HTTP/1.1\r\nHost: x\r\n\r\n")
	<-r.ctxs
	if stale := <-r.entry; stale {
		t.Error("request 2 ran on a Context still marked detached")
	}
	<-r.ctxs
	time.Sleep(100 * time.Millisecond)
	out := r.out.String()
	if !strings.Contains(out, "HTTP/1.1 500") || !strings.Contains(out, "HTTP/1.1 200") {
		t.Errorf("want a 500 then a 200, got %q", out)
	}
}

// TestDetachOnDetachPanicAfterEnginePublishedDetached889: the async-promoted
// epoll and io_uring path, where the engine's deferred hook publishes
// H1State.Detached and closes the connection before the panic reaches the
// router. The engine then skips CloseH1, so nothing else releases the
// Context, and it must still not go to the pool while the stream caches it.
func TestDetachOnDetachPanicAfterEnginePublishedDetached889(t *testing.T) {
	r := newNativeH1Rig(func(s *conn.H1State) { s.Detached.Store(true) })
	baseline := runtime.NumGoroutine()
	r.feed(t, "GET /d HTTP/1.1\r\nHost: x\r\n\r\n")
	c1 := <-r.ctxs
	time.Sleep(100 * time.Millisecond)
	if c1.pooled {
		t.Error("ALIAS: the Context is in contextPool while the stream still caches it")
	}
	if n := runtime.NumGoroutine(); n > baseline {
		t.Errorf("%d goroutines after the request, %d before: a release waiter is parked", n, baseline)
	}
}
