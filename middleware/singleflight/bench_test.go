package singleflight

import (
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

func BenchmarkSingleflightMiss(b *testing.B) {
	mw := New()
	handler := func(c *celeris.Context) error {
		return c.Blob(200, "text/plain", []byte("hello"))
	}
	opts := []celeristest.Option{celeristest.WithHandlers(mw, handler)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/bench", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkSingleflightSkip(b *testing.B) {
	mw := New(Config{SkipPaths: []string{"/skip"}})
	handler := func(c *celeris.Context) error {
		return c.Blob(200, "text/plain", []byte("hello"))
	}
	opts := []celeristest.Option{celeristest.WithHandlers(mw, handler)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/skip", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}
