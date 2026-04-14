package adapters

import (
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"

	"github.com/goceleris/celeris"
)

const maxCaptureBytes = 100 << 20 // 100MB, matching core bridge

var errCaptureResponseTooLarge = errors.New("adapters: response body exceeds 100MB limit")

// WrapMiddleware adapts a standard net/http middleware (func(http.Handler) http.Handler)
// into a celeris HandlerFunc. The adapted middleware receives a reconstructed
// *http.Request and a responseCapture that records headers, status, and body.
//
// When the stdlib middleware calls the inner handler (next.ServeHTTP), the
// celeris handler chain continues via c.Next(). Any headers the stdlib
// middleware set on the ResponseWriter before calling next are propagated
// to the celeris response.
//
// When the stdlib middleware does NOT call the inner handler (e.g., it returns
// an early 403), the captured response is written back via c.Blob.
func WrapMiddleware(mw func(http.Handler) http.Handler) celeris.HandlerFunc {
	if mw == nil {
		panic("adapters: WrapMiddleware argument must not be nil")
	}
	return func(c *celeris.Context) error {
		rec := acquireCapture()
		defer releaseCapture(rec)

		var nextCalled bool
		var nextErr error
		var nextPanic any

		inner := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			nextCalled = true
			// Propagate headers set by the stdlib middleware before it called inner.
			for key, values := range rec.header {
				lk := strings.ToLower(key)
				for _, v := range values {
					c.AddHeader(lk, v)
				}
			}
			// Catch panics from the celeris chain so the stdlib middleware's
			// own recover() (if any) can't swallow them. Re-panic AFTER
			// wrapped.ServeHTTP returns so celeris recovery middleware sees
			// the original value.
			defer func() {
				if r := recover(); r != nil {
					nextPanic = r
				}
			}()
			nextErr = c.Next()
		})

		wrapped := mw(inner)

		req := buildRequest(c)
		wrapped.ServeHTTP(rec, req)

		if nextPanic != nil {
			panic(nextPanic)
		}

		if nextCalled {
			return nextErr
		}

		// Stdlib middleware short-circuited (did not call inner).
		// Abort the celeris chain and write the captured response.
		c.Abort()

		contentType := ""
		for key, values := range rec.header {
			lk := strings.ToLower(key)
			if lk == "content-type" {
				if len(values) > 0 {
					contentType = values[0]
				}
				continue
			}
			for _, v := range values {
				c.AddHeader(lk, v)
			}
		}

		code := rec.code
		if code == 0 {
			code = http.StatusOK
		}
		return c.Blob(code, contentType, rec.body.Bytes())
	}
}

// buildRequest reconstructs an *http.Request from a celeris Context for use
// with stdlib middleware/handlers.
func buildRequest(c *celeris.Context) *http.Request {
	path := c.Path()
	if q := c.RawQuery(); q != "" {
		path += "?" + q
	}

	var body io.Reader
	data := c.Body()
	if len(data) > 0 {
		body = bytes.NewReader(data)
	}

	req, _ := http.NewRequestWithContext(c.Context(), c.Method(), path, body)

	for _, h := range c.RequestHeaders() {
		if strings.HasPrefix(h[0], ":") {
			continue
		}
		req.Header.Add(h[0], h[1])
	}

	if host := c.Host(); host != "" {
		req.Host = host
	}

	req.RemoteAddr = c.RemoteAddr()

	if cl := c.ContentLength(); cl >= 0 {
		req.ContentLength = cl
	}

	if c.Scheme() == "https" {
		req.TLS = &tls.ConnectionState{}
	}

	if c.Protocol() == "2" {
		req.Proto = "HTTP/2.0"
		req.ProtoMajor = 2
		req.ProtoMinor = 0
	} else {
		req.Proto = "HTTP/1.1"
		req.ProtoMajor = 1
		req.ProtoMinor = 1
	}

	return req
}

// responseCapture implements http.ResponseWriter, capturing headers, status
// code, and body for inspection after a stdlib handler runs.
type responseCapture struct {
	header http.Header
	body   bytes.Buffer
	code   int
}

func (w *responseCapture) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *responseCapture) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	if w.body.Len()+len(b) > maxCaptureBytes {
		return 0, errCaptureResponseTooLarge
	}
	return w.body.Write(b)
}

func (w *responseCapture) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
}

var capturePool = sync.Pool{New: func() any {
	return &responseCapture{header: make(http.Header)}
}}

func acquireCapture() *responseCapture {
	return capturePool.Get().(*responseCapture)
}

func releaseCapture(rec *responseCapture) {
	for k := range rec.header {
		delete(rec.header, k)
	}
	rec.body.Reset()
	rec.code = 0
	capturePool.Put(rec)
}

// ReverseProxy creates a reverse proxy handler that forwards requests
// to the given target URL. Uses httputil.ReverseProxy under the hood,
// wrapped via celeris.Adapt for integration with the celeris handler chain.
//
// The proxy sets X-Forwarded-For, X-Forwarded-Host, and X-Forwarded-Proto
// headers automatically via httputil.ProxyRequest.SetXForwarded.
//
// Panics if target is nil.
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
	if cfg.modifyResponse != nil {
		proxy.ModifyResponse = cfg.modifyResponse
	}
	if cfg.errorHandler != nil {
		proxy.ErrorHandler = cfg.errorHandler
	}
	return celeris.Adapt(proxy)
}
