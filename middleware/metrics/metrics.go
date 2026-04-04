package metrics

import (
	"bytes"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goceleris/celeris"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/common/expfmt"
)

// New creates a Prometheus metrics middleware with the given config.
func New(config ...Config) celeris.HandlerFunc {
	cfg := defaultConfig
	if len(config) > 0 {
		cfg = config[0]
	}
	cfg = applyDefaults(cfg)
	cfg.validate()

	reg := cfg.Registry
	if reg == nil {
		reg = prometheus.NewRegistry()
		reg.MustRegister(collectors.NewGoCollector())
		reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	}

	constLabels := prometheus.Labels(cfg.ConstLabels)

	// Build sorted custom label names for deterministic label order.
	customLabelNames := make([]string, 0, len(cfg.LabelFuncs))
	for name := range cfg.LabelFuncs {
		customLabelNames = append(customLabelNames, name)
	}
	sort.Strings(customLabelNames)

	// Build ordered label funcs matching the sorted names.
	customLabelFuncs := make([]func(*celeris.Context) string, len(customLabelNames))
	for i, name := range customLabelNames {
		customLabelFuncs[i] = cfg.LabelFuncs[name]
	}

	baseLabels := []string{"method", "path", "status"}
	allLabels := append(baseLabels, customLabelNames...)

	requestsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   cfg.Namespace,
		Subsystem:   cfg.Subsystem,
		Name:        "requests_total",
		Help:        "Total number of HTTP requests.",
		ConstLabels: constLabels,
	}, allLabels)

	requestDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   cfg.Namespace,
		Subsystem:   cfg.Subsystem,
		Name:        "request_duration_seconds",
		Help:        "HTTP request duration in seconds.",
		Buckets:     cfg.Buckets,
		ConstLabels: constLabels,
	}, allLabels)

	requestSize := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   cfg.Namespace,
		Subsystem:   cfg.Subsystem,
		Name:        "request_size_bytes",
		Help:        "HTTP request body size in bytes.",
		Buckets:     cfg.SizeBuckets,
		ConstLabels: constLabels,
	}, allLabels)

	responseSize := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   cfg.Namespace,
		Subsystem:   cfg.Subsystem,
		Name:        "response_size_bytes",
		Help:        "HTTP response body size in bytes.",
		Buckets:     cfg.SizeBuckets,
		ConstLabels: constLabels,
	}, allLabels)

	activeRequests := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   cfg.Namespace,
		Subsystem:   cfg.Subsystem,
		Name:        "active_requests",
		Help:        "Number of currently active HTTP requests.",
		ConstLabels: constLabels,
	})

	reg.MustRegister(requestsTotal, requestDuration, requestSize, responseSize, activeRequests)

	skipMap := make(map[string]struct{}, len(cfg.SkipPaths))
	for _, p := range cfg.SkipPaths {
		skipMap[p] = struct{}{}
	}

	ignoreStatus := make(map[int]struct{}, len(cfg.IgnoreStatusCodes))
	for _, code := range cfg.IgnoreStatusCodes {
		ignoreStatus[code] = struct{}{}
	}

	// Pre-cache status code strings for common codes.
	statusStrings := make(map[int]string, 17)
	for _, code := range []int{
		200, 201, 204,
		301, 302, 304,
		400, 401, 403, 404, 405, 408, 429,
		500, 502, 503, 504,
	} {
		statusStrings[code] = strconv.Itoa(code)
	}

	metricsPath := cfg.Path
	authFunc := cfg.AuthFunc
	nCustom := len(customLabelNames)

	return func(c *celeris.Context) error {
		if c.Path() == metricsPath {
			method := c.Method()
			if method != "GET" && method != "HEAD" {
				return c.Next()
			}
			if authFunc != nil && !authFunc(c) {
				c.Abort()
				return c.NoContent(403)
			}
			c.Abort()
			return serveMetrics(c, reg)
		}

		if cfg.Skip != nil && cfg.Skip(c) {
			return c.Next()
		}
		if _, ok := skipMap[c.Path()]; ok {
			return c.Next()
		}

		activeRequests.Inc()

		err := c.Next()

		duration := time.Since(c.StartTime()).Seconds()
		activeRequests.Dec()

		status := c.StatusCode()
		if _, ignored := ignoreStatus[status]; ignored {
			return err
		}

		statusStr, ok := statusStrings[status]
		if !ok {
			statusStr = strconv.Itoa(status)
		}

		// With celeris v1.2.4+ core, FullPath() returns "<unmatched>" for 404
		// and "<method-not-allowed>" for 405 when the request passes through
		// the core router, so the 404 fallback below is redundant in that
		// case. The fallback is kept for edge cases: middleware running
		// without the core router (e.g., ToHandler bridge) or pre-v1.2.4.
		path := c.FullPath()
		if path == "" {
			if status == 404 {
				path = "<unmatched>"
			} else {
				path = c.Path()
			}
		}
		path = strings.ToValidUTF8(path, "")

		// Build label values: method, path, status + custom labels.
		lv := make([]string, 3, 3+nCustom)
		lv[0] = c.Method()
		lv[1] = path
		lv[2] = statusStr
		for i := range nCustom {
			lv = append(lv, customLabelFuncs[i](c))
		}

		requestsTotal.WithLabelValues(lv...).Inc()
		requestDuration.WithLabelValues(lv...).Observe(duration)

		if cl := c.ContentLength(); cl > 0 {
			requestSize.WithLabelValues(lv...).Observe(float64(cl))
		}
		if bw := c.BytesWritten(); bw > 0 {
			responseSize.WithLabelValues(lv...).Observe(float64(bw))
		}

		return err
	}
}

func serveMetrics(c *celeris.Context, gatherer prometheus.Gatherer) error {
	mfs, err := gatherer.Gather()
	if err != nil {
		return c.String(500, "internal error")
	}
	var buf bytes.Buffer
	enc := expfmt.NewEncoder(&buf, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, mf := range mfs {
		if err := enc.Encode(mf); err != nil {
			return c.String(500, "internal error")
		}
	}
	return c.Blob(200, "text/plain; version=0.0.4; charset=utf-8", buf.Bytes())
}
