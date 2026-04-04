package csrf

import (
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

func BenchmarkCSRFSafeMethod(b *testing.B) {
	mw := New()
	noop := func(_ *celeris.Context) error { return nil }
	opts := []celeristest.Option{celeristest.WithHandlers(mw, noop)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkCSRFUnsafeMethod(b *testing.B) {
	token := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	mw := New()
	noop := func(_ *celeris.Context) error { return nil }
	opts := []celeristest.Option{
		celeristest.WithHeader("x-csrf-token", token),
		celeristest.WithCookie("_csrf", token),
		celeristest.WithHandlers(mw, noop),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("POST", "/submit", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkCSRFSkipPath(b *testing.B) {
	mw := New(Config{SkipPaths: []string{"/health"}})
	noop := func(_ *celeris.Context) error { return nil }
	opts := []celeristest.Option{celeristest.WithHandlers(mw, noop)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("POST", "/health", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}
