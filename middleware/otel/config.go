package otel

import (
	"github.com/goceleris/celeris"

	otelglobal "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Config defines the OpenTelemetry middleware configuration.
type Config struct {
	// Skip defines a function to skip this middleware for certain requests.
	// When Skip returns true, the request bypasses tracing entirely.
	Skip func(c *celeris.Context) bool

	// SkipPaths lists paths to skip (exact match on c.Path()).
	SkipPaths []string

	// TracerProvider is the OTel TracerProvider to use.
	// Default: otel.GetTracerProvider().
	TracerProvider trace.TracerProvider

	// MeterProvider is the OTel MeterProvider to use.
	// Default: otel.GetMeterProvider().
	MeterProvider metric.MeterProvider

	// Propagators is the propagator to use for context injection/extraction.
	// Default: otel.GetTextMapPropagator().
	Propagators propagation.TextMapPropagator

	// SpanNameFormatter overrides the default span name ("METHOD /route").
	SpanNameFormatter func(c *celeris.Context) string

	// Filter provides an allow-list predicate. When non-nil, requests for
	// which Filter returns false are skipped. Kept for OTel ecosystem
	// convention compatibility (inverse of Skip).
	Filter func(c *celeris.Context) bool

	// DisableMetrics skips all metric instrument creation and recording.
	// When true, only tracing is active.
	DisableMetrics bool

	// CollectClientIP enables recording the client IP address in the
	// "client.address" span attribute. Default: false (PII opt-in).
	CollectClientIP bool

	// CollectUserAgent enables recording the User-Agent header in the
	// "user_agent.original" span attribute. Default: true for backwards
	// compatibility.
	CollectUserAgent *bool

	// CustomAttributes is called per-request and appended to the span attributes.
	CustomAttributes func(c *celeris.Context) []attribute.KeyValue

	// CustomMetricAttributes is called per-request and appended to the metric attributes.
	CustomMetricAttributes func(c *celeris.Context) []attribute.KeyValue

	// ServerPort, when > 0, adds the "server.port" attribute to spans and metrics.
	// Negative values are clamped to 0 in validate().
	ServerPort int

	// NOTE: url.query is intentionally omitted from span attributes because
	// query parameters frequently contain PII (tokens, emails, session IDs).
	// Callers who need it can add it via CustomAttributes.
}

var defaultConfig = Config{}

func applyDefaults(cfg Config) Config {
	if cfg.TracerProvider == nil {
		cfg.TracerProvider = otelglobal.GetTracerProvider()
	}
	if cfg.MeterProvider == nil {
		cfg.MeterProvider = otelglobal.GetMeterProvider()
	}
	if cfg.Propagators == nil {
		cfg.Propagators = otelglobal.GetTextMapPropagator()
	}
	return cfg
}

func (cfg *Config) validate() {
	if cfg.ServerPort < 0 {
		cfg.ServerPort = 0
	}
}
