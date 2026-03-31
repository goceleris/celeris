package std

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/goceleris/celeris/protocol/h2/stream"

	"golang.org/x/net/http2"
)

// Bridge implements http.Handler, bridging net/http requests to stream.Handler.
type Bridge struct {
	engine  *Engine
	handler stream.Handler
}

// ServeHTTP converts an http.Request to a stream.Stream, calls the handler, and writes the response.
func (b *Bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.engine.metrics.reqCount.Add(1)

	s := stream.NewH1Stream(1)
	defer s.Release()

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}

	hdrs := make([][2]string, 0, len(r.Header)+4)
	hdrs = append(hdrs,
		[2]string{":method", r.Method},
		[2]string{":path", r.RequestURI},
		[2]string{":scheme", scheme},
		[2]string{":authority", r.Host},
	)

	for name, values := range r.Header {
		lowerName := strings.ToLower(name)
		for _, v := range values {
			hdrs = append(hdrs, [2]string{lowerName, v})
		}
	}
	s.Headers = hdrs

	if r.Body != nil && r.Body != http.NoBody {
		maxBodySize := b.engine.cfg.MaxRequestBodySize // 0 = unlimited (after WithDefaults)
		var body []byte
		var err error
		if maxBodySize > 0 {
			body, err = io.ReadAll(io.LimitReader(r.Body, maxBodySize+1))
		} else {
			body, err = io.ReadAll(r.Body)
		}
		_ = r.Body.Close()
		if err != nil {
			b.engine.metrics.errCount.Add(1)
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}
		if maxBodySize > 0 && int64(len(body)) > maxBodySize {
			b.engine.metrics.errCount.Add(1)
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		if len(body) > 0 {
			_, _ = s.GetBuf().Write(body)
		}
	}

	s.RemoteAddr = r.RemoteAddr
	s.EndStream = true
	s.SetState(stream.StateHalfClosedRemote)

	rw := &stdResponseWriter{w: w}
	s.ResponseWriter = rw

	if err := b.handler.HandleStream(r.Context(), s); err != nil {
		b.engine.metrics.errCount.Add(1)
		if !rw.flushed {
			http.Error(w, "handler error", http.StatusInternalServerError)
		}
		return
	}

	if !rw.flushed {
		w.WriteHeader(http.StatusOK)
	}
}

// stdResponseWriter adapts http.ResponseWriter to stream.ResponseWriter.
type stdResponseWriter struct {
	w       http.ResponseWriter
	flushed bool
}

func (rw *stdResponseWriter) WriteResponse(_ *stream.Stream, status int, headers [][2]string, body []byte) error {
	h := rw.w.Header()
	for _, hdr := range headers {
		h.Add(hdr[0], hdr[1])
	}

	if len(body) > 0 && h.Get("Content-Length") == "" {
		h.Set("Content-Length", strconv.Itoa(len(body)))
	}

	rw.w.WriteHeader(status)
	rw.flushed = true

	if len(body) > 0 {
		if _, err := rw.w.Write(body); err != nil {
			return fmt.Errorf("write body: %w", err)
		}
	}

	if f, ok := rw.w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func (rw *stdResponseWriter) SendGoAway(_ uint32, _ http2.ErrCode, _ []byte) error {
	return nil
}

func (rw *stdResponseWriter) MarkStreamClosed(_ uint32) {}

func (rw *stdResponseWriter) IsStreamClosed(_ uint32) bool {
	return false
}

func (rw *stdResponseWriter) WriteRSTStreamPriority(_ uint32, _ http2.ErrCode) error {
	return nil
}

func (rw *stdResponseWriter) CloseConn() error {
	return nil
}

// Streaming support — stdResponseWriter implements stream.Streamer.

func (rw *stdResponseWriter) WriteHeader(_ *stream.Stream, status int, headers [][2]string) error {
	h := rw.w.Header()
	for _, hdr := range headers {
		h.Add(hdr[0], hdr[1])
	}
	rw.w.WriteHeader(status)
	rw.flushed = true
	return nil
}

func (rw *stdResponseWriter) Write(_ *stream.Stream, data []byte) error {
	_, err := rw.w.Write(data)
	return err
}

func (rw *stdResponseWriter) Flush(_ *stream.Stream) error {
	if f, ok := rw.w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func (rw *stdResponseWriter) Close(_ *stream.Stream) error {
	return nil
}

func (rw *stdResponseWriter) Hijack(_ *stream.Stream) (net.Conn, error) {
	h, ok := rw.w.(http.Hijacker)
	if !ok {
		return nil, stream.ErrHijackNotSupported
	}
	conn, _, err := h.Hijack()
	if err == nil {
		rw.flushed = true
	}
	return conn, err
}

var _ stream.Hijacker = (*stdResponseWriter)(nil)
var _ stream.Streamer = (*stdResponseWriter)(nil)
