package celeris

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// Adapt wraps a standard net/http Handler so it can be used as a celeris
// HandlerFunc. The adapted handler receives a reconstructed *http.Request
// with headers, body, and context from the celeris Context. Response body
// is buffered in memory, capped at 100MB.
func Adapt(h http.Handler) HandlerFunc {
	return func(c *Context) error {
		rw := &bridgeResponseWriter{}

		req, err := buildHTTPRequest(c)
		if err != nil {
			return c.AbortWithStatus(http.StatusInternalServerError)
		}

		h.ServeHTTP(rw, req)

		contentType := ""
		for key, values := range rw.header {
			lk := strings.ToLower(key)
			if lk == "content-type" {
				if len(values) > 0 {
					contentType = values[0]
				}
				continue // skip — Blob sets content-type from the parameter
			}
			for _, v := range values {
				c.AddHeader(lk, v)
			}
		}

		code := rw.code
		if code == 0 {
			code = http.StatusOK
		}
		return c.Blob(code, contentType, rw.body.Bytes())
	}
}

// AdaptFunc wraps a standard net/http handler function. It is a convenience
// wrapper equivalent to Adapt(http.HandlerFunc(h)).
func AdaptFunc(h http.HandlerFunc) HandlerFunc {
	return Adapt(h)
}

// buildHTTPRequest builds the *http.Request an adapted handler receives.
//
// Every string it hands to net/http is a copy. On epoll and io_uring the
// method, path, query and header strings are views of the connection's
// receive buffer, which the engine reuses for the connection's next request
// and, once the connection closes, for another connection. net/http lets a
// handler keep the request's strings after ServeHTTP returns (a log line
// queued for later, a map key, a value handed to a goroutine), so a kept view
// would read other request bytes (celeris#732). The copies share one
// allocation. The body is not copied: net/http forbids reading it after
// ServeHTTP returns.
func buildHTTPRequest(c *Context) (*http.Request, error) {
	// The H1 parser of the native engines defers the header slice until
	// something reads a header. Without this, a route with no header read
	// before Adapt handed net/http no request headers at all (celeris#720).
	c.stream.MaterializeHeaders()
	hdrs := c.stream.Headers

	urlLen := len(c.path)
	if c.rawQuery != "" {
		urlLen += 1 + len(c.rawQuery)
	}
	authority := ""
	n := len(c.method) + urlLen
	for _, h := range hdrs {
		if strings.HasPrefix(h[0], ":") {
			if h[0] == ":authority" && authority == "" {
				authority = h[1]
			}
			continue
		}
		n += len(h[0]) + len(h[1])
	}
	n += len(authority)

	var sb strings.Builder
	sb.Grow(n)
	sb.WriteString(c.method)
	sb.WriteString(c.path)
	if c.rawQuery != "" {
		sb.WriteByte('?')
		sb.WriteString(c.rawQuery)
	}
	for _, h := range hdrs {
		if !strings.HasPrefix(h[0], ":") {
			sb.WriteString(h[0])
			sb.WriteString(h[1])
		}
	}
	sb.WriteString(authority)
	// Cut the copies back out, in the order they were written.
	rest := sb.String()
	next := func(l int) string {
		s := rest[:l]
		rest = rest[l:]
		return s
	}

	var body io.Reader
	data := c.Body()
	if len(data) > 0 {
		body = bytes.NewReader(data)
	}

	method := next(len(c.method))
	req, err := http.NewRequestWithContext(c.Context(), method, next(urlLen), body)
	if err != nil {
		return nil, err
	}

	for _, h := range hdrs {
		if strings.HasPrefix(h[0], ":") {
			continue
		}
		key := next(len(h[0]))
		req.Header.Add(key, next(len(h[1])))
	}

	if host := next(len(authority)); host != "" {
		req.Host = host
	}

	if cl := c.Header("content-length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil && n >= 0 {
			req.ContentLength = n
		}
	}

	return req, nil
}

const maxBridgeResponseBytes = maxBodySize

var errBridgeResponseTooLarge = errors.New("bridge: response body exceeds 100MB limit")

type bridgeResponseWriter struct {
	header http.Header
	body   bytes.Buffer
	code   int
}

func (w *bridgeResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *bridgeResponseWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	if int64(w.body.Len())+int64(len(b)) > maxBridgeResponseBytes {
		return 0, errBridgeResponseTooLarge
	}
	return w.body.Write(b)
}

func (w *bridgeResponseWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
}
