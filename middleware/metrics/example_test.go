package metrics_test

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/goceleris/celeris/middleware/metrics"
)

func ExampleNew() {
	// Zero-config: registers metrics with a dedicated registry and
	// serves them at /metrics in Prometheus text format.
	_ = metrics.New()
}

func ExampleNew_customPath() {
	// Serve metrics at a custom path with a custom namespace.
	_ = metrics.New(metrics.Config{
		Path:      "/prom",
		Namespace: "myapp",
		SkipPaths: []string{"/health", "/ready"},
	})
}

func ExampleNew_customRegistry() {
	// Use a custom Prometheus registry for isolated metric collection.
	reg := prometheus.NewRegistry()
	_ = metrics.New(metrics.Config{
		Registry:  reg,
		Namespace: "gateway",
		Subsystem: "http",
	})
}
