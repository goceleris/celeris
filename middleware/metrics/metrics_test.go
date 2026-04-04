package metrics

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func newIsolatedRegistry() *prometheus.Registry {
	return prometheus.NewRegistry()
}

func runChain(t *testing.T, chain []celeris.HandlerFunc, method, path string, opts ...celeristest.Option) (*celeristest.ResponseRecorder, error) {
	t.Helper()
	allOpts := make([]celeristest.Option, 0, len(opts)+1)
	allOpts = append(allOpts, celeristest.WithHandlers(chain...))
	allOpts = append(allOpts, opts...)
	ctx, rec := celeristest.NewContextT(t, method, path, allOpts...)
	err := ctx.Next()
	return rec, err
}

func getMetricValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			for _, m := range mf.GetMetric() {
				if m.GetCounter() != nil {
					return m.GetCounter().GetValue()
				}
				if m.GetGauge() != nil {
					return m.GetGauge().GetValue()
				}
			}
		}
	}
	return 0
}

func getCounterValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if matchLabels(m.GetLabel(), labels) {
				if m.GetCounter() != nil {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

func getHistogramCount(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) uint64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if matchLabels(m.GetLabel(), labels) {
				if m.GetHistogram() != nil {
					return m.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	return 0
}

func matchLabels(pairs []*dto.LabelPair, expected map[string]string) bool {
	if len(expected) == 0 {
		return true
	}
	matched := 0
	for _, lp := range pairs {
		if v, ok := expected[lp.GetName()]; ok && v == lp.GetValue() {
			matched++
		}
	}
	return matched == len(expected)
}

func TestMetricsEndpointServesPrometheus(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{Registry: reg})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	// Make a request to generate metrics.
	rec, err := runChain(t, chain, "GET", "/api/users")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.StatusCode != 200 {
		t.Fatalf("status: got %d, want 200", rec.StatusCode)
	}

	// Fetch the metrics endpoint.
	rec, err = runChain(t, chain, "GET", "/metrics")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.StatusCode != 200 {
		t.Fatalf("status: got %d, want 200", rec.StatusCode)
	}
	ct := rec.Header("content-type")
	if !strings.Contains(ct, "text/plain") {
		t.Fatalf("content-type: got %q, want text/plain", ct)
	}
	body := rec.BodyString()
	if !strings.Contains(body, "celeris_requests_total") {
		t.Fatalf("body missing celeris_requests_total:\n%s", body)
	}
	if !strings.Contains(body, "celeris_request_duration_seconds") {
		t.Fatalf("body missing celeris_request_duration_seconds:\n%s", body)
	}
	if !strings.Contains(body, "celeris_active_requests") {
		t.Fatalf("body missing celeris_active_requests:\n%s", body)
	}
}

func TestRequestCounterIncrements(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{Registry: reg})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	for range 3 {
		_, err := runChain(t, chain, "GET", "/api/items")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	count := getCounterValue(t, reg, "celeris_requests_total", map[string]string{
		"method": "GET",
		"path":   "/api/items",
		"status": "200",
	})
	if count != 3 {
		t.Fatalf("requests_total: got %v, want 3", count)
	}
}

func TestDurationHistogramRecords(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{Registry: reg})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	_, err := runChain(t, chain, "POST", "/api/data")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	count := getHistogramCount(t, reg, "celeris_request_duration_seconds", map[string]string{
		"method": "POST",
		"path":   "/api/data",
		"status": "200",
	})
	if count != 1 {
		t.Fatalf("duration histogram count: got %d, want 1", count)
	}
}

func TestActiveRequestsGauge(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{Registry: reg})

	var gaugeInFlight float64
	handler := func(_ *celeris.Context) error {
		gaugeInFlight = getMetricValue(t, reg, "celeris_active_requests")
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}

	_, err := runChain(t, chain, "GET", "/api/check")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gaugeInFlight != 1 {
		t.Fatalf("active_requests during request: got %v, want 1", gaugeInFlight)
	}

	after := getMetricValue(t, reg, "celeris_active_requests")
	if after != 0 {
		t.Fatalf("active_requests after request: got %v, want 0", after)
	}
}

