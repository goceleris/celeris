// Package metrics provides Prometheus metrics middleware for celeris.
//
// The middleware records per-request metrics (counters, histograms, gauges)
// and exposes them in Prometheus text exposition format at a configurable
// endpoint (default "/metrics").
//
// Register with default settings:
//
//	server.Use(metrics.New())
//
// Or supply a [Config] to customise the endpoint path, namespace, subsystem,
// histogram buckets, label extensions, or the backing [prometheus.Registry]:
//
//	server.Use(metrics.New(metrics.Config{
//	    Path:      "/prom",
//	    Namespace: "myapp",
//	    Subsystem: "http",
//	    SkipPaths: []string{"/health", "/ready"},
//	}))
//
// # Security Warning
//
// The metrics endpoint exposes internal server metrics. In production,
// protect it using [Config].AuthFunc, a separate listener, authentication
// middleware, or network-level restriction.
//
// # Registered Metrics
//
//   - {ns}_{sub}_requests_total (CounterVec): total HTTP requests
//   - {ns}_{sub}_request_duration_seconds (HistogramVec): request latency
//   - {ns}_{sub}_request_size_bytes (HistogramVec): request body size
//   - {ns}_{sub}_response_size_bytes (HistogramVec): response body size
//   - {ns}_{sub}_active_requests (Gauge): in-flight requests
//
// The subsystem segment is omitted when [Config].Subsystem is empty.
// Base labels on all metrics are method, path, and status. Add custom
// dimensions via [Config].LabelFuncs; keys must not conflict with the
// three reserved base labels.
//
// # Cardinality Protection
//
// Unmatched routes (404 with no registered pattern) use the sentinel path
// label "<unmatched>" to prevent high-cardinality label explosion.
//
// # Histogram Buckets
//
// [DefaultBuckets] provides fine-grained sub-millisecond latency boundaries.
// Override with [Config].Buckets for duration histograms or
// [Config].SizeBuckets for request/response size histograms.
//
// # Registry Isolation
//
// By default, the middleware creates a dedicated [prometheus.Registry] with
// Go runtime and process collectors. Pass a custom [Config].Registry to use
// a shared or pre-configured registry.
//
// # Response Size and Compression
//
// The response_size_bytes metric records c.BytesWritten() at the point
// metrics middleware runs in the chain. With the recommended ordering
// (metrics before compress), this records the uncompressed application-level
// size. If metrics runs after compress, it records the compressed network-level
// size. Be aware of this when interpreting response size dashboards.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/observability
package metrics
