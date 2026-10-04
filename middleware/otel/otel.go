package otel

import (
	"reflect"
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
var standardMethods = map[string]string{
	"GET":     "GET",
	"HEAD":    "HEAD",
	"POST":    "POST",
	"PUT":     "PUT",
	"DELETE":  "DELETE",
	"PATCH":   "PATCH",
	"OPTIONS": "OPTIONS",
	"TRACE":   "TRACE",
	"CONNECT": "CONNECT",
}

// normalizeMethod returns the method if it is a standard HTTP method,
// or "_OTHER" per the OTel semconv specification. It returns the package's
// own constant, never method itself: the request method can be a view of the
// connection's receive buffer (see ownStrings), and the result goes into
// attributes the SDK keeps.
func normalizeMethod(method string) string {
	if m, ok := standardMethods[method]; ok {
		return m
	}
	return "_OTHER"
}

// metricScheme returns the url.scheme a metric attribute set carries: http
// or https, the package's own constants, or _OTHER for any other value, which
// only an override set with Context.SetScheme can give (celeris#924).
func metricScheme(scheme string) string {
	switch scheme {
	case "http":
		return "http"
	case "https":
		return "https"
	}
	return "_OTHER"
}

// ownStrings replaces each *p with a copy. The copies share one allocation,
// and nothing is allocated when every string is empty.
//
// On epoll and io_uring the request path, Host, headers and the strings
// derived from them are views of the connection's receive buffer, which the
// engine reuses for the connection's next request and, once the connection
// closes, for another connection. The attributes built from them outlive the
// request: a span processor keeps an ended span until it exports it, and the
// metric SDK keeps every attribute set it has seen as an aggregation key for
// the life of the provider. A kept view would read other request bytes,
// including another client's headers (celeris#732).
func ownStrings(ps ...*string) {
	n := 0
	for _, p := range ps {
		n += len(*p)
	}
	if n == 0 {
		return
	}
	var b strings.Builder
	b.Grow(n)
	for _, p := range ps {
		b.WriteString(*p)
	}
	rest := b.String()
	for _, p := range ps {
		l := len(*p)
		*p, rest = rest[:l], rest[l:]
	}
}

// appendOwned appends attrs to dst with every string in them copied: the
// keys, STRING and STRINGSLICE values, and the values and map keys nested
// in SLICE and MAP values, at any depth. The attributes come from
// CustomAttributes or CustomMetricAttributes, which typically read request
// headers (views, see ownStrings); a request string can sit in any of those
// places, including a key (celeris#742). The copies share one allocation.
// BYTESLICE values are copies already (attribute.ByteSliceValue converts).
func appendOwned(dst, attrs []attribute.KeyValue) []attribute.KeyValue {
	var o owner
	k := 0
	for _, kv := range attrs {
		switch kv.Value.Type() {
		case attribute.STRINGSLICE, attribute.SLICE, attribute.MAP:
			k++
		}
	}
	if k > 0 {
		// Room for the top-level lists; nested ones grow it.
		o.lists = make([]ownedList, 0, k)
	}
	for _, kv := range attrs {
		o.measureKV(kv)
	}
	if o.n == 0 {
		return append(dst, attrs...)
	}
	var b strings.Builder
	b.Grow(o.n)
	for _, kv := range attrs {
		o.writeKV(&b, kv)
	}
	o.rest, o.next = b.String(), 0
	for _, kv := range attrs {
		dst = append(dst, o.cutKV(kv))
	}
	return dst
}

// owner walks attributes three times in the same order: measure sums the
// strings' lengths, write copies them into one buffer, and cut builds the
// owned attributes from that buffer. A STRINGSLICE, SLICE or MAP value is
// read out of the attribute (each As* call returns a new slice) once, in
// measure, and the next passes reuse that list.
type owner struct {
	n     int
	lists []ownedList
	next  int
	rest  string
}

// ownedList is one STRINGSLICE, SLICE or MAP value's elements.
type ownedList struct {
	ss  []string
	vs  []attribute.Value
	kvs []attribute.KeyValue
}

func (o *owner) measureKV(kv attribute.KeyValue) {
	o.n += len(kv.Key)
	o.measure(kv.Value)
}

func (o *owner) measure(v attribute.Value) {
	switch v.Type() {
	case attribute.STRING:
		o.n += len(v.AsString())
	case attribute.STRINGSLICE:
		ss := v.AsStringSlice()
		o.lists = append(o.lists, ownedList{ss: ss})
		for _, s := range ss {
			o.n += len(s)
		}
	case attribute.SLICE:
		vs := v.AsSlice()
		o.lists = append(o.lists, ownedList{vs: vs})
		for _, e := range vs {
			o.measure(e)
		}
	case attribute.MAP:
		kvs := v.AsMap()
		o.lists = append(o.lists, ownedList{kvs: kvs})
		for _, e := range kvs {
			o.measureKV(e)
		}
	}
}

func (o *owner) writeKV(b *strings.Builder, kv attribute.KeyValue) {
	b.WriteString(string(kv.Key))
	o.write(b, kv.Value)
}

func (o *owner) write(b *strings.Builder, v attribute.Value) {
	switch v.Type() {
	case attribute.STRING:
		b.WriteString(v.AsString())
	case attribute.STRINGSLICE, attribute.SLICE, attribute.MAP:
		l := o.lists[o.next]
		o.next++
		for _, s := range l.ss {
			b.WriteString(s)
		}
		for _, e := range l.vs {
			o.write(b, e)
		}
		for _, e := range l.kvs {
			o.writeKV(b, e)
		}
	}
}

func (o *owner) cut(l int) string {
	s := o.rest[:l]
	o.rest = o.rest[l:]
	return s
}

func (o *owner) cutKV(kv attribute.KeyValue) attribute.KeyValue {
	key := attribute.Key(o.cut(len(kv.Key)))
	return attribute.KeyValue{Key: key, Value: o.cutValue(kv.Value)}
}

func (o *owner) cutValue(v attribute.Value) attribute.Value {
	switch v.Type() {
	case attribute.STRING:
		return attribute.StringValue(o.cut(len(v.AsString())))
	case attribute.STRINGSLICE:
		l := o.lists[o.next]
		o.next++
		for i, s := range l.ss { // the list is a copy: As* returned a new slice
			l.ss[i] = o.cut(len(s))
		}
		return attribute.StringSliceValue(l.ss)
	case attribute.SLICE:
		l := o.lists[o.next]
		o.next++
		for i, e := range l.vs {
			l.vs[i] = o.cutValue(e)
		}
		return attribute.SliceValue(l.vs...)
	case attribute.MAP:
		l := o.lists[o.next]
		o.next++
		for i, e := range l.kvs {
			l.kvs[i] = o.cutKV(e)
		}
		return attribute.MapValue(l.kvs...)
	}
	return v
}

// recordError records the handler's error on span: an exception event and
// the error status, both from one copy of its message, made only when the
// span records.
//
// The span keeps both until it is exported. An error's message can be a
// request string, errors.New(c.Param("id")) or an HTTPError whose Message is
// a header, and on epoll and io_uring that is a view of the connection's
// receive buffer, which the engine reuses for the connection's next request
// and, once the connection closes, for another connection (celeris#732).
// span.RecordError calls err.Error() itself and keeps what it returns, so
// the event is built here as the SDK's RecordError builds it.
func recordError(span trace.Span, err error) {
	if !span.IsRecording() {
		return
	}
	msg := strings.Clone(err.Error())
	span.AddEvent(semconv.ExceptionEventName, trace.WithAttributes(
		semconv.ExceptionType(errorType(err)),
		semconv.ExceptionMessage(msg),
	))
	span.SetStatus(codes.Error, truncateString(msg, maxErrorLen))
}

// errorType names err's type as the OTel SDK's RecordError does: package
// path and name, or the type's string for an unnamed type such as a pointer.
func errorType(err error) string {
	t := reflect.TypeOf(err)
	if t.PkgPath() == "" && t.Name() == "" {
		return t.String()
	}
	return t.PkgPath() + "." + t.Name()
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
	metricServerAddress := cfg.MetricServerAddress

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

		rawMethod := c.Method()
		method := normalizeMethod(rawMethod)

		// With celeris v1.2.4+ core, FullPath() returns "<unmatched>" for 404
		// and "<method-not-allowed>" for 405 when the request passes through
		// the core router. When FullPath is empty (no core router, or
		// pre-v1.2.4), the http.route attribute is simply omitted.
		route := c.FullPath()

		// The request strings the span and the metric attribute sets keep,
		// copied (see ownStrings). The route is the registered pattern and
		// the protocol a constant.
		spanName := rawMethod
		if spanNameFmt != nil {
			spanName = spanNameFmt(c)
		}
		var methodOrig, clientIP, userAgent string
		if method != rawMethod {
			methodOrig = rawMethod
		}
		if collectClientIP {
			clientIP = c.ClientIP()
		}
		if collectUserAgent {
			userAgent = c.Header("user-agent")
		}
		scheme, path, host, requestID := c.Scheme(), c.Path(), c.Host(), c.RequestID()
		ownStrings(&spanName, &methodOrig, &scheme, &path, &clientIP, &host, &userAgent, &requestID)
		if spanNameFmt == nil && route != "" {
			spanName += " " + route
		}

		var spanBuf [14]attribute.KeyValue
		n := 0
		spanBuf[n] = semconv.HTTPRequestMethodKey.String(method)
		n++
		if method != rawMethod {
			spanBuf[n] = attribute.String("http.request.method_original", methodOrig)
			n++
		}
		if route != "" {
			spanBuf[n] = semconv.HTTPRoute(route)
			n++
		}
		spanBuf[n] = semconv.URLScheme(scheme)
		n++
		spanBuf[n] = semconv.URLPath(path)
		n++
		spanBuf[n] = semconv.NetworkProtocolVersion(c.Protocol())
		n++
		if collectClientIP {
			spanBuf[n] = semconv.ClientAddress(clientIP)
			n++
		}
		spanBuf[n] = semconv.ServerAddress(host)
		n++
		if serverPort > 0 {
			spanBuf[n] = semconv.ServerPort(serverPort)
			n++
		}
		if collectUserAgent {
			spanBuf[n] = semconv.UserAgentOriginal(userAgent)
			n++
		}
		spanAttrs := spanBuf[:n]
		if customAttrs != nil {
			spanAttrs = appendOwned(spanAttrs, customAttrs(c))
		}

		spanCtx, span := tracer.Start(parentCtx, spanName,
			serverSpanKind,
			trace.WithAttributes(spanAttrs...),
		)
		defer span.End()

		c.SetContext(spanCtx)

		if requestID != "" {
			span.SetAttributes(attribute.String("request.id", requestID))
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
			// url.scheme is bounded as the method is: c.Scheme() is http
			// or https unless a middleware overrode it (celeris#924).
			metricBuf[mn] = semconv.URLScheme(metricScheme(scheme))
			mn++
			// server.address is the client's Host: opt-in on the metrics,
			// or one client could make unbounded series (celeris#924).
			if metricServerAddress {
				metricBuf[mn] = semconv.ServerAddress(host)
				mn++
			}
			if serverPort > 0 {
				metricBuf[mn] = semconv.ServerPort(serverPort)
				mn++
			}
			metricBaseAttrs := metricBuf[:mn:mn]
			if customMetricAttrs != nil {
				metricBaseAttrs = appendOwned(metricBaseAttrs, customMetricAttrs(c))
			}
			activeAttrSet := metric.WithAttributeSet(attribute.NewSet(metricBaseAttrs...))
			if activeRequests != nil {
				activeRequests.Add(spanCtx, 1, activeAttrSet)
				// Defer ensures we decrement even if a panic escapes
				// this middleware (recovery ordered outside otel).
				defer activeRequests.Add(spanCtx, -1, activeAttrSet)
			}
			// Inject the response trace context BEFORE the handler runs:
			// celeris writes response headers to the wire as soon as the
			// handler emits a body, so a header added after c.Next() would be
			// silently dropped (the same defect as celeris#507 for the
			// session cookie). The span context is fixed once the span has
			// started, so injecting here is equivalent for the client.
			propagators.Inject(spanCtx, carrier)

			err := c.Next()

			duration := time.Since(c.StartTime()).Seconds()
			status := c.StatusCode()

			if span.IsRecording() {
				span.SetAttributes(
					semconv.HTTPResponseStatusCode(status),
					semconv.HTTPResponseBodySize(c.BytesWritten()),
				)
			}

			if err != nil {
				recordError(span, err)
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

		// See above: inject before the handler can write the body.
		propagators.Inject(spanCtx, carrier)

		err := c.Next()

		status := c.StatusCode()

		if span.IsRecording() {
			span.SetAttributes(
				semconv.HTTPResponseStatusCode(status),
				semconv.HTTPResponseBodySize(c.BytesWritten()),
			)
		}

		if err != nil {
			recordError(span, err)
		} else if status >= 500 {
			span.SetStatus(codes.Error, "")
		}

		return err
	}
}
