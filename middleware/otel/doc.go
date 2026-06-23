// Package otel provides OpenTelemetry tracing and metrics middleware
// for celeris.
//
// [New] returns a [celeris.HandlerFunc] that creates a server span per
// request, propagates context via configured propagators, and records
// request duration and active request count as OTel metrics. By default
// it reads the global TracerProvider, MeterProvider, and TextMapPropagator;
// supply a [Config] to use explicit providers.
//
// Downstream handlers retrieve the active span with [SpanFromContext].
// Span names default to "METHOD /route/pattern"; override with
// [Config].SpanNameFormatter. Attributes follow OTel semconv v1.32.0.
//
// Metrics recorded: http.server.request.duration (s),
// http.server.active_requests, http.server.request.body.size (By),
// http.server.response.body.size (By). Set [Config].DisableMetrics for
// tracing-only mode.
//
// PII controls: client.address is opt-in ([Config].CollectClientIP);
// user_agent.original is opt-out ([Config].CollectUserAgent).
// Use [Config].Skip, [Config].SkipPaths, or [Config].Filter to exclude
// endpoints from tracing.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/observability
package otel
