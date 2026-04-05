package otel_test

import (
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/goceleris/celeris"

	"github.com/goceleris/celeris/middleware/otel"
)

func ExampleNew() {
	// Zero-config: uses global OTel TracerProvider, MeterProvider, and Propagators.
	_ = otel.New()
}

func ExampleNew_withSkipPaths() {
	// Skip health and readiness endpoints from tracing.
	_ = otel.New(otel.Config{
		SkipPaths: []string{"/health", "/ready"},
	})
}

func ExampleNew_withSpanNameFormatter() {
	// Custom span names.
	_ = otel.New(otel.Config{
		SpanNameFormatter: func(c *celeris.Context) string {
			return c.Method() + " " + c.Path()
		},
	})
}

func ExampleNew_withProvider() {
	// Explicit TracerProvider with an in-memory exporter (for testing).
	// In production, replace with an OTLP exporter.
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))

	_ = otel.New(otel.Config{
		TracerProvider: tp,
	})
}

func ExampleNew_filter() {
	// Allow-list filter: only trace requests to /api/*.
	_ = otel.New(otel.Config{
		Filter: func(c *celeris.Context) bool {
			return len(c.Path()) >= 4 && c.Path()[:4] == "/api"
		},
	})
}
