package otel

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.32.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

func newTestTP() (*sdktrace.TracerProvider, *tracetest.InMemoryExporter) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	return tp, exp
}

func runChain(t *testing.T, chain []celeris.HandlerFunc, method, path string, opts ...celeristest.Option) error {
	t.Helper()
	allOpts := make([]celeristest.Option, 0, len(opts)+1)
	allOpts = append(allOpts, celeristest.WithHandlers(chain...))
	allOpts = append(allOpts, opts...)
	ctx, _ := celeristest.NewContextT(t, method, path, allOpts...)
	return ctx.Next()
}

func TestSpanCreated(t *testing.T) {
	tp, exp := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	mw := New(Config{TracerProvider: tp})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/users")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	span := spans[0]
	if span.SpanKind != trace.SpanKindServer {
		t.Fatalf("expected SpanKindServer, got %v", span.SpanKind)
	}
}

func TestSpanName(t *testing.T) {
	tp, exp := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	mw := New(Config{TracerProvider: tp})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "POST", "/api/items")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	// FullPath is empty in test context (no router), so name is just "POST".
	if got := spans[0].Name; got != "POST" {
		t.Fatalf("expected span name %q, got %q", "POST", got)
	}
}

func TestSpanNameFormatter(t *testing.T) {
	tp, exp := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	mw := New(Config{
		TracerProvider: tp,
		SpanNameFormatter: func(c *celeris.Context) string {
			return "custom " + c.Path()
		},
	})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/custom-path")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if got := spans[0].Name; got != "custom /custom-path" {
		t.Fatalf("expected span name %q, got %q", "custom /custom-path", got)
	}
}

func TestSpanAttributes(t *testing.T) {
	tp, exp := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	mw := New(Config{TracerProvider: tp, CollectClientIP: true})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/attrs",
		celeristest.WithHeader("x-forwarded-for", "10.0.0.1"),
		celeristest.WithHeader("user-agent", "TestAgent/1.0"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	attrMap := make(map[string]attribute.Value, len(spans[0].Attributes))
	for _, a := range spans[0].Attributes {
		attrMap[string(a.Key)] = a.Value
	}

	if got := attrMap["http.request.method"].AsString(); got != "GET" {
		t.Fatalf("expected http.request.method=GET, got %q", got)
	}
	if got := attrMap["url.scheme"].AsString(); got != "http" {
		t.Fatalf("expected url.scheme=http, got %q", got)
	}
	if got := attrMap["client.address"].AsString(); got != "10.0.0.1" {
		t.Fatalf("expected client.address=10.0.0.1, got %q", got)
	}
	if got := attrMap["http.response.status_code"].AsInt64(); got != 200 {
		t.Fatalf("expected http.response.status_code=200, got %d", got)
	}
	if got := attrMap["user_agent.original"].AsString(); got != "TestAgent/1.0" {
		t.Fatalf("expected user_agent.original=TestAgent/1.0, got %q", got)
	}
	if got := attrMap["url.path"].AsString(); got != "/attrs" {
		t.Fatalf("expected url.path=/attrs, got %q", got)
	}
	if got := attrMap["http.response.body.size"].AsInt64(); got != 2 {
		t.Fatalf("expected http.response.body.size=2, got %d", got)
	}
}

func TestSpanStatusError(t *testing.T) {
	tp, exp := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	mw := New(Config{TracerProvider: tp})
	handler := func(c *celeris.Context) error { return c.String(503, "unavailable") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/fail")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if spans[0].Status.Code != codes.Error {
		t.Fatalf("expected span status Error for 503, got %v", spans[0].Status.Code)
	}
}

func TestSpanStatusUnset(t *testing.T) {
	tp, exp := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	mw := New(Config{TracerProvider: tp})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/ok")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if spans[0].Status.Code != codes.Unset {
		t.Fatalf("expected span status Unset for 200, got %v", spans[0].Status.Code)
	}
}

