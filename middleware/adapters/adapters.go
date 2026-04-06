package adapters

import (
	"bytes"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/goceleris/celeris"
)

// WrapMiddleware converts a standard net/http middleware into a
// [celeris.HandlerFunc]. The stdlib middleware signature
// func(http.Handler) http.Handler is the convention used by rs/cors,
// gorilla/csrf, chi/middleware, and most Go ecosystem middleware.
//
// If the stdlib middleware calls its inner handler, [celeris.Context.Next]
// runs the remaining celeris chain. If the stdlib middleware
// short-circuits (returns without calling inner), the captured response
// is written to the celeris context.
func WrapMiddleware(mw func(http.Handler) http.Handler) celeris.HandlerFunc {
	return func(c *celeris.Context) error {
		var nextCalled bool
		var nextErr error

		inner := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			nextCalled = true
			nextErr = c.Next()
		})

		wrapped := mw(inner)

		req := buildRequest(c)
		rec := &responseCapture{header: make(http.Header)}
		wrapped.ServeHTTP(rec, req)

		if nextCalled {
			return nextErr
		}

		// The stdlib middleware short-circuited. Abort the celeris
		// chain so no downstream handlers run after we return.
		c.Abort()
		for key, values := range rec.header {
			lk := strings.ToLower(key)
			for _, v := range values {
				c.AddHeader(lk, v)
			}
		}
		code := rec.code
		if code == 0 {
			code = http.StatusOK
		}
		if len(rec.body) > 0 {
			ct := rec.header.Get("Content-Type")
			return c.Blob(code, ct, rec.body)
		}
		return c.NoContent(code)
	}
}

// ToStdlib converts a [celeris.HandlerFunc] to an [http.Handler].
// It delegates to [celeris.ToHandler].
func ToStdlib(h celeris.HandlerFunc) http.Handler {
	return celeris.ToHandler(h)
}

// ReverseProxy creates a [celeris.HandlerFunc] that proxies requests to
// target using [httputil.ReverseProxy]. The target must not be nil.
func ReverseProxy(target *url.URL, opts ...Option) celeris.HandlerFunc {
	if target == nil {
		panic("adapters: ReverseProxy target must not be nil")
	}
	cfg := &proxyConfig{}
	for _, o := range opts {
		o(cfg)
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.SetXForwarded()
			if cfg.modifyRequest != nil {
				cfg.modifyRequest(r.Out)
			}
		},
	}
	if cfg.transport != nil {
		proxy.Transport = cfg.transport
	}
	if cfg.errorHandler != nil {
		proxy.ErrorHandler = cfg.errorHandler
	}
	return celeris.Adapt(proxy)
}

// buildRequest reconstructs an *http.Request from a celeris context.
func buildRequest(c *celeris.Context) *http.Request {
	path := c.Path()
	if q := c.RawQuery(); q != "" {
		path += "?" + q
	}

	var body *bytes.Reader
	data := c.Body()
	if len(data) > 0 {
		body = bytes.NewReader(data)
	}

	var req *http.Request
	if body != nil {
		req, _ = http.NewRequestWithContext(c.Context(), c.Method(), path, body)
	} else {
		req, _ = http.NewRequestWithContext(c.Context(), c.Method(), path, nil)
	}
	req.Host = c.Host()
	for _, h := range c.RequestHeaders() {
		if len(h[0]) > 0 && h[0][0] == ':' {
			continue
		}
		req.Header.Add(h[0], h[1])
	}
	req.RemoteAddr = c.RemoteAddr()
	return req
}

// responseCapture is a minimal http.ResponseWriter that buffers the
// response for inspection.
type responseCapture struct {
	header http.Header
	code   int
	body   []byte
}

func (r *responseCapture) Header() http.Header         { return r.header }
func (r *responseCapture) WriteHeader(code int)         { r.code = code }
func (r *responseCapture) Write(b []byte) (int, error)  { r.body = append(r.body, b...); return len(b), nil }
