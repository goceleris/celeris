package celeris

import (
	"context"
	"errors"
	"testing"

	"github.com/goceleris/celeris/protocol/h2/stream"
)

// celeris#836 in the root package: an HTTP/2 request's c.Context() is the
// context of that request only, made when something asks for it.

// TestContextOfH2RequestEndsWithIt836: a context taken from c.Context() is
// cancelled once the request is over and stays cancelled when the stream it
// came from is reused, and a request that never asks for it gets none.
func TestContextOfH2RequestEndsWithIt836(t *testing.T) {
	s, _ := newTestStream("GET", "/836")
	c := acquireContext(s)
	if c.ctx != nil {
		t.Fatal("acquireContext set a context: an H2 request that never asks for one must not make one")
	}
	ctx := c.Context()
	if ctx.Err() != nil {
		t.Fatalf("c.Context().Err() = %v during the request, want nil", ctx.Err())
	}
	if c.Context() != ctx {
		t.Fatal("two c.Context() calls in one request returned different contexts")
	}
	releaseContext(c)
	s.Release()
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("c.Context().Err() = %v after the request, want context.Canceled", ctx.Err())
	}

	// The next request, likely on the same pooled Stream object.
	s2, _ := newTestStream("GET", "/836")
	defer s2.Release()
	c2 := acquireContext(s2)
	defer releaseContext(c2)
	if c2.Context() == ctx {
		t.Fatal("the next request got the previous request's context")
	}
	if c2.Context().Err() != nil {
		t.Fatalf("the next request's context: Err = %v, want nil", c2.Context().Err())
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("the previous request's context: Err = %v once the next request began, want context.Canceled", ctx.Err())
	}
}

// TestH1ContextIsSetWhenAcquired836: on HTTP/1 the request's context is
// context.Background(), stored when the Context is acquired, as it was
// before #836's fix. Context() must not read the stream for it: a detached
// handler (SSE, WebSocket) may call it while the engine releases the
// stream, which the race detector reports here.
func TestH1ContextIsSetWhenAcquired836(t *testing.T) {
	s := stream.NewH1Stream(1)
	s.Method, s.Path, s.Scheme, s.Authority = "GET", "/836", "http", "localhost"
	s.ResponseWriter = &mockResponseWriter{}
	c := acquireContext(s)
	if c.ctx != context.Background() {
		t.Fatalf("acquireContext on an HTTP/1 stream set ctx %v, want context.Background()", c.ctx)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 1000 {
			if c.Context() != context.Background() {
				t.Error("c.Context() on an HTTP/1 request is not context.Background()")
				return
			}
		}
	}()
	s.Release()
	<-done
	releaseContext(c)
}

// TestDetachedH2ContextIsItsRequests836: Detach keeps the Context past the
// handler, but on HTTP/2 the processor releases the stream when HandleStream
// returns all the same. A c.Context() called after that, from the goroutine
// the handler left running, is still the request's own context, cancelled
// with the request. It must not be made from the released stream: that
// context would be live, and it would belong to the pooled stream's next use.
func TestDetachedH2ContextIsItsRequests836(t *testing.T) {
	for _, askFirst := range []bool{true, false} {
		name := "asked-before-detach"
		if !askFirst {
			name = "first-asked-after-release"
		}
		t.Run(name, func(t *testing.T) {
			s, _ := newTestStream("GET", "/836")
			c := acquireContext(s)
			var early context.Context
			if askFirst {
				early = c.Context()
			}
			done := c.Detach()
			s.Release() // what the H2 processor does when HandleStream returns
			late := c.Context()
			if askFirst && late != early {
				t.Fatal("after the stream was released, c.Context() of the detached request is not the context it returned during the request")
			}
			if !errors.Is(late.Err(), context.Canceled) {
				t.Fatalf("after the stream was released, c.Context().Err() of the detached request = %v, want context.Canceled", late.Err())
			}

			// The next use, likely of the same pooled Stream object.
			s2, _ := newTestStream("GET", "/836")
			c2 := acquireContext(s2)
			if next := c2.Context(); next == late || next.Err() != nil {
				t.Fatalf("the next request's context: same as the detached one's: %v, Err = %v; want a live context of its own", next == late, next.Err())
			}
			if c.Context() != late || !errors.Is(late.Err(), context.Canceled) {
				t.Fatal("the detached request's context changed, or went live, once the next request began")
			}
			releaseContext(c2)
			s2.Release()
			done()
			releaseContext(c)
		})
	}
}

// BenchmarkDetachH2836 measures Detach on an HTTP/2 request whose handler
// has not asked for c.Context(): Detach keeps the request's context, which
// makes it (celeris#836).
func BenchmarkDetachH2836(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		s, _ := newTestStream("GET", "/836")
		c := acquireContext(s)
		done := c.Detach()
		s.Release()
		done()
		releaseContext(c)
	}
}

// BenchmarkHandleStreamContext836 is BenchmarkHandleStreamFull with a
// handler that asks for c.Context(), as logger, timeout, otel and cache do:
// on HTTP/2 that makes the request's context (celeris#836).
func BenchmarkHandleStreamContext836(b *testing.B) {
	srv := New(Config{})
	srv.Use(func(c *Context) error {
		_ = c.Context().Err()
		return c.Next()
	})
	srv.GET("/users/:id", func(c *Context) error {
		return c.String(200, "user-%s", c.Param("id"))
	})

	adapter := &routerAdapter{server: srv}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		st, _ := newTestStream("GET", "/users/42")
		_ = adapter.HandleStream(context.Background(), st)
		st.Release()
	}
}
