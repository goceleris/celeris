package etag

import (
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

func BenchmarkETag200(b *testing.B) {
	mw := New()
	handler := func(c *celeris.Context) error {
		return c.Blob(200, "text/plain", []byte("hello world"))
	}
	opts := []celeristest.Option{celeristest.WithHandlers(mw, handler)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkETag304(b *testing.B) {
	mw := New()
	handler := func(c *celeris.Context) error {
		return c.Blob(200, "text/plain", []byte("hello world"))
	}
	tag := expectedWeakTag("hello world")
	opts := []celeristest.Option{
		celeristest.WithHandlers(mw, handler),
		celeristest.WithHeader("if-none-match", tag),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkETagSkipPOST(b *testing.B) {
	mw := New()
	handler := func(c *celeris.Context) error {
		return c.Blob(200, "text/plain", []byte("created"))
	}
	opts := []celeristest.Option{celeristest.WithHandlers(mw, handler)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("POST", "/", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}
