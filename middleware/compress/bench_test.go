package compress

import (
	"strings"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

func benchBody() []byte {
	return []byte(strings.Repeat("The quick brown fox jumps over the lazy dog. ", 22))
}

func BenchmarkCompressGzip(b *testing.B) {
	body := benchBody()
	mw := New(Config{Encodings: []string{"gzip"}})
	handler := func(c *celeris.Context) error {
		return c.Blob(200, "application/json", body)
	}
	opts := []celeristest.Option{
		celeristest.WithHeader("accept-encoding", "gzip"),
		celeristest.WithHandlers(mw, handler),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/data", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkCompressBrotli(b *testing.B) {
	body := benchBody()
	mw := New(Config{Encodings: []string{"br"}})
	handler := func(c *celeris.Context) error {
		return c.Blob(200, "application/json", body)
	}
	opts := []celeristest.Option{
		celeristest.WithHeader("accept-encoding", "br"),
		celeristest.WithHandlers(mw, handler),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/data", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkCompressZstd(b *testing.B) {
	body := benchBody()
	mw := New(Config{Encodings: []string{"zstd"}})
	handler := func(c *celeris.Context) error {
		return c.Blob(200, "application/json", body)
	}
	opts := []celeristest.Option{
		celeristest.WithHeader("accept-encoding", "zstd"),
		celeristest.WithHandlers(mw, handler),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/data", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkCompressSkip(b *testing.B) {
	body := benchBody()
	mw := New()
	handler := func(c *celeris.Context) error {
		return c.Blob(200, "application/json", body)
	}
	opts := []celeristest.Option{
		celeristest.WithHandlers(mw, handler),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/data", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}