func TestCustomNamespaceSubsystem(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{
		Registry:  reg,
		Namespace: "myapp",
		Subsystem: "http",
	})
	handler := func(c *celeris.Context) error {
		return c.String(201, "created")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	_, err := runChain(t, chain, "POST", "/items")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	count := getCounterValue(t, reg, "myapp_http_requests_total", map[string]string{
		"method": "POST",
		"path":   "/items",
		"status": "201",
	})
	if count != 1 {
		t.Fatalf("myapp_http_requests_total: got %v, want 1", count)
	}

	histCount := getHistogramCount(t, reg, "myapp_http_request_duration_seconds", map[string]string{
		"method": "POST",
		"path":   "/items",
		"status": "201",
	})
	if histCount != 1 {
		t.Fatalf("myapp_http_request_duration_seconds count: got %d, want 1", histCount)
	}
}

func TestSkipPathsExcludesFromMetrics(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{
		Registry:  reg,
		SkipPaths: []string{"/health", "/ready"},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	_, err := runChain(t, chain, "GET", "/health")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, err = runChain(t, chain, "GET", "/ready")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, err = runChain(t, chain, "GET", "/api/data")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	healthCount := getCounterValue(t, reg, "celeris_requests_total", map[string]string{
		"path": "/health",
	})
	if healthCount != 0 {
		t.Fatalf("health path should be skipped: got %v", healthCount)
	}
	readyCount := getCounterValue(t, reg, "celeris_requests_total", map[string]string{
		"path": "/ready",
	})
	if readyCount != 0 {
		t.Fatalf("ready path should be skipped: got %v", readyCount)
	}

	apiCount := getCounterValue(t, reg, "celeris_requests_total", map[string]string{
		"path": "/api/data",
	})
	if apiCount != 1 {
		t.Fatalf("api path should be recorded: got %v, want 1", apiCount)
	}
}

func TestSkipFunc(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{
		Registry: reg,
		Skip: func(c *celeris.Context) bool {
			return c.Path() == "/internal"
		},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	_, err := runChain(t, chain, "GET", "/internal")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	count := getCounterValue(t, reg, "celeris_requests_total", map[string]string{
		"path": "/internal",
	})
	if count != 0 {
		t.Fatalf("skipped path should not be recorded: got %v", count)
	}
}

func TestCustomRegistryIsolation(t *testing.T) {
	reg1 := newIsolatedRegistry()
	reg2 := newIsolatedRegistry()
	mw1 := New(Config{Registry: reg1, Namespace: "svc1"})
	mw2 := New(Config{Registry: reg2, Namespace: "svc2"})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}

	chain1 := []celeris.HandlerFunc{mw1, handler}
	chain2 := []celeris.HandlerFunc{mw2, handler}

	_, _ = runChain(t, chain1, "GET", "/a")
	_, _ = runChain(t, chain2, "GET", "/b")

	svc1Count := getCounterValue(t, reg1, "svc1_requests_total", map[string]string{
		"path": "/a",
	})
	if svc1Count != 1 {
		t.Fatalf("svc1 count: got %v, want 1", svc1Count)
	}

	// reg1 should not contain svc2 metrics.
	svc2InReg1 := getCounterValue(t, reg1, "svc2_requests_total", nil)
	if svc2InReg1 != 0 {
		t.Fatalf("svc2 metric in reg1: got %v, want 0", svc2InReg1)
	}

	svc2Count := getCounterValue(t, reg2, "svc2_requests_total", map[string]string{
		"path": "/b",
	})
	if svc2Count != 1 {
		t.Fatalf("svc2 count: got %v, want 1", svc2Count)
	}
}

func TestMetricsEndpointNotRecordedItself(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{Registry: reg})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	_, err := runChain(t, chain, "GET", "/metrics")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	count := getCounterValue(t, reg, "celeris_requests_total", map[string]string{
		"path": "/metrics",
	})
	if count != 0 {
		t.Fatalf("metrics path should not be recorded: got %v", count)
	}
}

