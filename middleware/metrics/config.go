package metrics

import (
	"fmt"

	"github.com/goceleris/celeris"
	"github.com/prometheus/client_golang/prometheus"
)

// DefaultBuckets returns the default histogram bucket boundaries for
// sub-millisecond latency resolution.
func DefaultBuckets() []float64 {
	dst := make([]float64, len(defaultBuckets))
	copy(dst, defaultBuckets)
	return dst
}

var defaultBuckets = []float64{0.0005, 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 5}

// Config defines the metrics middleware configuration.
type Config struct {
	// Skip defines a function to skip this middleware for certain requests.
	Skip func(c *celeris.Context) bool

	// SkipPaths lists paths to skip from metrics recording (exact match).
	// The metrics endpoint itself is never recorded regardless of this list.
	SkipPaths []string

	// Path is the HTTP path that serves the Prometheus metrics endpoint.
	// Default: "/metrics".
	Path string

	// Namespace is the Prometheus metric namespace prefix.
	// Default: "celeris".
	Namespace string

	// Subsystem is the Prometheus metric subsystem prefix.
	// Default: "" (no subsystem).
	Subsystem string

	// Buckets defines the histogram bucket boundaries for request duration.
	// Default: DefaultBuckets.
	Buckets []float64

	// Registry is the Prometheus registry to use for metric registration and
	// gathering. When nil, the middleware creates a dedicated registry with
	// Go runtime and process collectors pre-registered.
	Registry *prometheus.Registry

	// ConstLabels are applied to every metric registration as constant labels
	// (e.g., service name, environment).
	ConstLabels map[string]string

	// AuthFunc is an authentication check executed before serving the metrics
	// endpoint. If it returns false, the middleware responds with 403 Forbidden.
	// Default: nil (no auth, all requests to the metrics endpoint are served).
	AuthFunc func(c *celeris.Context) bool

	// IgnoreStatusCodes excludes requests with matching response status codes
	// from all metric recording (counter, histograms). Common use:
	// IgnoreStatusCodes: []int{404} to suppress scanner noise.
	IgnoreStatusCodes []int

	// LabelFuncs defines custom label dimensions appended to all metric
	// label sets. Each map key becomes a label name and the function extracts
	// the label value from the request context. The functions are called
	// after c.Next() returns, so response-derived values are available.
	LabelFuncs map[string]func(*celeris.Context) string

	// SizeBuckets defines histogram bucket boundaries for request and
	// response size histograms. Defaults to exponential byte-size buckets.
	SizeBuckets []float64
}

var defaultConfig = Config{}

func applyDefaults(cfg Config) Config {
	if cfg.Path == "" {
		cfg.Path = "/metrics"
	}
	if cfg.Namespace == "" {
		cfg.Namespace = "celeris"
	}
	if len(cfg.Buckets) == 0 {
		cfg.Buckets = DefaultBuckets()
	}
	if len(cfg.SizeBuckets) == 0 {
		cfg.SizeBuckets = []float64{100, 1000, 10000, 100000, 1000000, 10000000}
	}
	return cfg
}

func (cfg Config) validate() {
	for i := 1; i < len(cfg.Buckets); i++ {
		if cfg.Buckets[i] <= cfg.Buckets[i-1] {
			panic("metrics: Buckets must be sorted in ascending order")
		}
	}
	for i := 1; i < len(cfg.SizeBuckets); i++ {
		if cfg.SizeBuckets[i] <= cfg.SizeBuckets[i-1] {
			panic("metrics: SizeBuckets must be sorted in ascending order")
		}
	}
	reservedLabels := map[string]bool{"method": true, "path": true, "status": true}
	for k := range cfg.LabelFuncs {
		if reservedLabels[k] {
			panic(fmt.Sprintf("metrics: LabelFuncs key %q conflicts with reserved base label", k))
		}
	}
}
