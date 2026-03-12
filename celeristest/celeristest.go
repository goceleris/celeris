// Package celeristest provides test utilities for celeris handlers.
//
// Use [NewContext] to create a [celeris.Context] and [ResponseRecorder] pair,
// then pass the context to a handler and inspect the recorder.
//
//	ctx, rec := celeristest.NewContext("GET", "/hello")
//	defer celeristest.ReleaseContext(ctx)
//	err := handler(ctx)
//	if rec.StatusCode != 200 { ... }
package celeristest

import (
	"net/url"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/internal/ctxkit"
	"github.com/goceleris/celeris/protocol/h2/stream"

	"golang.org/x/net/http2"
)

// ResponseRecorder captures the response written by a handler.
type ResponseRecorder struct {
	// StatusCode is the HTTP status code written by the handler.
	StatusCode int
	// Headers are the response headers as key-value pairs.
	Headers [][2]string
	// Body is the raw response body bytes.
	Body []byte
}

// Header returns the value of the first response header matching the given
// key. Returns empty string if the header is not present.
func (r *ResponseRecorder) Header(key string) string {
	for _, h := range r.Headers {
		if h[0] == key {
			return h[1]
		}
	}
	return ""
}

// BodyString returns the response body as a string.
func (r *ResponseRecorder) BodyString() string {
	return string(r.Body)
}

// recorderWriter adapts a ResponseRecorder to the internal
// stream.ResponseWriter interface without leaking internal types
// in the public API.
type recorderWriter struct {
	rec *ResponseRecorder
}

func (w *recorderWriter) WriteResponse(_ *stream.Stream, status int, headers [][2]string, body []byte) error {
	w.rec.StatusCode = status
	w.rec.Headers = headers
	w.rec.Body = make([]byte, len(body))
	copy(w.rec.Body, body)
	return nil
}

func (w *recorderWriter) SendGoAway(_ uint32, _ http2.ErrCode, _ []byte) error { return nil }
func (w *recorderWriter) MarkStreamClosed(_ uint32)                            {}
func (w *recorderWriter) IsStreamClosed(_ uint32) bool                         { return false }
func (w *recorderWriter) WriteRSTStreamPriority(_ uint32, _ http2.ErrCode) error {
	return nil
}
func (w *recorderWriter) CloseConn() error { return nil }

var _ stream.ResponseWriter = (*recorderWriter)(nil)

// Option configures a test context.
type Option func(*config)

type config struct {
	body    []byte
	headers [][2]string
	queries [][2]string
	params  [][2]string
}

// WithBody sets the request body.
func WithBody(body []byte) Option {
	return func(c *config) { c.body = body }
}

// WithHeader adds a request header.
func WithHeader(key, value string) Option {
	return func(c *config) { c.headers = append(c.headers, [2]string{key, value}) }
}

// WithQuery adds a query parameter.
func WithQuery(key, value string) Option {
	return func(c *config) { c.queries = append(c.queries, [2]string{key, value}) }
}

// WithParam adds a URL parameter (e.g. from :id in the route pattern).
func WithParam(key, value string) Option {
	return func(c *config) { c.params = append(c.params, [2]string{key, value}) }
}

// WithContentType is a shorthand for WithHeader("content-type", ct).
func WithContentType(ct string) Option {
	return WithHeader("content-type", ct)
}

// ReleaseContext returns a [celeris.Context] to the pool. The context must not
// be used after this call.
func ReleaseContext(ctx *celeris.Context) {
	ctxkit.ReleaseContext(ctx)
}

// NewContextT is like [NewContext] but registers an automatic cleanup with
// t.Cleanup so callers do not need to defer [ReleaseContext] manually.
func NewContextT(t *testing.T, method, path string, opts ...Option) (*celeris.Context, *ResponseRecorder) {
	t.Helper()
	ctx, rec := NewContext(method, path, opts...)
	t.Cleanup(func() { ReleaseContext(ctx) })
	return ctx, rec
}

// NewContext creates a [celeris.Context] and [ResponseRecorder] for testing.
// The returned context has the given method and path, plus any options applied.
// Call [ReleaseContext] when done to clean up pooled resources.
func NewContext(method, path string, opts ...Option) (*celeris.Context, *ResponseRecorder) {
	cfg := &config{}
	for _, o := range opts {
		o(cfg)
	}

	fullPath := path
	if len(cfg.queries) > 0 {
		q := url.Values{}
		for _, kv := range cfg.queries {
			q.Add(kv[0], kv[1])
		}
		fullPath = path + "?" + q.Encode()
	}

	s := stream.NewStream(1)
	s.Headers = [][2]string{
		{":method", method},
		{":path", fullPath},
		{":scheme", "http"},
		{":authority", "localhost"},
	}
	s.Headers = append(s.Headers, cfg.headers...)
	if len(cfg.body) > 0 {
		s.Data.Write(cfg.body)
	}

	rec := &ResponseRecorder{}
	s.ResponseWriter = &recorderWriter{rec: rec}

	ctx := ctxkit.NewContext(s).(*celeris.Context)
	for _, p := range cfg.params {
		ctxkit.AddParam(ctx, p[0], p[1])
	}
	return ctx, rec
}
