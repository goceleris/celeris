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

func buildHTTPRequest(c *Context) (*http.Request, error) {
	url := c.path
	if c.rawQuery != "" {
		url += "?" + c.rawQuery
	}

	var body io.Reader
	data := c.Body()
	if len(data) > 0 {
		body = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(c.Context(), c.method, url, body)
	if err != nil {
		return nil, err
	}

	for _, h := range c.stream.Headers {
		if strings.HasPrefix(h[0], ":") {
			continue
		}
		req.Header.Add(h[0], h[1])
	}

	if host := c.Header(":authority"); host != "" {
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
