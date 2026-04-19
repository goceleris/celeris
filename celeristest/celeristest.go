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
	"encoding/base64"
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/protocol/h2/stream"
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

// recorderCombo bundles a ResponseRecorder and its recorderWriter in a
// single allocation so they can be pooled together.
type recorderCombo struct {
	rec ResponseRecorder
	rw  recorderWriter
}

var recorderPool = sync.Pool{New: func() any {
	combo := &recorderCombo{}
	combo.rw.rec = &combo.rec
	combo.rw.combo = combo
	return combo
}}

// recorderWriter adapts a ResponseRecorder to the internal
// stream.ResponseWriter interface without leaking internal types
// in the public API.
type recorderWriter struct {
	rec   *ResponseRecorder
	combo *recorderCombo
}

func (w *recorderWriter) WriteResponse(_ *stream.Stream, status int, headers [][2]string, body []byte) error {
	w.rec.StatusCode = status
	w.rec.Headers = headers
	w.rec.Body = append(w.rec.Body[:0], body...)
	return nil
}

var _ stream.ResponseWriter = (*recorderWriter)(nil)

// Option configures a test context.
type Option func(*config)

type config struct {
	body           []byte
	headers        [][2]string
	queries        [][2]string
	params         [][2]string
	cookies        [][2]string
	remoteAddr     string
	handlers       []any
	fullPath       string
	protocol       string
	scheme         string
	trustedProxies []string
	headersBuf     [4][2]string
	handlersBuf    [4]any
}

var configPool = sync.Pool{New: func() any {
	c := &config{}
	c.headers = c.headersBuf[:0]
	c.handlers = c.handlersBuf[:0]
	return c
}}