func TestHandlerError(t *testing.T) {
	tp, exp := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	mw := New(Config{TracerProvider: tp})
	want := errors.New("handler failed")
	handler := func(_ *celeris.Context) error { return want }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/err")
	if !errors.Is(err, want) {
		t.Fatalf("expected error %v, got %v", want, err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
}

func TestHandlerErrorRecordedOnSpan(t *testing.T) {
	tp, exp := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	mw := New(Config{TracerProvider: tp})
	want := errors.New("handler failed")
	handler := func(_ *celeris.Context) error { return want }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/err")
	if !errors.Is(err, want) {
		t.Fatalf("expected error %v, got %v", want, err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if spans[0].Status.Code != codes.Error {
		t.Fatalf("expected span status Error for handler error, got %v", spans[0].Status.Code)
	}
	if spans[0].Status.Description != "handler failed" {
		t.Fatalf("expected status description %q, got %q", "handler failed", spans[0].Status.Description)
	}
	// Verify error event was recorded.
	found := false
	for _, ev := range spans[0].Events {
		if ev.Name == "exception" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected exception event from RecordError, not found")
	}
}

func TestSkipFunc(t *testing.T) {
	tp, exp := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	mw := New(Config{
		TracerProvider: tp,
		Skip: func(c *celeris.Context) bool {
			return c.Path() == "/health"
		},
	})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/health")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 0 {
		t.Fatalf("expected 0 spans for skipped request, got %d", len(spans))
	}
}

func TestSkipPaths(t *testing.T) {
	tp, exp := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	mw := New(Config{
		TracerProvider: tp,
		SkipPaths:      []string{"/health", "/ready"},
	})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }

	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/health")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	err = runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/ready")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 0 {
		t.Fatalf("expected 0 spans for skipped paths, got %d", len(spans))
	}
}

func TestFilterAllowList(t *testing.T) {
	tp, exp := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	mw := New(Config{
		TracerProvider: tp,
		Filter: func(c *celeris.Context) bool {
			return c.Path() == "/traced"
		},
	})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }

	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/not-traced")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(exp.GetSpans()) != 0 {
		t.Fatalf("expected 0 spans for filtered path, got %d", len(exp.GetSpans()))
	}

	err = runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/traced")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(exp.GetSpans()) != 1 {
		t.Fatalf("expected 1 span for allowed path, got %d", len(exp.GetSpans()))
	}
}

func TestContextPropagation(t *testing.T) {
	tp, exp := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	prop := propagation.TraceContext{}

	mw := New(Config{
		TracerProvider: tp,
		Propagators:    prop,
	})

	// Create a parent span and inject its context into headers.
	parentTracer := tp.Tracer("test-parent")
	parentCtx, parentSpan := parentTracer.Start(context.Background(), "parent-op")
	defer parentSpan.End()

	carrier := propagation.MapCarrier{}
	prop.Inject(parentCtx, carrier)
	traceparent := carrier.Get("traceparent")
	if traceparent == "" {
		t.Fatal("expected traceparent header from parent span")
	}

	handler := func(c *celeris.Context) error {
		span := trace.SpanFromContext(c.Context())
		if !span.SpanContext().IsValid() {
			t.Error("expected valid span in context")
		}
		sc := span.SpanContext()
		parentSC := parentSpan.SpanContext()
		if sc.TraceID() != parentSC.TraceID() {
			t.Errorf("trace ID mismatch: got %s, want %s", sc.TraceID(), parentSC.TraceID())
		}
		return c.String(200, "ok")
	}

	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/propagated",
		celeristest.WithHeader("traceparent", traceparent))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) < 1 {
		t.Fatal("expected at least 1 span")
	}
}

func TestContextSet(t *testing.T) {
	tp, _ := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	mw := New(Config{TracerProvider: tp})
	var capturedCtx context.Context
	handler := func(c *celeris.Context) error {
		capturedCtx = c.Context()
		return c.String(200, "ok")
	}
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/ctx")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	span := trace.SpanFromContext(capturedCtx)
	if !span.SpanContext().IsValid() {
		t.Fatal("expected middleware to set a valid span context on c.Context()")
	}
}

func TestNoopProviders(t *testing.T) {
	tp := tracenoop.NewTracerProvider()
	mw := New(Config{TracerProvider: tp})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/noop")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDefaultConfig(t *testing.T) {
	// Ensure New() with no args does not panic.
	mw := New()
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDisableMetrics(t *testing.T) {
	tp, exp := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	mw := New(Config{
		TracerProvider: tp,
		DisableMetrics: true,
	})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/metrics-off")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span even with DisableMetrics, got %d", len(spans))
	}
	if spans[0].Status.Code == codes.Error {
		t.Fatal("expected non-error span status")
	}
}

