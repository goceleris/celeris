package otel

import (
	"testing"

	"go.opentelemetry.io/otel/attribute"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// The per-request cost of copying CustomAttributes and
// CustomMetricAttributes (appendOwned, celeris#742), with no-op providers so
// the copies are most of what differs. Strings: two STRING attributes read
// from headers, the common shape. Nested: the same plus a SLICE and a MAP
// holding request strings.
func benchCustomAttrs(b *testing.B, attrs func(c *celeris.Context) []attribute.KeyValue) {
	mw := New(Config{
		TracerProvider:         tracenoop.NewTracerProvider(),
		MeterProvider:          metricnoop.NewMeterProvider(),
		CustomAttributes:       attrs,
		CustomMetricAttributes: attrs,
	})
	h := func(c *celeris.Context) error { return c.String(200, "ok") }
	opts := []celeristest.Option{
		celeristest.WithHandlers(mw, h),
		celeristest.WithHeader("x-tenant", "tenant-0123456789"),
		celeristest.WithHeader("x-region", "eu-west-1"),
	}
	b.ReportAllocs()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/api/users", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkOTelCustomAttributesStrings742(b *testing.B) {
	benchCustomAttrs(b, func(c *celeris.Context) []attribute.KeyValue {
		return []attribute.KeyValue{
			attribute.String("tenant", c.Header("x-tenant")),
			attribute.String("region", c.Header("x-region")),
		}
	})
}

func BenchmarkOTelCustomAttributesNested742(b *testing.B) {
	benchCustomAttrs(b, func(c *celeris.Context) []attribute.KeyValue {
		return []attribute.KeyValue{
			attribute.String("tenant", c.Header("x-tenant")),
			attribute.String("region", c.Header("x-region")),
			attribute.Slice("tags", attribute.StringValue(c.Header("x-tenant")), attribute.IntValue(1)),
			attribute.Map("labels", attribute.String("region", c.Header("x-region"))),
		}
	})
}

// Numeric: no string value at all. appendOwned still copies the attribute
// keys, which can be request strings too: one allocation per call where
// main made none.
func BenchmarkOTelCustomAttributesNumeric742(b *testing.B) {
	benchCustomAttrs(b, func(c *celeris.Context) []attribute.KeyValue {
		return []attribute.KeyValue{
			attribute.Int("shard", 7),
			attribute.Bool("beta", true),
		}
	})
}