func TestCustomMetricsPath(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{Registry: reg, Path: "/prom"})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	rec, err := runChain(t, chain, "GET", "/prom")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.StatusCode != 200 {
		t.Fatalf("status: got %d, want 200", rec.StatusCode)
	}
	ct := rec.Header("content-type")
	if !strings.Contains(ct, "text/plain") {
		t.Fatalf("content-type: got %q, want text/plain", ct)
	}
}

func TestMultipleStatusCodes(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{Registry: reg})

	codes := []int{200, 404, 500}
	for _, code := range codes {
		status := code
		handler := func(c *celeris.Context) error {
			return c.String(status, "response")
		}
		chain := []celeris.HandlerFunc{mw, handler}
		_, err := runChain(t, chain, "GET", "/test")
		if err != nil {
			t.Fatalf("unexpected error for status %d: %v", code, err)
		}
	}

	for _, code := range codes {
		path := "/test"
		if code == 404 {
			path = "<unmatched>"
		}
		label := map[string]string{
			"path":   path,
			"status": strconv.Itoa(code),
		}
		count := getCounterValue(t, reg, "celeris_requests_total", label)
		if count != 1 {
			t.Fatalf("requests_total for status %d: got %v, want 1", code, count)
		}
	}
}

func TestDefaultConfigValues(t *testing.T) {
	cfg := applyDefaults(defaultConfig)
	if cfg.Path != "/metrics" {
		t.Fatalf("default path: got %q, want /metrics", cfg.Path)
	}
	if cfg.Namespace != "celeris" {
		t.Fatalf("default namespace: got %q, want celeris", cfg.Namespace)
	}
	if len(cfg.Buckets) != len(DefaultBuckets()) {
		t.Fatalf("default buckets: got %d entries, want %d", len(cfg.Buckets), len(DefaultBuckets()))
	}
}

func TestCustomBuckets(t *testing.T) {
	reg := newIsolatedRegistry()
	custom := []float64{0.01, 0.05, 0.1, 0.5, 1}
	mw := New(Config{Registry: reg, Buckets: custom})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	_, err := runChain(t, chain, "GET", "/api/buckets")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mfs, gatherErr := reg.Gather()
	if gatherErr != nil {
		t.Fatalf("gather: %v", gatherErr)
	}
	for _, mf := range mfs {
		if mf.GetName() != "celeris_request_duration_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			h := m.GetHistogram()
			if h == nil {
				continue
			}
			buckets := h.GetBucket()
			if len(buckets) != len(custom) {
				t.Fatalf("expected %d buckets, got %d", len(custom), len(buckets))
			}
			for i, b := range buckets {
				if b.GetUpperBound() != custom[i] {
					t.Fatalf("bucket %d: got %v, want %v", i, b.GetUpperBound(), custom[i])
				}
			}
		}
	}
}

func TestConstLabels(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{
		Registry:    reg,
		ConstLabels: map[string]string{"service": "myapp", "env": "test"},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	_, err := runChain(t, chain, "GET", "/api/const")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mfs, gatherErr := reg.Gather()
	if gatherErr != nil {
		t.Fatalf("gather: %v", gatherErr)
	}

	for _, mf := range mfs {
		if mf.GetName() != "celeris_requests_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			found := map[string]bool{"service": false, "env": false}
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "service" && lp.GetValue() == "myapp" {
					found["service"] = true
				}
				if lp.GetName() == "env" && lp.GetValue() == "test" {
					found["env"] = true
				}
			}
			if !found["service"] || !found["env"] {
				t.Fatalf("expected const labels service=myapp, env=test; got labels: %v", m.GetLabel())
			}
		}
	}
}

func TestUnsortedBucketsPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for unsorted buckets")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "sorted in ascending order") {
			t.Fatalf("unexpected panic message: %v", r)
		}
	}()
	New(Config{
		Registry: newIsolatedRegistry(),
		Buckets:  []float64{0.5, 0.1, 1.0},
	})
}

func TestDuplicateBucketsPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for duplicate buckets")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "sorted in ascending order") {
			t.Fatalf("unexpected panic message: %v", r)
		}
	}()
	New(Config{
		Registry: newIsolatedRegistry(),
		Buckets:  []float64{0.1, 0.5, 0.5, 1.0},
	})
}

func TestLabelFuncsReservedKeyPanics(t *testing.T) {
	for _, key := range []string{"method", "path", "status"} {
		t.Run(key, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("expected panic for reserved LabelFuncs key %q", key)
				}
				msg, ok := r.(string)
				if !ok || !strings.Contains(msg, "conflicts with reserved base label") {
					t.Fatalf("unexpected panic message: %v", r)
				}
			}()
			New(Config{
				Registry: newIsolatedRegistry(),
				LabelFuncs: map[string]func(*celeris.Context) string{
					key: func(_ *celeris.Context) string { return "x" },
				},
			})
		})
	}
}

func TestSortedBucketsDoNotPanic(_ *testing.T) {
	// Should not panic with properly sorted buckets.
	New(Config{
		Registry: newIsolatedRegistry(),
		Buckets:  []float64{0.001, 0.01, 0.1, 1.0, 10.0},
	})
}

func TestDurationHistogramIncludesStatus(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{Registry: reg})
	handler := func(c *celeris.Context) error {
		return c.String(201, "created")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	_, err := runChain(t, chain, "POST", "/api/create")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	count := getHistogramCount(t, reg, "celeris_request_duration_seconds", map[string]string{
		"method": "POST",
		"path":   "/api/create",
		"status": "201",
	})
	if count != 1 {
		t.Fatalf("expected histogram with status=201, got count %d", count)
	}
}

func TestUnmatched404UseSentinel(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{Registry: reg})
	handler := func(c *celeris.Context) error {
		return c.String(404, "not found")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	_, err := runChain(t, chain, "GET", "/random/scanner/path")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	count := getCounterValue(t, reg, "celeris_requests_total", map[string]string{
		"method": "GET",
		"path":   "<unmatched>",
		"status": "404",
	})
	if count != 1 {
		t.Fatalf("expected 404 recorded with <unmatched> path, got count %v", count)
	}

	rawCount := getCounterValue(t, reg, "celeris_requests_total", map[string]string{
		"method": "GET",
		"path":   "/random/scanner/path",
		"status": "404",
	})
	if rawCount != 0 {
		t.Fatalf("raw path should NOT be recorded for 404, got count %v", rawCount)
	}
}

func TestAuthFuncBlocksMetricsEndpoint(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{
		Registry: reg,
		AuthFunc: func(_ *celeris.Context) bool { return false },
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	rec, err := runChain(t, chain, "GET", "/metrics")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.StatusCode != 403 {
		t.Fatalf("expected 403, got %d", rec.StatusCode)
	}
}

func TestAuthFuncAllowsMetricsEndpoint(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{
		Registry: reg,
		AuthFunc: func(_ *celeris.Context) bool { return true },
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	rec, err := runChain(t, chain, "GET", "/metrics")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", rec.StatusCode)
	}
	body := rec.BodyString()
	if !strings.Contains(body, "celeris_") {
		t.Fatalf("expected metrics content, got: %s", body)
	}
}

func TestAuthFuncNilAllowsByDefault(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{Registry: reg})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	rec, err := runChain(t, chain, "GET", "/metrics")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.StatusCode != 200 {
		t.Fatalf("expected 200 when AuthFunc is nil, got %d", rec.StatusCode)
	}
}

