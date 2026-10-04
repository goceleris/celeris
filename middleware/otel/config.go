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
	// String and string-slice values are copied first (one allocation per
	// string): the span keeps them, and a request string is only valid
	// during the request on epoll and io_uring. Keys are not copied.
	CustomAttributes func(c *celeris.Context) []attribute.KeyValue

	// CustomMetricAttributes is called per-request and appended to the metric attributes.
	// String values are copied as for CustomAttributes.
	CustomMetricAttributes func(c *celeris.Context) []attribute.KeyValue

	// ServerPort, when > 0, adds the "server.port" attribute to spans and metrics.
	// Negative values are clamped to 0 in validate().
	ServerPort int

	// MetricServerAddress adds the "server.address" attribute (the
	// request's Host, or :authority on HTTP/2) to the metric attribute
	// sets. Default: false; spans always carry it. The value comes from
	// the client, so each distinct Host makes new series: a client that
	// sends made-up Host values fills an instrument up to the SDK's
	// cardinality limit (2000 by default), after which every new attribute
	// set lands in one overflow series, and without a limit memory grows
	// with it. OTel's semantic conventions make server.address Opt-In on
	// the HTTP server metrics for that reason. Enable it only where the
	// Host is bounded upstream, for example by a proxy that rejects
	// unknown hosts (celeris#924).
	MetricServerAddress bool

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