func TestDisableMetricsWithError(t *testing.T) {
	tp, exp := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	mw := New(Config{
		TracerProvider: tp,
		DisableMetrics: true,
	})
	want := errors.New("fail")
	handler := func(_ *celeris.Context) error { return want }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/err")
	if !errors.Is(err, want) {
		t.Fatalf("expected error %v, got %v", want, err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if spans[0].Status.Code != codes.Error {
		t.Fatalf("expected Error status with DisableMetrics, got %v", spans[0].Status.Code)
	}
}

func TestDisableMetrics500(t *testing.T) {
	tp, exp := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	mw := New(Config{
		TracerProvider: tp,
		DisableMetrics: true,
	})
	handler := func(c *celeris.Context) error { return c.String(500, "fail") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/500")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if spans[0].Status.Code != codes.Error {
		t.Fatalf("expected Error status for 500 with DisableMetrics, got %v", spans[0].Status.Code)
	}
}

func TestCustomAttributes(t *testing.T) {
	tp, exp := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	mw := New(Config{
		TracerProvider: tp,
		CustomAttributes: func(_ *celeris.Context) []attribute.KeyValue {
			return []attribute.KeyValue{
				attribute.String("custom.key", "custom-value"),
				attribute.Int("custom.int", 42),
			}
		},
	})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/custom-attrs")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	attrMap := make(map[string]attribute.Value, len(spans[0].Attributes))
	for _, a := range spans[0].Attributes {
		attrMap[string(a.Key)] = a.Value
	}
	if got := attrMap["custom.key"].AsString(); got != "custom-value" {
		t.Fatalf("expected custom.key=custom-value, got %q", got)
	}
	if got := attrMap["custom.int"].AsInt64(); got != 42 {
		t.Fatalf("expected custom.int=42, got %d", got)
	}
	// Ensure standard attributes still present.
	if got := attrMap["http.request.method"].AsString(); got != "GET" {
		t.Fatalf("expected http.request.method=GET, got %q", got)
	}
}

func TestCustomMetricAttributes(t *testing.T) {
	tp, exp := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	mw := New(Config{
		TracerProvider: tp,
		CustomMetricAttributes: func(_ *celeris.Context) []attribute.KeyValue {
			return []attribute.KeyValue{
				attribute.String("region", "us-east-1"),
			}
		},
	})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/metric-attrs")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
}

func TestResponseBodySizeAttribute(t *testing.T) {
	tp, exp := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	mw := New(Config{TracerProvider: tp})
	handler := func(c *celeris.Context) error { return c.String(200, "hello world") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/body-size")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	attrMap := make(map[string]attribute.Value, len(spans[0].Attributes))
	for _, a := range spans[0].Attributes {
		attrMap[string(a.Key)] = a.Value
	}
	if got := attrMap["http.response.body.size"].AsInt64(); got != 11 {
		t.Fatalf("expected http.response.body.size=11, got %d", got)
	}
}

func TestUrlPathAttribute(t *testing.T) {
	tp, exp := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	mw := New(Config{TracerProvider: tp})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/api/users/123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	attrMap := make(map[string]attribute.Value, len(spans[0].Attributes))
	for _, a := range spans[0].Attributes {
		attrMap[string(a.Key)] = a.Value
	}
	if got := attrMap["url.path"].AsString(); got != "/api/users/123" {
		t.Fatalf("expected url.path=/api/users/123, got %q", got)
	}
}

func TestNetworkProtocolVersion(t *testing.T) {
	tp, exp := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	mw := New(Config{TracerProvider: tp})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/proto")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	var found bool
	for _, a := range spans[0].Attributes {
		if a.Key == semconv.NetworkProtocolVersionKey {
			found = true
			if v := a.Value.AsString(); v == "" {
				t.Fatalf("network.protocol.version is empty")
			}
			break
		}
	}
	if !found {
		t.Fatal("network.protocol.version attribute not found")
	}
}

func TestMetricSchemeAndServerAddress(t *testing.T) {
	tp, exp := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	mw := New(Config{TracerProvider: tp})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/scheme-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify span was created (metric attributes are internal, tested via no-panic).
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
}

// --- CollectClientIP tests ---

func TestCollectClientIPDefault(t *testing.T) {
	tp, exp := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	// Default: CollectClientIP=false, client.address should be absent.
	mw := New(Config{TracerProvider: tp})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/ip-test",
		celeristest.WithHeader("x-forwarded-for", "10.0.0.1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	for _, a := range spans[0].Attributes {
		if string(a.Key) == "client.address" {
			t.Fatal("client.address should be absent when CollectClientIP is false (default)")
		}
	}
}

func TestCollectClientIPTrue(t *testing.T) {
	tp, exp := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	mw := New(Config{TracerProvider: tp, CollectClientIP: true})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/ip-test",
		celeristest.WithHeader("x-forwarded-for", "10.0.0.1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	attrMap := make(map[string]attribute.Value, len(spans[0].Attributes))
	for _, a := range spans[0].Attributes {
		attrMap[string(a.Key)] = a.Value
	}
	if got := attrMap["client.address"].AsString(); got != "10.0.0.1" {
		t.Fatalf("expected client.address=10.0.0.1, got %q", got)
	}
}

// --- CollectUserAgent tests ---

func TestCollectUserAgentDefault(t *testing.T) {
	tp, exp := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	// Default: CollectUserAgent=nil (true), user_agent.original should be present.
	mw := New(Config{TracerProvider: tp})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/ua-test",
		celeristest.WithHeader("user-agent", "TestBot/1.0"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	attrMap := make(map[string]attribute.Value, len(spans[0].Attributes))
	for _, a := range spans[0].Attributes {
		attrMap[string(a.Key)] = a.Value
	}
	if got := attrMap["user_agent.original"].AsString(); got != "TestBot/1.0" {
		t.Fatalf("expected user_agent.original=TestBot/1.0, got %q", got)
	}
}

func TestCollectUserAgentFalse(t *testing.T) {
	tp, exp := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	f := false
	mw := New(Config{TracerProvider: tp, CollectUserAgent: &f})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/ua-test",
		celeristest.WithHeader("user-agent", "TestBot/1.0"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	for _, a := range spans[0].Attributes {
		if string(a.Key) == "user_agent.original" {
			t.Fatal("user_agent.original should be absent when CollectUserAgent is false")
		}
	}
}

func TestSpanFromContext(t *testing.T) {
	tp, _ := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	mw := New(Config{TracerProvider: tp})
	var span trace.Span
	handler := func(c *celeris.Context) error {
		span = SpanFromContext(c)
		return c.String(200, "ok")
	}
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/span-helper")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if span == nil {
		t.Fatal("SpanFromContext returned nil")
	}
	if !span.SpanContext().IsValid() {
		t.Fatal("SpanFromContext returned invalid span")
	}
}

func TestSpanFromContextWithoutMiddleware(t *testing.T) {
	handler := func(c *celeris.Context) error {
		span := SpanFromContext(c)
		if span == nil {
			t.Fatal("SpanFromContext should never return nil (returns noop span)")
		}
		return c.String(200, "ok")
	}
	err := runChain(t, []celeris.HandlerFunc{handler}, "GET", "/no-otel")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCollectUserAgentExplicitTrue(t *testing.T) {
	tp, exp := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	tr := true
	mw := New(Config{TracerProvider: tp, CollectUserAgent: &tr})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/ua-test",
		celeristest.WithHeader("user-agent", "TestBot/1.0"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	attrMap := make(map[string]attribute.Value, len(spans[0].Attributes))
	for _, a := range spans[0].Attributes {
		attrMap[string(a.Key)] = a.Value
	}
	if got := attrMap["user_agent.original"].AsString(); got != "TestBot/1.0" {
		t.Fatalf("expected user_agent.original=TestBot/1.0, got %q", got)
	}
}

// --- Semconv attribute key tests ---

func TestSemconvAttributeKeys(t *testing.T) {
	tp, exp := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	mw := New(Config{TracerProvider: tp, CollectClientIP: true})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/semconv-test",
		celeristest.WithHeader("x-forwarded-for", "10.0.0.1"),
		celeristest.WithHeader("user-agent", "TestAgent/1.0"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	attrMap := make(map[string]attribute.Value, len(spans[0].Attributes))
	for _, a := range spans[0].Attributes {
		attrMap[string(a.Key)] = a.Value
	}

	// Verify attribute keys match semconv constants exactly.
	// http.route is omitted because FullPath() is empty in test contexts (no router).
	wantKeys := map[attribute.Key]bool{
		semconv.HTTPRequestMethodKey:        true,
		semconv.URLSchemeKey:                true,
		semconv.URLPathKey:                  true,
		semconv.NetworkProtocolVersionKey:   true,
		semconv.ClientAddressKey:            true,
		semconv.ServerAddressKey:            true,
		semconv.UserAgentOriginalKey:        true,
		semconv.HTTPResponseStatusCodeKey:   true,
		semconv.HTTPResponseBodySizeKey:     true,
	}
	for _, a := range spans[0].Attributes {
		delete(wantKeys, a.Key)
	}
	if len(wantKeys) > 0 {
		missing := make([]string, 0, len(wantKeys))
		for k := range wantKeys {
			missing = append(missing, string(k))
		}
		t.Fatalf("missing expected semconv attributes: %s", strings.Join(missing, ", "))
	}
}

// --- Metric instrument tests ---

func newTestMetrics(t *testing.T) (*sdktrace.TracerProvider, *tracetest.InMemoryExporter, *sdkmetric.MeterProvider, *sdkmetric.ManualReader) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	return tp, exp, mp, reader
}

func collectMetrics(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("failed to collect metrics: %v", err)
	}
	return rm
}

func findMetric(rm metricdata.ResourceMetrics, name string) *metricdata.Metrics {
	for _, sm := range rm.ScopeMetrics {
		for i := range sm.Metrics {
			if sm.Metrics[i].Name == name {
				return &sm.Metrics[i]
			}
		}
	}
	return nil
}

func TestRequestDurationMetric(t *testing.T) {
	tp, _, mp, reader := newTestMetrics(t)
	defer func() {
		_ = tp.Shutdown(context.Background())
		_ = mp.Shutdown(context.Background())
	}()

	mw := New(Config{TracerProvider: tp, MeterProvider: mp})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/duration-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rm := collectMetrics(t, reader)
	m := findMetric(rm, "http.server.request.duration")
	if m == nil {
		t.Fatal("http.server.request.duration metric not found")
	}
	hist, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("expected Histogram[float64], got %T", m.Data)
	}
	if len(hist.DataPoints) != 1 {
		t.Fatalf("expected 1 data point, got %d", len(hist.DataPoints))
	}
	if hist.DataPoints[0].Count != 1 {
		t.Fatalf("expected count=1, got %d", hist.DataPoints[0].Count)
	}
}

func TestActiveRequestsMetric(t *testing.T) {
	tp, _, mp, reader := newTestMetrics(t)
	defer func() {
		_ = tp.Shutdown(context.Background())
		_ = mp.Shutdown(context.Background())
	}()

	mw := New(Config{TracerProvider: tp, MeterProvider: mp})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/active-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rm := collectMetrics(t, reader)
	m := findMetric(rm, "http.server.active_requests")
	if m == nil {
		t.Fatal("http.server.active_requests metric not found")
	}
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("expected Sum[int64], got %T", m.Data)
	}
	if len(sum.DataPoints) != 1 {
		t.Fatalf("expected 1 data point, got %d", len(sum.DataPoints))
	}
	// After request completes: +1 then -1 = 0.
	if sum.DataPoints[0].Value != 0 {
		t.Fatalf("expected active_requests=0 after completion, got %d", sum.DataPoints[0].Value)
	}
}

func TestResponseBodySizeMetric(t *testing.T) {
	tp, _, mp, reader := newTestMetrics(t)
	defer func() {
		_ = tp.Shutdown(context.Background())
		_ = mp.Shutdown(context.Background())
	}()

	mw := New(Config{TracerProvider: tp, MeterProvider: mp})
	handler := func(c *celeris.Context) error { return c.String(200, "hello world") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/resp-body-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rm := collectMetrics(t, reader)
	m := findMetric(rm, "http.server.response.body.size")
	if m == nil {
		t.Fatal("http.server.response.body.size metric not found")
	}
	hist, ok := m.Data.(metricdata.Histogram[int64])
	if !ok {
		t.Fatalf("expected Histogram[int64], got %T", m.Data)
	}
	if len(hist.DataPoints) != 1 {
		t.Fatalf("expected 1 data point, got %d", len(hist.DataPoints))
	}
	if hist.DataPoints[0].Count != 1 {
		t.Fatalf("expected count=1, got %d", hist.DataPoints[0].Count)
	}
	// "hello world" = 11 bytes
	if hist.DataPoints[0].Sum != 11 {
		t.Fatalf("expected sum=11, got %d", hist.DataPoints[0].Sum)
	}
}

func TestRequestBodySizeMetric(t *testing.T) {
	tp, _, mp, reader := newTestMetrics(t)
	defer func() {
		_ = tp.Shutdown(context.Background())
		_ = mp.Shutdown(context.Background())
	}()

	mw := New(Config{TracerProvider: tp, MeterProvider: mp})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	body := []byte(`{"key":"value"}`)
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "POST", "/req-body-test",
		celeristest.WithBody(body),
		celeristest.WithHeader("content-length", "15"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rm := collectMetrics(t, reader)
	m := findMetric(rm, "http.server.request.body.size")
	if m == nil {
		t.Fatal("http.server.request.body.size metric not found")
	}
	hist, ok := m.Data.(metricdata.Histogram[int64])
	if !ok {
		t.Fatalf("expected Histogram[int64], got %T", m.Data)
	}
	if len(hist.DataPoints) != 1 {
		t.Fatalf("expected 1 data point, got %d", len(hist.DataPoints))
	}
	if hist.DataPoints[0].Count != 1 {
		t.Fatalf("expected count=1, got %d", hist.DataPoints[0].Count)
	}
	if hist.DataPoints[0].Sum != 15 {
		t.Fatalf("expected sum=15, got %d", hist.DataPoints[0].Sum)
	}
}

func TestRequestBodySizeZeroNotRecorded(t *testing.T) {
	tp, _, mp, reader := newTestMetrics(t)
	defer func() {
		_ = tp.Shutdown(context.Background())
		_ = mp.Shutdown(context.Background())
	}()

	mw := New(Config{TracerProvider: tp, MeterProvider: mp})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/no-body")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rm := collectMetrics(t, reader)
	m := findMetric(rm, "http.server.request.body.size")
	if m != nil {
		hist, ok := m.Data.(metricdata.Histogram[int64])
		if ok && len(hist.DataPoints) > 0 && hist.DataPoints[0].Count > 0 {
			t.Fatal("http.server.request.body.size should not be recorded for requests with no body")
		}
	}
}

func TestResponseBodySizeZeroNotRecorded(t *testing.T) {
	tp, _, mp, reader := newTestMetrics(t)
	defer func() {
		_ = tp.Shutdown(context.Background())
		_ = mp.Shutdown(context.Background())
	}()

	mw := New(Config{TracerProvider: tp, MeterProvider: mp})
	handler := func(c *celeris.Context) error {
		c.Status(204)
		return nil
	}
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/no-resp-body")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rm := collectMetrics(t, reader)
	m := findMetric(rm, "http.server.response.body.size")
	if m != nil {
		hist, ok := m.Data.(metricdata.Histogram[int64])
		if ok && len(hist.DataPoints) > 0 && hist.DataPoints[0].Count > 0 {
			t.Fatal("http.server.response.body.size should not be recorded when no bytes written")
		}
	}
}

func TestServerPortSpanAttribute(t *testing.T) {
	tp, exp := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	mw := New(Config{TracerProvider: tp, ServerPort: 8080})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/port-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	attrMap := make(map[string]attribute.Value, len(spans[0].Attributes))
	for _, a := range spans[0].Attributes {
		attrMap[string(a.Key)] = a.Value
	}
	if got := attrMap["server.port"].AsInt64(); got != 8080 {
		t.Fatalf("expected server.port=8080, got %d", got)
	}
}

func TestServerPortZeroOmitted(t *testing.T) {
	tp, exp := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	mw := New(Config{TracerProvider: tp})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/no-port")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	for _, a := range spans[0].Attributes {
		if string(a.Key) == "server.port" {
			t.Fatal("server.port should be absent when ServerPort is 0")
		}
	}
}

func TestServerPortMetricAttribute(t *testing.T) {
	tp, _, mp, reader := newTestMetrics(t)
	defer func() {
		_ = tp.Shutdown(context.Background())
		_ = mp.Shutdown(context.Background())
	}()

	mw := New(Config{TracerProvider: tp, MeterProvider: mp, ServerPort: 9090})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/port-metric")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rm := collectMetrics(t, reader)
	m := findMetric(rm, "http.server.request.duration")
	if m == nil {
		t.Fatal("http.server.request.duration metric not found")
	}
	hist, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("expected Histogram[float64], got %T", m.Data)
	}
	if len(hist.DataPoints) != 1 {
		t.Fatalf("expected 1 data point, got %d", len(hist.DataPoints))
	}

	if v, found := hist.DataPoints[0].Attributes.Value(semconv.ServerPortKey); !found {
		t.Fatal("expected server.port metric attribute, not found")
	} else if v.AsInt64() != 9090 {
		t.Fatalf("expected server.port=9090, got %d", v.AsInt64())
	}
}

func TestMethodNormalizationStandard(t *testing.T) {
	tp, exp := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	for _, method := range []string{"GET", "HEAD", "POST", "PUT", "DELETE", "PATCH", "OPTIONS", "TRACE", "CONNECT"} {
		exp.Reset()
		mw := New(Config{TracerProvider: tp})
		handler := func(c *celeris.Context) error { return c.String(200, "ok") }
		err := runChain(t, []celeris.HandlerFunc{mw, handler}, method, "/method-test")
		if err != nil {
			t.Fatalf("unexpected error for %s: %v", method, err)
		}

		spans := exp.GetSpans()
		if len(spans) != 1 {
			t.Fatalf("[%s] expected 1 span, got %d", method, len(spans))
		}
		attrMap := make(map[string]attribute.Value, len(spans[0].Attributes))
		for _, a := range spans[0].Attributes {
			attrMap[string(a.Key)] = a.Value
		}
		if got := attrMap["http.request.method"].AsString(); got != method {
			t.Fatalf("[%s] expected http.request.method=%s, got %q", method, method, got)
		}
	}
}

func TestMethodNormalizationNonStandard(t *testing.T) {
	tp, exp := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	mw := New(Config{TracerProvider: tp})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "PURGE", "/purge-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	attrMap := make(map[string]attribute.Value, len(spans[0].Attributes))
	for _, a := range spans[0].Attributes {
		attrMap[string(a.Key)] = a.Value
	}
	if got := attrMap["http.request.method"].AsString(); got != "_OTHER" {
		t.Fatalf("expected http.request.method=_OTHER for PURGE, got %q", got)
	}
}

func TestMethodNormalizationNonStandardMetric(t *testing.T) {
	tp, _, mp, reader := newTestMetrics(t)
	defer func() {
		_ = tp.Shutdown(context.Background())
		_ = mp.Shutdown(context.Background())
	}()

	mw := New(Config{TracerProvider: tp, MeterProvider: mp})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "PURGE", "/purge-metric")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rm := collectMetrics(t, reader)
	m := findMetric(rm, "http.server.request.duration")
	if m == nil {
		t.Fatal("http.server.request.duration metric not found")
	}
	hist, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("expected Histogram[float64], got %T", m.Data)
	}
	if len(hist.DataPoints) != 1 {
		t.Fatalf("expected 1 data point, got %d", len(hist.DataPoints))
	}

	if v, found := hist.DataPoints[0].Attributes.Value(semconv.HTTPRequestMethodKey); !found {
		t.Fatal("expected http.request.method metric attribute, not found")
	} else if v.AsString() != "_OTHER" {
		t.Fatalf("expected http.request.method=_OTHER, got %q", v.AsString())
	}
}

func TestSSEResponseBodySizeSkipped(t *testing.T) {
	tp, _, mp, reader := newTestMetrics(t)
	defer func() {
		_ = tp.Shutdown(context.Background())
		_ = mp.Shutdown(context.Background())
	}()

	mw := New(Config{TracerProvider: tp, MeterProvider: mp})
	handler := func(c *celeris.Context) error {
		c.SetHeader("content-type", "text/event-stream")
		return c.Blob(200, "text/event-stream", []byte("data: hello\n\n"))
	}
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/sse-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rm := collectMetrics(t, reader)
	m := findMetric(rm, "http.server.response.body.size")
	if m != nil {
		hist, ok := m.Data.(metricdata.Histogram[int64])
		if ok && len(hist.DataPoints) > 0 && hist.DataPoints[0].Count > 0 {
			t.Fatal("http.server.response.body.size should not be recorded for SSE responses")
		}
	}
}

func TestNonSSEResponseBodySizeRecorded(t *testing.T) {
	tp, _, mp, reader := newTestMetrics(t)
	defer func() {
		_ = tp.Shutdown(context.Background())
		_ = mp.Shutdown(context.Background())
	}()

	mw := New(Config{TracerProvider: tp, MeterProvider: mp})
	handler := func(c *celeris.Context) error { return c.String(200, "hello") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/normal-resp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rm := collectMetrics(t, reader)
	m := findMetric(rm, "http.server.response.body.size")
	if m == nil {
		t.Fatal("http.server.response.body.size metric not found for non-SSE response")
	}
	hist, ok := m.Data.(metricdata.Histogram[int64])
	if !ok {
		t.Fatalf("expected Histogram[int64], got %T", m.Data)
	}
	if len(hist.DataPoints) != 1 {
		t.Fatalf("expected 1 data point, got %d", len(hist.DataPoints))
	}
	if hist.DataPoints[0].Sum != 5 {
		t.Fatalf("expected sum=5, got %d", hist.DataPoints[0].Sum)
	}
}

func TestMetricAttributeKeys(t *testing.T) {
	tp, _, mp, reader := newTestMetrics(t)
	defer func() {
		_ = tp.Shutdown(context.Background())
		_ = mp.Shutdown(context.Background())
	}()

	mw := New(Config{TracerProvider: tp, MeterProvider: mp})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/metric-keys")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rm := collectMetrics(t, reader)
	m := findMetric(rm, "http.server.request.duration")
	if m == nil {
		t.Fatal("http.server.request.duration metric not found")
	}
	hist, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("expected Histogram[float64], got %T", m.Data)
	}
	if len(hist.DataPoints) != 1 {
		t.Fatalf("expected 1 data point, got %d", len(hist.DataPoints))
	}

	// http.route is omitted because FullPath() is empty in test contexts (no router).
	attrs := hist.DataPoints[0].Attributes
	wantKeys := []attribute.Key{
		semconv.HTTPRequestMethodKey,
		semconv.URLSchemeKey,
		semconv.ServerAddressKey,
		semconv.HTTPResponseStatusCodeKey,
	}
	for _, k := range wantKeys {
		if _, found := attrs.Value(k); !found {
			t.Fatalf("expected metric attribute %q not found", k)
		}
	}
}

func TestMethodOriginalAttributeOnNonStandard(t *testing.T) {
	tp, exp := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	mw := New(Config{TracerProvider: tp})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "PURGE", "/original-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	attrMap := make(map[string]attribute.Value, len(spans[0].Attributes))
	for _, a := range spans[0].Attributes {
		attrMap[string(a.Key)] = a.Value
	}
	if got := attrMap["http.request.method"].AsString(); got != "_OTHER" {
		t.Fatalf("expected http.request.method=_OTHER, got %q", got)
	}
	if got := attrMap["http.request.method_original"].AsString(); got != "PURGE" {
		t.Fatalf("expected http.request.method_original=PURGE, got %q", got)
	}
}

func TestMethodOriginalAbsentForStandard(t *testing.T) {
	tp, exp := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	mw := New(Config{TracerProvider: tp})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/standard-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	for _, a := range spans[0].Attributes {
		if string(a.Key) == "http.request.method_original" {
			t.Fatal("http.request.method_original should be absent for standard methods")
		}
	}
}

func TestMethodOriginalVariousMethods(t *testing.T) {
	for _, method := range []string{"FOOBAR", "LOCK", "MKCOL", "PROPFIND"} {
		tp, exp := newTestTP()
		mw := New(Config{TracerProvider: tp})
		handler := func(c *celeris.Context) error { return c.String(200, "ok") }
		err := runChain(t, []celeris.HandlerFunc{mw, handler}, method, "/method-orig")
		if err != nil {
			t.Fatalf("[%s] unexpected error: %v", method, err)
		}

		spans := exp.GetSpans()
		if len(spans) != 1 {
			t.Fatalf("[%s] expected 1 span, got %d", method, len(spans))
		}
		attrMap := make(map[string]attribute.Value, len(spans[0].Attributes))
		for _, a := range spans[0].Attributes {
			attrMap[string(a.Key)] = a.Value
		}
		if got := attrMap["http.request.method_original"].AsString(); got != method {
			t.Fatalf("[%s] expected http.request.method_original=%s, got %q", method, method, got)
		}
		_ = tp.Shutdown(context.Background())
	}
}

func TestResponseHeaderPropagation(t *testing.T) {
	traceTP, _ := newTestTP()
	defer func() { _ = traceTP.Shutdown(context.Background()) }()

	prop := propagation.TraceContext{}
	mw := New(Config{TracerProvider: traceTP, Propagators: prop})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }

	allOpts := []celeristest.Option{celeristest.WithHandlers(mw, handler)}
	ctx, rec := celeristest.NewContextT(t, "GET", "/resp-prop", allOpts...)
	err := ctx.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	traceparent := rec.Header("traceparent")
	if traceparent == "" {
		t.Fatal("expected traceparent response header from response propagation, got empty")
	}
	if !strings.HasPrefix(traceparent, "00-") {
		t.Fatalf("traceparent should start with version 00-, got %q", traceparent)
	}
}

func TestResponseHeaderPropagationDisableMetrics(t *testing.T) {
	traceTP, _ := newTestTP()
	defer func() { _ = traceTP.Shutdown(context.Background()) }()

	prop := propagation.TraceContext{}
	mw := New(Config{TracerProvider: traceTP, Propagators: prop, DisableMetrics: true})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }

	allOpts := []celeristest.Option{celeristest.WithHandlers(mw, handler)}
	ctx, rec := celeristest.NewContextT(t, "GET", "/resp-prop-no-metrics", allOpts...)
	err := ctx.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	traceparent := rec.Header("traceparent")
	if traceparent == "" {
		t.Fatal("expected traceparent response header with DisableMetrics, got empty")
	}
	if !strings.HasPrefix(traceparent, "00-") {
		t.Fatalf("traceparent should start with version 00-, got %q", traceparent)
	}
}

