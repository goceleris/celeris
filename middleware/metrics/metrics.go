package metrics

import (
	"bytes"
	"encoding/binary"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/common/expfmt"

	"github.com/goceleris/celeris"
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

	allLabels := make([]string, 0, 3+len(customLabelNames))
	allLabels = append(allLabels, "method", "path", "status")
	allLabels = append(allLabels, customLabelNames...)

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
	nLabels := len(allLabels)
	set := &seriesSet{
		requestsTotal:   requestsTotal,
		requestDuration: requestDuration,
		m:               make(map[string]*series),
	}

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
		// Defer ensures the gauge is always decremented, even when a
		// panic escapes this middleware (e.g. recovery ordered outside
		// metrics).
		defer activeRequests.Dec()

		err := c.Next()

		duration := time.Since(c.StartTime()).Seconds()

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
		fullPath := c.FullPath()
		path := fullPath
		if path == "" {
			if status == 404 {
				path = "<unmatched>"
			} else {
				path = c.Path()
			}
		}
		path = strings.ToValidUTF8(path, "")

		// A route's method is one the app registered, so its label values
		// are bounded by the routes. A request no route matched (FullPath
		// "<unmatched>", "<method-not-allowed>", or "" without the core
		// router) carries whatever method the client sent, which the global
		// middleware sees since celeris#852: one new series per distinct
		// method, kept for the life of the registry, and WithLabelValues
		// panics on one that is not valid UTF-8. Outside the standard set it
		// is "_OTHER", as in the otel middleware.
		method := c.Method()
		if (fullPath == "" || fullPath[0] != '/') && !isStandardMethod(method) {
			method = "_OTHER"
		}

		// Label values: method, path, status + custom labels, as a lookup
		// key. The key is built on the stack and the lookup copies
		// nothing; see seriesSet.
		var kb [256]byte
		key := appendLabelValue(kb[:0], method)
		key = appendLabelValue(key, path)
		key = appendLabelValue(key, statusStr)
		for i := range nCustom {
			key = appendLabelValue(key, customLabelFuncs[i](c))
		}
		s := set.get(key)
		if s == nil {
			s = set.add(key, nLabels)
		}

		s.total.Inc()
		s.duration.Observe(duration)

		if cl := c.ContentLength(); cl > 0 {
			s.observer(&s.reqSize, requestSize).Observe(float64(cl))
		}
		if bw := c.BytesWritten(); bw > 0 {
			s.observer(&s.respSize, responseSize).Observe(float64(bw))
		}

		return err
	}
}

// seriesSet holds every label-value combination the middleware has
// recorded, keyed by the values, with copies of the values it owns and the
// series it resolved for them.
//
// The label values are request strings: c.Method() for a method the H1
// parser does not intern, c.Path() when there is no route pattern, and
// whatever a LabelFuncs function reads from the request. On epoll and
// io_uring those are views of the connection's receive buffer, which the
// engine reuses for the connection's next request and, once the connection
// closes, for another connection. client_golang keeps the label values of
// every new series for the life of the registry and does not copy them, so a
// series created from views would change its labels to other request bytes,
// including another client's headers (celeris#732). Only Prometheus ever
// sees the owned copies, and they are made only when a combination is new:
// a request whose combination was seen before copies nothing and resolves
// its series with one map lookup instead of one WithLabelValues call per
// metric.
type seriesSet struct {
	requestsTotal   *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec

	mu sync.RWMutex
	m  map[string]*series
}

// series is one label-value combination.
type series struct {
	values   []string // owned copies, cut from the seriesSet key
	total    prometheus.Counter
	duration prometheus.Observer
	// The size histograms are resolved on first use, so a combination that
	// never carried a body has no request_size_bytes series, as before.
	reqSize  atomic.Pointer[prometheus.Observer]
	respSize atomic.Pointer[prometheus.Observer]
}

// isStandardMethod reports whether m is one of the HTTP methods of RFC 9110
// §9 or PATCH, the set the otel middleware keeps.
func isStandardMethod(m string) bool {
	switch m {
	case "GET", "HEAD", "POST", "PUT", "DELETE", "PATCH", "OPTIONS", "TRACE", "CONNECT":
		return true
	}
	return false
}

// appendLabelValue appends v to a lookup key, length-prefixed so that no
// two combinations share a key whatever bytes the values hold.
func appendLabelValue(key []byte, v string) []byte {
	key = binary.AppendUvarint(key, uint64(len(v)))
	return append(key, v...)
}

func (set *seriesSet) get(key []byte) *series {
	set.mu.RLock()
	s := set.m[string(key)]
	set.mu.RUnlock()
	return s
}

// add records a new combination of n label values. The map key is a copy of
// the lookup key, and the label values handed to Prometheus are cut from it.
func (set *seriesSet) add(key []byte, n int) *series {
	owned := string(key)
	values := make([]string, 0, n)
	for off := 0; off < len(key); {
		l, w := binary.Uvarint(key[off:])
		off += w
		values = append(values, owned[off:off+int(l)])
		off += int(l)
	}
	// Resolved outside the lock: WithLabelValues panics on a label value
	// that is not valid UTF-8, and the panic must not leave mu held.
	s := &series{
		values:   values,
		total:    set.requestsTotal.WithLabelValues(values...),
		duration: set.requestDuration.WithLabelValues(values...),
	}
	set.mu.Lock()
	defer set.mu.Unlock()
	if prev := set.m[owned]; prev != nil {
		return prev
	}
	set.m[owned] = s
	return s
}

// observer returns the series' observer in vec, resolving it on first use.
func (s *series) observer(p *atomic.Pointer[prometheus.Observer], vec *prometheus.HistogramVec) prometheus.Observer {
	if o := p.Load(); o != nil {
		return *o
	}
	o := vec.WithLabelValues(s.values...)
	p.Store(&o)
	return o
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
