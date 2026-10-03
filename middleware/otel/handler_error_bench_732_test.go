package otel

import (
	"errors"
	"testing"

	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// The per-request cost of recording a handler's error on a span that
// records (celeris#732): an SDK tracer provider with no span processor, so
// every span is sampled and recorded and nothing is exported. The error's
// message is a request string, as errors.New(c.Param("id")) gives.
func BenchmarkOTelHandlerError732(b *testing.B) {
	mw := New(Config{
		TracerProvider: sdktrace.NewTracerProvider(),
		MeterProvider:  metricnoop.NewMeterProvider(),
	})
	h := func(c *celeris.Context) error { return errors.New(c.Header("x-id")) }
	opts := []celeristest.Option{
		celeristest.WithHandlers(mw, h),
		celeristest.WithHeader("x-id", "user-0123456789"),
	}
	b.ReportAllocs()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/api/users", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}
