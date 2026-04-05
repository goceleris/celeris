package debug

import (
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/observe"
)

func BenchmarkDebugPassthrough(b *testing.B) {
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

func BenchmarkDebugStatus(b *testing.B) {
	mw := New(Config{AuthFunc: openAuth()})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/debug/celeris/status")
		_ = mw(ctx)
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkDebugMetrics(b *testing.B) {
	col := observe.NewCollector()
	mw := New(Config{Collector: col, AuthFunc: openAuth()})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/debug/celeris/metrics")
		_ = mw(ctx)
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkDebugConfig(b *testing.B) {
	mw := New(Config{AuthFunc: openAuth()})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/debug/celeris/config")
		_ = mw(ctx)
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkDebugMemory(b *testing.B) {
	mw := New(Config{AuthFunc: openAuth()})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/debug/celeris/memory")
		_ = mw(ctx)
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkDebugBuild(b *testing.B) {
	mw := New(Config{AuthFunc: openAuth()})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/debug/celeris/build")
		_ = mw(ctx)
		celeristest.ReleaseContext(ctx)
	}
}
