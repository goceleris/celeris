package pprof

import (
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

func BenchmarkPprofPassthrough(b *testing.B) {
	mw := New(Config{AuthFunc: openAuth()})
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

func BenchmarkPprofIndex(b *testing.B) {
	mw := New(Config{AuthFunc: openAuth()})
	opts := []celeristest.Option{celeristest.WithRemoteAddr("127.0.0.1:12345")}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/debug/pprof/", opts...)
		_ = mw(ctx)
		celeristest.ReleaseContext(ctx)
	}
}
