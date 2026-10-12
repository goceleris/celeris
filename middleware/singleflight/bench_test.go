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

// BenchmarkDefaultKeyBrowser is the default key of a browser's conditional
// request: a blank-separated Accept-Encoding and two validators (celeris#888
// items 23 and 24).
func BenchmarkDefaultKeyBrowser(b *testing.B) {
	c, _ := celeristest.NewContext("GET", "/api/items",
		celeristest.WithQuery("page", "2"),
		celeristest.WithHeader("accept-encoding", "gzip, deflate, br, zstd"),
		celeristest.WithHeader("if-none-match", `"v1"`),
		celeristest.WithHeader("if-modified-since", "Mon, 05 Oct 2026 10:00:00 GMT"))
	defer celeristest.ReleaseContext(c)
	b.ReportAllocs()
	for b.Loop() {
		_ = defaultKeyFunc(c)
	}
}

// BenchmarkDefaultKeyPlain is the default key of a request that carries none
// of the headers it holds.
func BenchmarkDefaultKeyPlain(b *testing.B) {
	c, _ := celeristest.NewContext("GET", "/api/items")
	defer celeristest.ReleaseContext(c)
	b.ReportAllocs()
	for b.Loop() {
		_ = defaultKeyFunc(c)
	}
}
