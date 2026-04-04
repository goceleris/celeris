// Package metrics provides Prometheus metrics middleware for celeris.
//
// The middleware records per-request metrics (counters, histograms, gauges)
// and exposes them in Prometheus text exposition format at a configurable
// endpoint (default "/metrics").
//
// Basic usage:
//
//	server.Use(metrics.New())
//
// Custom configuration:
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
//
// # Custom Labels
//
// Use [Config].LabelFuncs to add custom label dimensions. Functions are
// called after c.Next() returns:
//
//	server.Use(metrics.New(metrics.Config{
//	    LabelFuncs: map[string]func(*celeris.Context) string{
//	        "region": func(c *celeris.Context) string {
//	            return c.Header("x-region")
//	        },
//	    },
//	}))
//
// # Cardinality Protection
//
// Unmatched routes (404 with no registered pattern) use the sentinel path
// label "<unmatched>" to prevent high-cardinality label explosion.
//
// # Histogram Buckets
//
// [DefaultBuckets] provides fine-grained latency boundaries:
//
//	[]float64{0.0005, 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 5}
//
// Override with [Config].Buckets for duration histograms or
// [Config].SizeBuckets for request/response size histograms.
//
// # Registry Isolation
//
// By default, the middleware creates a dedicated [prometheus.Registry] with
// Go runtime and process collectors. Pass a custom [Config].Registry to use
// a shared or pre-configured registry.
package metrics
