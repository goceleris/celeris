package healthcheck

import (
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

func BenchmarkHealthcheckPassthrough(b *testing.B) {
	mw := New()
	noop := func(_ *celeris.Context) error { return nil }
	opts := []celeristest.Option{celeristest.WithHandlers(mw, noop)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/api/users", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkHealthcheckLive(b *testing.B) {
	mw := New()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/livez")
		_ = mw(ctx)
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkHealthcheckReady(b *testing.B) {
	mw := New()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/readyz")
		_ = mw(ctx)
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkHealthcheckStartup(b *testing.B) {
	mw := New()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/startupz")
		_ = mw(ctx)
		celeristest.ReleaseContext(ctx)
	}
}
