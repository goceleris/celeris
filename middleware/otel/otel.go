package otel

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/goceleris/celeris"

	otelglobal "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.32.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	tracerName             = "github.com/goceleris/celeris/middleware/otel"
	instrumentationVersion = "0.2.0"
	maxErrorLen            = 256
)

// standardMethods is the set of HTTP methods recognized by the OTel semconv spec.
// Non-standard methods are normalized to "_OTHER".
var standardMethods = map[string]struct{}{
	"GET":     {},
	"HEAD":    {},
	"POST":    {},
	"PUT":     {},
	"DELETE":  {},
	"PATCH":   {},
	"OPTIONS": {},
	"TRACE":   {},
	"CONNECT": {},
}

// normalizeMethod returns the method if it is a standard HTTP method,
// or "_OTHER" per the OTel semconv specification.
func normalizeMethod(method string) string {
	if _, ok := standardMethods[method]; ok {
		return method
	}
	return "_OTHER"
}

// truncateString truncates s to maxLen bytes without splitting multi-byte
// UTF-8 runes. After slicing at maxLen it backs up to the last valid rune
// boundary using [utf8.DecodeLastRuneInString].
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	truncated := s[:maxLen]
	// If the truncation point lands in the middle of a multi-byte rune,
	// the trailing bytes form an incomplete sequence. DecodeLastRune
	// returns RuneError for such bytes. Strip them one at a time.
	for len(truncated) > 0 {
		r, _ := utf8.DecodeLastRuneInString(truncated)
		if r != utf8.RuneError {
			break
		}
		// RuneError with valid single-byte 0xFFFD encoding would not
		// appear here since we are truncating a valid string.
		truncated = truncated[:len(truncated)-1]
	}
	return truncated
}

// isSSE reports whether the response Content-Type indicates a Server-Sent Events stream.
func isSSE(c *celeris.Context) bool {
	for _, h := range c.ResponseHeaders() {
		// c.SetHeader lowercases keys at storage, so exact == on
		// "content-type" is both correct and ~6x faster than
		// strings.EqualFold on a full-header sweep.
		if h[0] == "content-type" && strings.HasPrefix(h[1], "text/event-stream") {
			return true
		}
	}
	return false
}

// SpanFromContext returns the active OpenTelemetry span from the request context.
// This is a convenience wrapper around trace.SpanFromContext(c.Context()).
func SpanFromContext(c *celeris.Context) trace.Span {
	return trace.SpanFromContext(c.Context())
}

