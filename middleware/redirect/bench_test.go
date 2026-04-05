package redirect

import (
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

func BenchmarkHTTPSRedirect(b *testing.B) {
	mw := HTTPSRedirect()
	noop := func(_ *celeris.Context) error { return nil }
	opts := []celeristest.Option{
		celeristest.WithHandlers(mw, noop),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/hello", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkHTTPSNoRedirect(b *testing.B) {
	mw := HTTPSRedirect()
	noop := func(_ *celeris.Context) error { return nil }
	opts := []celeristest.Option{
		celeristest.WithHandlers(mw, noop),
		celeristest.WithHeader("x-forwarded-proto", "https"),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/hello", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkTrailingSlash(b *testing.B) {
	mw := TrailingSlashRedirect()
	noop := func(_ *celeris.Context) error { return nil }
	opts := []celeristest.Option{
		celeristest.WithHandlers(mw, noop),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/foo", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkHTTPSRedirectWithQuery(b *testing.B) {
	mw := HTTPSRedirect()
	noop := func(_ *celeris.Context) error { return nil }
	opts := []celeristest.Option{
		celeristest.WithHandlers(mw, noop),
		celeristest.WithQuery("page", "2"),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/hello", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkHTTPSWWWRedirect(b *testing.B) {
	mw := HTTPSWWWRedirect()
	noop := func(_ *celeris.Context) error { return nil }
	opts := []celeristest.Option{
		celeristest.WithHandlers(mw, noop),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/path", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkTrailingSlashRewrite(b *testing.B) {
	mw := TrailingSlashRewrite()
	noop := func(_ *celeris.Context) error { return nil }
	opts := []celeristest.Option{
		celeristest.WithHandlers(mw, noop),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/path", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkRemoveTrailingSlashRewrite(b *testing.B) {
	mw := RemoveTrailingSlashRewrite()
	noop := func(_ *celeris.Context) error { return nil }
	opts := []celeristest.Option{
		celeristest.WithHandlers(mw, noop),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/path/", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}
