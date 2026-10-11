package std

import (
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// Bridge implements http.Handler, bridging net/http requests to stream.Handler.
type Bridge struct {
	engine  *Engine
	handler stream.Handler
}

// ServeHTTP converts an http.Request to a stream.Stream, calls the handler, and writes the response.
func (b *Bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.ProtoMajor == 2 {
		// An h2c stream: the drain waits for it (celeris#759), unless the
		// drain has ended, when nothing new may start (celeris#878).
		if !b.engine.enterH2Stream() {
			// Only a connection that handed itself over after the drain's
			// GOAWAY went out gets here. It is sent GOAWAY when this stream
			// ends: Connection: close makes the HTTP/2 server do it with
			// this response, on every build (net/http's server would also
			// do it once the connection is idle, x/net's legacy one reads
			// no such state from the server).
			w.Header().Set("Connection", "close")
			http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
			return
		}
		defer b.engine.h2Streams.Add(-1)
		if b.engine.draining.Load() {
			// A connection that handed itself over after the drain began
			// missed the GOAWAY Shutdown sent at its start, and this is a
			// stream it opened after it. Connection: close makes the HTTP/2
			// server send the connection GOAWAY with this response, where
			// otherwise it would wait for the connection to go idle, which a
			// connection with a long stream open does not. The response is
			// still delivered.
			w.Header().Set("Connection", "close")
		}
	}
	// Counted here, past the h2 gate: an h2c request refused after the drain
	// is not one the engine served and is not in RequestCount.
	b.engine.metrics.reqCount.Add(1)

	// An h2c request gets an HTTP/2 stream, whose context ends when the
	// client resets the stream or goes away, as it does on the native
	// engines (celeris#949). An HTTP/1 request keeps the HTTP/1 stream, whose
	// context is context.Background() on every engine: no engine cancels it
	// when an HTTP/1 client leaves (SSE and WebSocket handlers are told
	// through OnWSDetachClose, below).
	var s *stream.Stream
	if r.ProtoMajor == 2 {
		s = stream.NewStream(1)
		defer s.Release()
		// Deferred after Release, so it runs before it: a cancel must not
		// reach the stream once it is back in the pool.
		defer BindStreamCancel(r.Context(), s)()
	} else {
		s = stream.NewH1Stream(1)
		defer s.Release()
	}

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
			// Prevent int64 overflow: maxBodySize+1 wraps for MaxInt64.
			limit := maxBodySize
			if limit < math.MaxInt64 {
				limit++
			}
			body, err = io.ReadAll(io.LimitReader(r.Body, limit))
		} else {
			body, err = io.ReadAll(r.Body)
		}
		_ = r.Body.Close()
		if err != nil {
			b.engine.metrics.errs.RequestBody.Add(1)
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}
		if maxBodySize > 0 && int64(len(body)) > maxBodySize {
			b.engine.metrics.errs.RequestBody.Add(1)
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		if len(body) > 0 {
			// body is a freshly-allocated slice from io.ReadAll; install
			// as zero-copy override so GetData avoids the Buffer.Write
			// memcpy (critical for large-body POSTs).
			s.SetRawBody(body)
		}
	}

	s.RemoteAddr = r.RemoteAddr
	s.SetProtoMajor(uint8(r.ProtoMajor))
	s.EndStream = true
	s.SetState(stream.StateHalfClosedRemote)

	rw := &stdResponseWriter{w: w, eng: b.engine}
	// A stream that detached (StreamDetached) hands back its deadlines and its
	// poke when the request is over, whichever way: a panic included.
	defer rw.streamEnded()
	s.ResponseWriter = rw

	// celeris#494 parity: the native engines tell detached-stream
	// middleware (SSE, WebSocket) that the peer went away through
	// H1State.OnDetachClose. On std the equivalent signal is r.Context(),
	// which net/http cancels when the client connection closes (and at the
	// latest when ServeHTTP returns). Expose it through the same
	// SetWSDetachClose hook so an SSE handler that only watches
	// client.Context() — heartbeat disabled, no Send to fail — is woken on
	// std too. OnDetach stays nil (EngineSupportsAsyncDetach() stays false,
	// SSE keeps running inline here) and OnWSSetError stays nil. The
	// AfterFunc registration is dropped when rctx ends, so nothing
	// outlives the request; s is pooled and Release() nils the hook.
	rctx := r.Context()
	s.OnWSDetachClose = func(fn func()) { context.AfterFunc(rctx, fn) }

	if err := b.handler.HandleStream(rctx, s); err != nil {
		b.engine.metrics.errs.Handler.Add(1)
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
	w http.ResponseWriter
	// eng is the engine whose request this is: the write timeout and the base
	// context a detached stream is bounded by (StreamDetached).
	eng *Engine
	// det is nil until the handler detaches.
	det     *detachedWrites
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
	if d := rw.det; d != nil {
		d.arm()
		_, err := rw.w.Write(data)
		d.disarm()
		return err
	}
	_, err := rw.w.Write(data)
	return err
}

func (rw *stdResponseWriter) Flush(_ *stream.Stream) error {
	if f, ok := rw.w.(http.Flusher); ok {
		if d := rw.det; d != nil {
			d.arm()
			f.Flush()
			d.disarm()
			return nil
		}
		f.Flush()
	}
	return nil
}

func (rw *stdResponseWriter) Close(_ *stream.Stream) error {
	return nil
}

// StreamDetached implements stream.DetachNotifier. net/http arms a read
// deadline (ReadTimeout) and a write deadline (WriteTimeout) for each request
// and keeps the write one armed while the handler runs: a response that takes
// longer than WriteTimeout fails its next write, so an SSE stream was cut at
// WriteTimeout while the native engines, which apply neither timeout to a
// detached connection, kept it open (celeris#855). The handler has detached,
// so this stream is the connection's: clear both, for this request only.
// net/http arms them again for the next request on the connection, so
// nothing is restored.
//
// Nothing then ends a stream whose client stays connected and reads nothing,
// so the stream's writes are bounded the way the WebSocket middleware bounds
// its own: each write gets WriteTimeout from its start and no deadline when it
// has returned, and when the drain budget runs out the writes fail (see
// detachedWrites). Runs when a stream starts, never per request.
//
// The read deadline is cleared as well. net/http already clears it once it
// starts watching the connection for the client going away (before the
// handler runs, for the bodies the bridge reads whole), so on HTTP/1 this
// is for a Go that does not; on h2 it stops the request-body timer. The
// errors are ignored: a writer that cannot set deadlines
// (http.ErrNotSupported) has none armed to clear.
func (rw *stdResponseWriter) StreamDetached() {
	if rw.det != nil {
		return
	}
	rc := http.NewResponseController(rw.w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	d := &detachedWrites{rc: rc}
	if e := rw.eng; e != nil {
		// http.Server's own, which stdTimeout has made negative for "none".
		if e.server != nil && e.server.WriteTimeout > 0 {
			d.wt = e.server.WriteTimeout
		}
		if e.baseCtx != nil {
			d.stopPoke = context.AfterFunc(e.baseCtx, d.poke)
		}
	}
	rw.det = d
}

// streamEnded is called as the request's ServeHTTP returns (see
// detachedWrites.end).
func (rw *stdResponseWriter) streamEnded() {
	if rw.det != nil {
		rw.det.end()
	}
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
var _ stream.DetachNotifier = (*stdResponseWriter)(nil)
