package metrics

import (
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/prometheus/client_golang/prometheus"
)

func BenchmarkMetricsPassthrough(b *testing.B) {
	reg := prometheus.NewRegistry()
	mw := New(Config{Registry: reg})
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

func BenchmarkMetricsEndpoint(b *testing.B) {
	reg := prometheus.NewRegistry()
	mw := New(Config{Registry: reg})
	noop := func(_ *celeris.Context) error { return nil }
	opts := []celeristest.Option{celeristest.WithHandlers(mw, noop)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/metrics", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkMetricsSkipPath(b *testing.B) {
	reg := prometheus.NewRegistry()
	mw := New(Config{Registry: reg, SkipPaths: []string{"/health"}})
	noop := func(_ *celeris.Context) error { return nil }
	opts := []celeristest.Option{celeristest.WithHandlers(mw, noop)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/health", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}