// --- truncateString UTF-8 safety tests ---

func TestTruncateStringASCII(t *testing.T) {
	got := truncateString("hello world", 5)
	if got != "hello" {
		t.Fatalf("expected %q, got %q", "hello", got)
	}
}

func TestTruncateStringNoTruncation(t *testing.T) {
	got := truncateString("short", 100)
	if got != "short" {
		t.Fatalf("expected %q, got %q", "short", got)
	}
}

func TestTruncateStringExactLength(t *testing.T) {
	got := truncateString("exact", 5)
	if got != "exact" {
		t.Fatalf("expected %q, got %q", "exact", got)
	}
}

func TestTruncateStringMultiByteNotSplit(t *testing.T) {
	// "\u00e4" is U+00E4 (a-umlaut), 2 bytes in UTF-8: 0xC3 0xA4
	s := "a\u00e4b" // "a" (1) + "a-umlaut" (2) + "b" (1) = 4 bytes
	// Truncate at 2 bytes: should NOT split the 2-byte rune.
	got := truncateString(s, 2)
	// Byte 0 is 'a', bytes 1-2 are the a-umlaut. maxLen=2 would cut into
	// the rune at byte 1. The function should back up to byte 1.
	if got != "a" {
		t.Fatalf("expected %q, got %q", "a", got)
	}
}

