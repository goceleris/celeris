// Package otel provides OpenTelemetry tracing and metrics middleware
// for celeris.
//
// The middleware creates a server span per request, propagates context
// via configured propagators, and records request duration and active
// request count as OTel metrics.
//
// Basic usage with global providers:
//
//	server.Use(otel.New())
//
// Explicit providers:
//
//	server.Use(otel.New(otel.Config{
//	    TracerProvider: tp,
//	    MeterProvider:  mp,
//	}))
//
// Downstream handlers can access the span via [SpanFromContext]:
//
//	span := otel.SpanFromContext(c)
//
// Spans are named "METHOD /route/pattern" by default; override with
// [Config].SpanNameFormatter. Attributes follow OTel semconv v1.32.0.
//
// PII controls: client.address is opt-in ([Config].CollectClientIP);
// user_agent.original is opt-out ([Config].CollectUserAgent).
//
// Filtering: use [Config].Skip, [Config].SkipPaths, or [Config].Filter
// to exclude endpoints from tracing.
//
// Metrics recorded: http.server.request.duration (s),
// http.server.active_requests, http.server.request.body.size (By),
// http.server.response.body.size (By). Set [Config].DisableMetrics for
// tracing-only mode. Use [Config].CustomAttributes and
// [Config].CustomMetricAttributes for per-request attribute injection.
//
// # Response Size and Compression
//
// The http.response.body.size metric records c.BytesWritten() at the point
// the OTel middleware runs. With the recommended ordering (otel before
// compress), this reflects uncompressed application-level sizes. Changing
// the middleware order to place otel after compress would record compressed
// sizes instead.
package otel
