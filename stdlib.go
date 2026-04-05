package celeris

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/goceleris/celeris/protocol/h2/stream"


)

const maxToHandlerBodySize = maxBodySize

// ToHandler wraps a celeris HandlerFunc as an http.Handler for use with
// net/http routers, middleware, or test infrastructure. The returned handler
// converts the *http.Request into a stream.Stream, invokes the celeris
// handler, and writes the response back via http.ResponseWriter.
//
// This is the reverse of [Adapt] / [AdaptFunc] which wrap net/http handlers
// for use inside celeris.
func ToHandler(h HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := stream.NewStream(1)
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
			body, err := io.ReadAll(io.LimitReader(r.Body, int64(maxToHandlerBodySize)+1))
			_ = r.Body.Close()
			if err != nil {
				http.Error(w, "failed to read body", http.StatusBadRequest)
				return
			}
			if len(body) > maxToHandlerBodySize {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			if len(body) > 0 {
				_, _ = s.GetBuf().Write(body)
			}
		}

		s.RemoteAddr = r.RemoteAddr
		s.SetProtoMajor(uint8(r.ProtoMajor))
		s.EndStream = true
		s.SetState(stream.StateHalfClosedRemote)

		rw := &toStdlibResponseWriter{w: w}
		s.ResponseWriter = rw

		c := acquireContext(s)
		c.startTime = time.Now()
		defer func() {
			if rv := recover(); rv != nil {
				if !rw.flushed {
					c.statusCode = 500
					w.WriteHeader(500)
					_, _ = w.Write([]byte("Internal Server Error"))
				}
			}
			releaseContext(c)
		}()

		c.handlers = []HandlerFunc{h}
		if err := c.Next(); err != nil {
			if !rw.flushed {
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}
	})
}

// toStdlibResponseWriter adapts http.ResponseWriter to stream.ResponseWriter
// for use in ToHandler.
type toStdlibResponseWriter struct {
	w       http.ResponseWriter
	flushed bool
}

func (rw *toStdlibResponseWriter) WriteResponse(_ *stream.Stream, status int, headers [][2]string, body []byte) error {
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
		_, _ = rw.w.Write(body)
	}
	if f, ok := rw.w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