func TestTruncateStringMultiByteExactBoundary(t *testing.T) {
	// "a" (1) + "a-umlaut" (2) = 3 bytes. Truncate at 3 keeps the whole thing.
	s := "a\u00e4"
	got := truncateString(s, 3)
	if got != s {
		t.Fatalf("expected %q, got %q", s, got)
	}
}

func TestTruncateString3ByteRune(t *testing.T) {
	// U+4E16 (CJK char) is 3 bytes in UTF-8.
	s := "ab\u4e16c" // 2 + 3 + 1 = 6 bytes
	// Truncate at 4 would split the 3-byte rune starting at byte 2.
	got := truncateString(s, 4)
	if got != "ab" {
		t.Fatalf("expected %q, got %q", "ab", got)
	}
}

func TestTruncateString4ByteRune(t *testing.T) {
	// U+1F600 (grinning face emoji) is 4 bytes in UTF-8.
	s := "x\U0001F600y" // 1 + 4 + 1 = 6 bytes
	// Truncate at 3 would split the 4-byte rune starting at byte 1.
	got := truncateString(s, 3)
	if got != "x" {
		t.Fatalf("expected %q, got %q", "x", got)
	}
}

func TestTruncateStringEmpty(t *testing.T) {
	got := truncateString("", 10)
	if got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

// --- v1.2.4 celeristest option tests ---

func TestProtocolAttributeH1(t *testing.T) {
	tp, exp := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	mw := New(Config{TracerProvider: tp})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/proto-h1",
		celeristest.WithProtocol("1.1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	attrMap := make(map[string]attribute.Value, len(spans[0].Attributes))
	for _, a := range spans[0].Attributes {
		attrMap[string(a.Key)] = a.Value
	}
	if got := attrMap[string(semconv.NetworkProtocolVersionKey)].AsString(); got != "1.1" {
		t.Fatalf("expected network.protocol.version=1.1, got %q", got)
	}
}

func TestFullPathInSpanName(t *testing.T) {
	tp, exp := newTestTP()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	mw := New(Config{TracerProvider: tp})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	err := runChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/users/42",
		celeristest.WithFullPath("/users/:id"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	wantName := "GET /users/:id"
	if got := spans[0].Name; got != wantName {
		t.Fatalf("expected span name %q, got %q", wantName, got)
	}

	attrMap := make(map[string]attribute.Value, len(spans[0].Attributes))
	for _, a := range spans[0].Attributes {
		attrMap[string(a.Key)] = a.Value
	}
	if got := attrMap[string(semconv.HTTPRouteKey)].AsString(); got != "/users/:id" {
		t.Fatalf("expected http.route=/users/:id, got %q", got)
	}
}