func TestAuthFuncChecksHeader(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{
		Registry: reg,
		AuthFunc: func(c *celeris.Context) bool {
			return c.Header("authorization") == "Bearer secret"
		},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	// Without auth header: 403.
	rec, err := runChain(t, chain, "GET", "/metrics")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.StatusCode != 403 {
		t.Fatalf("expected 403 without auth, got %d", rec.StatusCode)
	}

	// With auth header: 200.
	rec, err = runChain(t, chain, "GET", "/metrics",
		celeristest.WithHeader("authorization", "Bearer secret"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.StatusCode != 200 {
		t.Fatalf("expected 200 with auth, got %d", rec.StatusCode)
	}
}

func TestAuthFuncDoesNotAffectNonMetricsPaths(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{
		Registry: reg,
		AuthFunc: func(_ *celeris.Context) bool { return false },
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	// Non-metrics path should not be affected by AuthFunc.
	rec, err := runChain(t, chain, "GET", "/api/data")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.StatusCode != 200 {
		t.Fatalf("expected 200 for non-metrics path, got %d", rec.StatusCode)
	}
}

func TestNon404EmptyFullPathUsesRawPath(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{Registry: reg})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	_, err := runChain(t, chain, "GET", "/some/path")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	count := getCounterValue(t, reg, "celeris_requests_total", map[string]string{
		"method": "GET",
		"path":   "/some/path",
		"status": "200",
	})
	if count != 1 {
		t.Fatalf("expected raw path for non-404, got count %v", count)
	}
}

func TestMetricsEndpointPOSTPassesThrough(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{Registry: reg})
	handler := func(c *celeris.Context) error {
		return c.String(200, "handler reached")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	rec, err := runChain(t, chain, "POST", "/metrics")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.StatusCode != 200 {
		t.Fatalf("expected 200 for POST /metrics passthrough, got %d", rec.StatusCode)
	}
	body := rec.BodyString()
	if !strings.Contains(body, "handler reached") {
		t.Fatalf("expected handler body for POST /metrics, got: %s", body)
	}
}

func TestMetricsEndpointDELETEPassesThrough(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{Registry: reg})
	handler := func(c *celeris.Context) error {
		return c.String(200, "delete handler")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	rec, err := runChain(t, chain, "DELETE", "/metrics")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.StatusCode != 200 {
		t.Fatalf("expected 200 for DELETE /metrics passthrough, got %d", rec.StatusCode)
	}
}

func TestMetricsEndpointHEADAllowed(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{Registry: reg})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	rec, err := runChain(t, chain, "HEAD", "/metrics")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// HEAD to /metrics should serve metrics (200), not pass through.
	if rec.StatusCode != 200 {
		t.Fatalf("expected 200 for HEAD /metrics, got %d", rec.StatusCode)
	}
}

func TestHandlerErrorPropagated(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{Registry: reg})
	handler := func(_ *celeris.Context) error {
		return celeris.NewHTTPError(503, "service unavailable")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	_, err := runChain(t, chain, "GET", "/fail")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	count := getCounterValue(t, reg, "celeris_requests_total", map[string]string{
		"method": "GET",
		"path":   "/fail",
	})
	if count != 1 {
		t.Fatalf("request should still be recorded on error: got %v, want 1", count)
	}
}

func getHistogramSum(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if matchLabels(m.GetLabel(), labels) {
				if m.GetHistogram() != nil {
					return m.GetHistogram().GetSampleSum()
				}
			}
		}
	}
	return 0
}

func TestRequestSizeRecorded(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{Registry: reg})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	body := []byte(`{"key":"value"}`)
	_, err := runChain(t, chain, "POST", "/api/upload",
		celeristest.WithBody(body),
		celeristest.WithHeader("content-length", fmt.Sprintf("%d", len(body))),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	labels := map[string]string{"method": "POST", "path": "/api/upload", "status": "200"}
	count := getHistogramCount(t, reg, "celeris_request_size_bytes", labels)
	if count != 1 {
		t.Fatalf("request_size_bytes count: got %d, want 1", count)
	}
	sum := getHistogramSum(t, reg, "celeris_request_size_bytes", labels)
	if sum != float64(len(body)) {
		t.Fatalf("request_size_bytes sum: got %v, want %v", sum, float64(len(body)))
	}
}

