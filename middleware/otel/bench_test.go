package otel

import (
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"

	metricnoop "go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

func BenchmarkOTelTracing(b *testing.B) {
	tp := tracenoop.NewTracerProvider()
	mp := metricnoop.NewMeterProvider()
	mw := New(Config{TracerProvider: tp, MeterProvider: mp})
	noopH := func(c *celeris.Context) error { return c.String(200, "ok") }
	opts := []celeristest.Option{celeristest.WithHandlers(mw, noopH)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/api/users", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkOTelSkipPath(b *testing.B) {
	tp := tracenoop.NewTracerProvider()
	mp := metricnoop.NewMeterProvider()
	mw := New(Config{TracerProvider: tp, MeterProvider: mp, SkipPaths: []string{"/health"}})
	noopH := func(c *celeris.Context) error { return c.String(200, "ok") }
	opts := []celeristest.Option{celeristest.WithHandlers(mw, noopH)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/health", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkOTelSkipFunc(b *testing.B) {
	tp := tracenoop.NewTracerProvider()
	mp := metricnoop.NewMeterProvider()
	mw := New(Config{
		TracerProvider: tp,
		MeterProvider:  mp,
		Skip:           func(_ *celeris.Context) bool { return true },
	})
	noopH := func(c *celeris.Context) error { return c.String(200, "ok") }
	opts := []celeristest.Option{celeristest.WithHandlers(mw, noopH)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/api/users", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}