func (c *config) reset() {
	c.body = nil
	for i := range c.headers {
		c.headers[i] = [2]string{}
	}
	c.headers = c.headersBuf[:0]
	c.queries = nil
	c.params = nil
	c.cookies = nil
	c.remoteAddr = ""
	c.fullPath = ""
	c.protocol = ""
	c.scheme = ""
	c.trustedProxies = nil
	for i := range c.handlers {
		c.handlers[i] = nil
	}
	c.handlers = c.handlersBuf[:0]
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

// WithBasicAuth sets the Authorization header for HTTP Basic auth.
func WithBasicAuth(user, pass string) Option {
	encoded := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	return WithHeader("authorization", "Basic "+encoded)
}

// WithCookie adds a cookie to the request. Name and value are joined as
// "name=value" and concatenated with "; " between cookies, matching the
// Cookie header wire format. This helper does not escape semicolons or
// CR/LF inside value — tests should pass well-formed values only. If a
// test needs to exercise the server's handling of malformed cookie
// headers, set the raw "cookie" header via [WithHeader] instead.
func WithCookie(name, value string) Option {
	return func(c *config) {
		c.cookies = append(c.cookies, [2]string{name, value})
	}
}

// WithRemoteAddr sets the remote address on the test stream.
func WithRemoteAddr(addr string) Option {
	return func(c *config) { c.remoteAddr = addr }
}

// WithFullPath sets the route pattern (e.g., "/users/:id") on the test context.
func WithFullPath(path string) Option {
	return func(c *config) { c.fullPath = path }
}

// WithProtocol sets the HTTP protocol version. Use "1.1" for HTTP/1.1 or "2" for HTTP/2.
func WithProtocol(version string) Option {
	return func(c *config) { c.protocol = version }
}

// WithScheme sets the request scheme override (e.g. "https"). This simulates
// what the proxy middleware does via SetScheme, without requiring the raw
// X-Forwarded-Proto header (which Scheme() no longer trusts directly).
func WithScheme(scheme string) Option {
	return func(c *config) { c.scheme = scheme }
}

// WithTrustedProxies sets CIDR ranges for trusted proxy ClientIP resolution.
func WithTrustedProxies(cidrs ...string) Option {
	return func(c *config) { c.trustedProxies = cidrs }
}

// WithHandlers sets the handler chain on the test context. This enables
// middleware chain testing where mw1 calls c.Next() → mw2 runs → ... → final handler.
// Pass celeris.HandlerFunc values; they are stored as []any to avoid import cycles.
func WithHandlers(handlers ...celeris.HandlerFunc) Option {
	return func(c *config) {
		n := len(handlers)
		if n <= len(c.handlersBuf) {
			for i, h := range handlers {
				c.handlersBuf[i] = h
			}
			c.handlers = c.handlersBuf[:n]
		} else {
			c.handlers = make([]any, n)
			for i, h := range handlers {
				c.handlers[i] = h
			}
		}
	}
}

// ReleaseContext returns a [celeris.Context] to the pool. The context must not
// be used after this call.
func ReleaseContext(ctx *celeris.Context) {
	// Extract stream and recorder before releasing context (reset nils them).
	s := celeris.TestStream(ctx)
	var combo *recorderCombo
	if s != nil {
		if w, ok := s.ResponseWriter.(*recorderWriter); ok && w.combo != nil {
			combo = w.combo
		}
	}

	celeris.ReleaseTestContext(ctx)

	// Return stream to pool so NewStream reuses it on the next call.
	if s != nil {
		if s.HasDoneCh() {
			// A derived context (e.g. context.WithTimeout) spawned a
			// goroutine that holds a reference to this stream. Cancel to
			// terminate it, but don't pool the stream — the goroutine may
			// still read Err()/Done() after the pool recycles the struct.
			s.Cancel()
		} else {
			stream.ResetForPool(s)
		}
	}
	if combo != nil {
		combo.rec.StatusCode = 0
		combo.rec.Headers = nil
		combo.rec.Body = combo.rec.Body[:0]
		recorderPool.Put(combo)
	}
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
	cfg := configPool.Get().(*config)
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
	s.Headers = append(s.Headers,
		[2]string{":method", method},
		[2]string{":path", fullPath},
		[2]string{":scheme", "http"},
		[2]string{":authority", "localhost"},
	)
	s.Headers = append(s.Headers, cfg.headers...)
	if len(cfg.cookies) > 0 {
		parts := make([]string, 0, len(cfg.cookies))
		for _, kv := range cfg.cookies {
			parts = append(parts, kv[0]+"="+kv[1])
		}
		s.Headers = append(s.Headers, [2]string{"cookie", strings.Join(parts, "; ")})
	}
	if len(cfg.body) > 0 {
		s.GetBuf().Write(cfg.body)
	}

	combo := recorderPool.Get().(*recorderCombo)
	combo.rec.StatusCode = 0
	combo.rec.Headers = nil
	combo.rec.Body = combo.rec.Body[:0]
	rec := &combo.rec
	s.ResponseWriter = &combo.rw

	if cfg.remoteAddr != "" {
		s.RemoteAddr = cfg.remoteAddr
	}

	if cfg.protocol != "" {
		switch cfg.protocol {
		case "1.1":
			s.SetProtoMajor(1)
		case "2":
			s.SetProtoMajor(2)
		}
	}

	ctx := celeris.AcquireTestContext(s)
	celeris.SetTestStartTime(ctx, time.Now())
	for _, p := range cfg.params {
		celeris.AddTestParam(ctx, p[0], p[1])
	}
	if len(cfg.handlers) > 0 {
		chain := make([]celeris.HandlerFunc, len(cfg.handlers))
		for i, h := range cfg.handlers {
			chain[i] = h.(celeris.HandlerFunc)
		}
		celeris.SetTestHandlers(ctx, chain)
	}
	if cfg.fullPath != "" {
		celeris.SetTestFullPath(ctx, cfg.fullPath)
	}
	if cfg.scheme != "" {
		celeris.SetTestScheme(ctx, cfg.scheme)
	}
	if len(cfg.trustedProxies) > 0 {
		nets := make([]*net.IPNet, 0, len(cfg.trustedProxies))
		for _, cidr := range cfg.trustedProxies {
			_, ipNet, err := net.ParseCIDR(cidr)
			if err != nil {
				ip := net.ParseIP(cidr)
				if ip == nil {
					panic("celeristest: invalid trusted proxy: " + cidr)
				}
				if ip4 := ip.To4(); ip4 != nil {
					_, ipNet, _ = net.ParseCIDR(ip4.String() + "/32")
				} else {
					_, ipNet, _ = net.ParseCIDR(ip.String() + "/128")
				}
			}
			nets = append(nets, ipNet)
		}
		celeris.SetTestTrustedNets(ctx, nets)
	}

	cfg.reset()
	configPool.Put(cfg)

	return ctx, rec
}