func TestResponseSizeRecorded(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{Registry: reg})
	handler := func(c *celeris.Context) error {
		return c.String(200, "hello world response")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	_, err := runChain(t, chain, "GET", "/api/data")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	labels := map[string]string{"method": "GET", "path": "/api/data", "status": "200"}
	count := getHistogramCount(t, reg, "celeris_response_size_bytes", labels)
	if count != 1 {
		t.Fatalf("response_size_bytes count: got %d, want 1", count)
	}
	sum := getHistogramSum(t, reg, "celeris_response_size_bytes", labels)
	wantSize := float64(len("hello world response"))
	if sum != wantSize {
		t.Fatalf("response_size_bytes sum: got %v, want %v", sum, wantSize)
	}
}

func TestZeroBodyNotRecorded(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{Registry: reg})
	handler := func(c *celeris.Context) error {
		return c.NoContent(204)
	}
	chain := []celeris.HandlerFunc{mw, handler}

	// No body, no content-length header.
	_, err := runChain(t, chain, "GET", "/api/empty")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	labels := map[string]string{"method": "GET", "path": "/api/empty", "status": "204"}
	reqCount := getHistogramCount(t, reg, "celeris_request_size_bytes", labels)
	if reqCount != 0 {
		t.Fatalf("request_size_bytes should not record zero body: got count %d", reqCount)
	}
	respCount := getHistogramCount(t, reg, "celeris_response_size_bytes", labels)
	if respCount != 0 {
		t.Fatalf("response_size_bytes should not record zero body: got count %d", respCount)
	}
}

