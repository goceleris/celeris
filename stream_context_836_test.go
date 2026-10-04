package celeris

import (
	"context"
	"errors"
	"testing"
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