// New creates an OpenTelemetry tracing and metrics middleware.
func New(config ...Config) celeris.HandlerFunc {
	cfg := defaultConfig
	if len(config) > 0 {
		cfg = config[0]
	}
	cfg = applyDefaults(cfg)
	cfg.validate()

	tracer := cfg.TracerProvider.Tracer(tracerName,
		trace.WithInstrumentationVersion(instrumentationVersion),
	)

	metricsEnabled := !cfg.DisableMetrics
	var requestDuration metric.Float64Histogram
	var activeRequests metric.Int64UpDownCounter
	var requestBodySize metric.Int64Histogram
	var responseBodySize metric.Int64Histogram
	if metricsEnabled {
		meter := cfg.MeterProvider.Meter(tracerName,
			metric.WithInstrumentationVersion(instrumentationVersion),
		)
		var err error
		requestDuration, err = meter.Float64Histogram(
			"http.server.request.duration",
			metric.WithUnit("s"),
			metric.WithDescription("Duration of HTTP server requests"),
		)
		if err != nil {
			otelglobal.Handle(err)
		}
		activeRequests, err = meter.Int64UpDownCounter(
			"http.server.active_requests",
			metric.WithDescription("Number of active HTTP server requests"),
		)
		if err != nil {
			otelglobal.Handle(err)
		}
		requestBodySize, err = meter.Int64Histogram(
			"http.server.request.body.size",
			metric.WithUnit("By"),
			metric.WithDescription("Size of HTTP server request bodies"),
		)
		if err != nil {
			otelglobal.Handle(err)
		}
		responseBodySize, err = meter.Int64Histogram(
			"http.server.response.body.size",
			metric.WithUnit("By"),
			metric.WithDescription("Size of HTTP server response bodies"),
		)
		if err != nil {
			otelglobal.Handle(err)
		}
	}

	skipMap := make(map[string]struct{}, len(cfg.SkipPaths))
	for _, p := range cfg.SkipPaths {
		skipMap[p] = struct{}{}
	}

	propagators := cfg.Propagators
	spanNameFmt := cfg.SpanNameFormatter
	serverSpanKind := trace.WithSpanKind(trace.SpanKindServer)
	customAttrs := cfg.CustomAttributes
	customMetricAttrs := cfg.CustomMetricAttributes
	collectClientIP := cfg.CollectClientIP
	collectUserAgent := cfg.CollectUserAgent == nil || *cfg.CollectUserAgent
	serverPort := cfg.ServerPort

	return func(c *celeris.Context) error {
		if cfg.Skip != nil && cfg.Skip(c) {
			return c.Next()
		}
		if cfg.Filter != nil && !cfg.Filter(c) {
			return c.Next()
		}
		if _, ok := skipMap[c.Path()]; ok {
			return c.Next()
		}

		carrier := headerCarrier{ctx: c}
		parentCtx := propagators.Extract(c.Context(), carrier)

		spanName := c.Method()
		if fp := c.FullPath(); fp != "" {
			spanName += " " + fp
		}
		if spanNameFmt != nil {
			spanName = spanNameFmt(c)
		}

		rawMethod := c.Method()
		method := normalizeMethod(rawMethod)

		// With celeris v1.2.4+ core, FullPath() returns "<unmatched>" for 404
		// and "<method-not-allowed>" for 405 when the request passes through
		// the core router. When FullPath is empty (no core router, or
		// pre-v1.2.4), the http.route attribute is simply omitted.
		route := c.FullPath()

		var spanBuf [14]attribute.KeyValue
		n := 0
		spanBuf[n] = semconv.HTTPRequestMethodKey.String(method)
		n++
		if method != rawMethod {
			spanBuf[n] = attribute.String("http.request.method_original", rawMethod)
			n++
		}
		if route != "" {
			spanBuf[n] = semconv.HTTPRoute(route)
			n++
		}
		spanBuf[n] = semconv.URLScheme(c.Scheme())
		n++
		spanBuf[n] = semconv.URLPath(c.Path())
		n++
		spanBuf[n] = semconv.NetworkProtocolVersion(c.Protocol())
		n++
		if collectClientIP {
			spanBuf[n] = semconv.ClientAddress(c.ClientIP())
			n++
		}
		spanBuf[n] = semconv.ServerAddress(c.Host())
		n++
		if serverPort > 0 {
			spanBuf[n] = semconv.ServerPort(serverPort)
			n++
		}
		if collectUserAgent {
			spanBuf[n] = semconv.UserAgentOriginal(c.Header("user-agent"))
			n++
		}
		spanAttrs := spanBuf[:n]
		if customAttrs != nil {
			spanAttrs = append(spanAttrs, customAttrs(c)...)
		}

		spanCtx, span := tracer.Start(parentCtx, spanName,
			serverSpanKind,
			trace.WithAttributes(spanAttrs...),
		)
		defer span.End()

		c.SetContext(spanCtx)

		if rid, ok := c.Get(celeris.RequestIDKey); ok {
			if s, ok := rid.(string); ok && s != "" {
				span.SetAttributes(attribute.String("request.id", s))
			}
		}

		if metricsEnabled {
			var metricBuf [7]attribute.KeyValue
			mn := 0
			metricBuf[mn] = semconv.HTTPRequestMethodKey.String(method)
			mn++
			if route != "" {
				metricBuf[mn] = semconv.HTTPRoute(route)
				mn++
			}
			metricBuf[mn] = semconv.URLScheme(c.Scheme())
			mn++
			metricBuf[mn] = semconv.ServerAddress(c.Host())
			mn++
			if serverPort > 0 {
				metricBuf[mn] = semconv.ServerPort(serverPort)
				mn++
			}
			metricBaseAttrs := metricBuf[:mn:mn]
			if customMetricAttrs != nil {
				metricBaseAttrs = append(metricBaseAttrs, customMetricAttrs(c)...)
			}
			activeAttrSet := metric.WithAttributeSet(attribute.NewSet(metricBaseAttrs...))
			if activeRequests != nil {
				activeRequests.Add(spanCtx, 1, activeAttrSet)
				// Defer ensures we decrement even if a panic escapes
				// this middleware (recovery ordered outside otel).
				defer activeRequests.Add(spanCtx, -1, activeAttrSet)
			}
			err := c.Next()

			propagators.Inject(spanCtx, carrier)

			duration := time.Since(c.StartTime()).Seconds()
			status := c.StatusCode()

			if span.IsRecording() {
				span.SetAttributes(
					semconv.HTTPResponseStatusCode(status),
					semconv.HTTPResponseBodySize(c.BytesWritten()),
				)
			}

			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, truncateString(err.Error(), maxErrorLen))
			} else if status >= 500 {
				span.SetStatus(codes.Error, "")
			}

			allAttrs := append(metricBaseAttrs[:len(metricBaseAttrs):len(metricBaseAttrs)], semconv.HTTPResponseStatusCode(status))
			fullAttrSet := metric.WithAttributeSet(attribute.NewSet(allAttrs...))
			if requestDuration != nil {
				requestDuration.Record(spanCtx, duration, fullAttrSet)
			}
			// activeRequests decrement happens in the defer above.

			if reqSize := c.ContentLength(); reqSize > 0 && requestBodySize != nil {
				requestBodySize.Record(spanCtx, reqSize, fullAttrSet)
			}
			if respSize := int64(c.BytesWritten()); respSize > 0 && !isSSE(c) && responseBodySize != nil {
				responseBodySize.Record(spanCtx, respSize, fullAttrSet)
			}

			return err
		}

		err := c.Next()

		propagators.Inject(spanCtx, carrier)

		status := c.StatusCode()

		if span.IsRecording() {
			span.SetAttributes(
				semconv.HTTPResponseStatusCode(status),
				semconv.HTTPResponseBodySize(c.BytesWritten()),
			)
		}

		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, truncateString(err.Error(), maxErrorLen))
		} else if status >= 500 {
			span.SetStatus(codes.Error, "")
		}

		return err
	}
}