func TestLabelFuncsCustomLabels(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{
		Registry: reg,
		LabelFuncs: map[string]func(*celeris.Context) string{
			"region": func(c *celeris.Context) string {
				v := c.Header("x-region")
				if v == "" {
					return "unknown"
				}
				return v
			},
		},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	_, err := runChain(t, chain, "GET", "/api/region",
		celeristest.WithHeader("x-region", "us-east-1"),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	labels := map[string]string{
		"method": "GET",
		"path":   "/api/region",
		"status": "200",
		"region": "us-east-1",
	}
	count := getCounterValue(t, reg, "celeris_requests_total", labels)
	if count != 1 {
		t.Fatalf("requests_total with custom label: got %v, want 1", count)
	}
	histCount := getHistogramCount(t, reg, "celeris_request_duration_seconds", labels)
	if histCount != 1 {
		t.Fatalf("duration histogram with custom label: got %d, want 1", histCount)
	}
}

func TestLabelFuncsMultipleLabels(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{
		Registry: reg,
		LabelFuncs: map[string]func(*celeris.Context) string{
			"az": func(c *celeris.Context) string {
				v := c.Header("x-az")
				if v == "" {
					return "unknown"
				}
				return v
			},
			"region": func(c *celeris.Context) string {
				v := c.Header("x-region")
				if v == "" {
					return "unknown"
				}
				return v
			},
		},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	_, err := runChain(t, chain, "GET", "/api/multi",
		celeristest.WithHeader("x-region", "eu-west-1"),
		celeristest.WithHeader("x-az", "a"),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	labels := map[string]string{
		"method": "GET",
		"path":   "/api/multi",
		"status": "200",
		"region": "eu-west-1",
		"az":     "a",
	}
	count := getCounterValue(t, reg, "celeris_requests_total", labels)
	if count != 1 {
		t.Fatalf("requests_total with multiple custom labels: got %v, want 1", count)
	}
}

func TestCustomSizeBuckets(t *testing.T) {
	reg := newIsolatedRegistry()
	customBuckets := []float64{256, 1024, 4096}
	mw := New(Config{
		Registry:    reg,
		SizeBuckets: customBuckets,
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	body := []byte("x")
	_, err := runChain(t, chain, "POST", "/api/opts",
		celeristest.WithBody(body),
		celeristest.WithHeader("content-length", "1"),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mfs, gatherErr := reg.Gather()
	if gatherErr != nil {
		t.Fatalf("gather: %v", gatherErr)
	}
	for _, mf := range mfs {
		if mf.GetName() != "celeris_request_size_bytes" {
			continue
		}
		for _, m := range mf.GetMetric() {
			h := m.GetHistogram()
			if h == nil {
				continue
			}
			buckets := h.GetBucket()
			if len(buckets) != len(customBuckets) {
				t.Fatalf("expected %d buckets, got %d", len(customBuckets), len(buckets))
			}
			for i, b := range buckets {
				if b.GetUpperBound() != customBuckets[i] {
					t.Fatalf("bucket %d: got %v, want %v", i, b.GetUpperBound(), customBuckets[i])
				}
			}
			return
		}
	}
	t.Fatal("request_size_bytes metric not found")
}

func TestIgnoreStatusCodesExcludesFromAllMetrics(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{
		Registry:          reg,
		IgnoreStatusCodes: []int{404},
	})
	handler := func(c *celeris.Context) error {
		return c.String(404, "not found")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	for range 3 {
		_, err := runChain(t, chain, "GET", "/scanner/probe")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	count := getCounterValue(t, reg, "celeris_requests_total", map[string]string{
		"status": "404",
	})
	if count != 0 {
		t.Fatalf("requests_total should not record ignored status 404: got %v", count)
	}

	histCount := getHistogramCount(t, reg, "celeris_request_duration_seconds", map[string]string{
		"status": "404",
	})
	if histCount != 0 {
		t.Fatalf("duration histogram should not record ignored status 404: got %d", histCount)
	}
}

func TestIgnoreStatusCodesDoesNotAffectOtherCodes(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{
		Registry:          reg,
		IgnoreStatusCodes: []int{404},
	})
	chain200 := []celeris.HandlerFunc{mw, func(c *celeris.Context) error {
		return c.String(200, "ok")
	}}
	chain500 := []celeris.HandlerFunc{mw, func(c *celeris.Context) error {
		return c.String(500, "error")
	}}

	_, _ = runChain(t, chain200, "GET", "/api/data")
	_, _ = runChain(t, chain500, "GET", "/api/fail")

	ok200 := getCounterValue(t, reg, "celeris_requests_total", map[string]string{
		"status": "200",
	})
	if ok200 != 1 {
		t.Fatalf("200 should be recorded: got %v, want 1", ok200)
	}

	ok500 := getCounterValue(t, reg, "celeris_requests_total", map[string]string{
		"status": "500",
	})
	if ok500 != 1 {
		t.Fatalf("500 should be recorded: got %v, want 1", ok500)
	}
}

func TestIgnoreStatusCodesMultiple(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{
		Registry:          reg,
		IgnoreStatusCodes: []int{404, 405},
	})
	chain404 := []celeris.HandlerFunc{mw, func(c *celeris.Context) error {
		return c.String(404, "not found")
	}}
	chain405 := []celeris.HandlerFunc{mw, func(c *celeris.Context) error {
		return c.String(405, "method not allowed")
	}}

	_, _ = runChain(t, chain404, "GET", "/x")
	_, _ = runChain(t, chain405, "POST", "/y")

	for _, code := range []int{404, 405} {
		count := getCounterValue(t, reg, "celeris_requests_total", map[string]string{
			"status": strconv.Itoa(code),
		})
		if count != 0 {
			t.Fatalf("status %d should be ignored: got %v", code, count)
		}
	}
}

func TestIgnoreStatusCodesEmpty(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{Registry: reg})
	handler := func(c *celeris.Context) error {
		return c.String(404, "not found")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	_, err := runChain(t, chain, "GET", "/probe")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	count := getCounterValue(t, reg, "celeris_requests_total", map[string]string{
		"status": "404",
	})
	if count != 1 {
		t.Fatalf("404 should be recorded when IgnoreStatusCodes is empty: got %v, want 1", count)
	}
}

func TestIgnoreStatusCodesSizeHistograms(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{
		Registry:          reg,
		IgnoreStatusCodes: []int{404},
	})
	handler := func(c *celeris.Context) error {
		return c.String(404, "not found body")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	_, err := runChain(t, chain, "POST", "/probe",
		celeristest.WithBody([]byte("req body")),
		celeristest.WithHeader("content-length", "8"),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	reqCount := getHistogramCount(t, reg, "celeris_request_size_bytes", map[string]string{
		"status": "404",
	})
	if reqCount != 0 {
		t.Fatalf("request_size_bytes should not record ignored 404: got %d", reqCount)
	}
	respCount := getHistogramCount(t, reg, "celeris_response_size_bytes", map[string]string{
		"status": "404",
	})
	if respCount != 0 {
		t.Fatalf("response_size_bytes should not record ignored 404: got %d", respCount)
	}
}

func TestUTF8PathSanitization(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{Registry: reg})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	// Path with invalid UTF-8 byte sequence.
	invalidPath := "/api/\xff\xfe/data"
	_, err := runChain(t, chain, "GET", invalidPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	count := getCounterValue(t, reg, "celeris_requests_total", map[string]string{
		"method": "GET",
		"path":   "/api//data",
		"status": "200",
	})
	if count != 1 {
		t.Fatalf("expected sanitized path recorded, got count %v", count)
	}
}

func TestValidUTF8PathUnchanged(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{Registry: reg})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	_, err := runChain(t, chain, "GET", "/api/users")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	count := getCounterValue(t, reg, "celeris_requests_total", map[string]string{
		"method": "GET",
		"path":   "/api/users",
		"status": "200",
	})
	if count != 1 {
		t.Fatalf("valid UTF-8 path should be unchanged: got count %v, want 1", count)
	}
}

// --- v1.2.4 celeristest option tests ---

func TestFullPathSentinel404(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{Registry: reg})
	handler := func(c *celeris.Context) error {
		return c.String(404, "not found")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	// Simulate a 404 where the core router sets FullPath to "<unmatched>".
	_, err := runChain(t, chain, "GET", "/nonexistent/scanner/path",
		celeristest.WithFullPath("<unmatched>"),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The metrics path label should use the sentinel, not the raw request path.
	count := getCounterValue(t, reg, "celeris_requests_total", map[string]string{
		"method": "GET",
		"path":   "<unmatched>",
		"status": "404",
	})
	if count != 1 {
		t.Fatalf("expected 404 recorded with <unmatched> sentinel path, got count %v", count)
	}

	rawCount := getCounterValue(t, reg, "celeris_requests_total", map[string]string{
		"method": "GET",
		"path":   "/nonexistent/scanner/path",
		"status": "404",
	})
	if rawCount != 0 {
		t.Fatalf("raw request path should NOT appear as label when sentinel is set, got count %v", rawCount)
	}
}

func TestSizeMetricsVisibleOnEndpoint(t *testing.T) {
	reg := newIsolatedRegistry()
	mw := New(Config{Registry: reg})
	handler := func(c *celeris.Context) error {
		return c.String(200, "response body here")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	// Generate a request with body to produce size metrics.
	body := []byte("request body")
	_, err := runChain(t, chain, "POST", "/api/data",
		celeristest.WithBody(body),
		celeristest.WithHeader("content-length", fmt.Sprintf("%d", len(body))),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Fetch the metrics endpoint.
	rec, err := runChain(t, chain, "GET", "/metrics")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	output := rec.BodyString()
	if !strings.Contains(output, "celeris_request_size_bytes") {
		t.Fatalf("metrics output missing celeris_request_size_bytes:\n%s", output)
	}
	if !strings.Contains(output, "celeris_response_size_bytes") {
		t.Fatalf("metrics output missing celeris_response_size_bytes:\n%s", output)
	}
}
