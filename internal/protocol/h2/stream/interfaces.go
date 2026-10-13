package stream

import (
	"context"
	"errors"
	"net"
	"os"

	"golang.org/x/net/http2"
)

// ErrHijackNotSupported is returned when the engine does not support
// connection takeover.
var ErrHijackNotSupported = errors.New("celeris: hijack not supported by this engine")

// Hijacker is implemented by ResponseWriters that support connection takeover.
// This enables protocols like WebSocket that require raw TCP access.
type Hijacker interface {
	Hijack(stream *Stream) (net.Conn, error)
}

// Handler interface for processing streams.
//
// For an HTTP/2 stream the H2 processor passes context.Background() as ctx.
// The stream's own context, cancelled with the stream, is stream.Context():
// it is made the first time a use of the stream asks for it (celeris#836),
// so a stream whose handler never asks costs no allocation.
//
// What a handler's end means on HTTP/2 (celeris#889). Like net/http, which
// ends a response when its handler returns, the processor settles the stream:
//   - a handler that returns having written its response whole (WriteResponse)
//     changes nothing;
//   - a handler that streamed its response (a StreamWriter) and returned
//     without closing it has the stream ended with END_STREAM when the
//     response is complete (the headers are out and, for a declared
//     content-length, exactly that many bytes were written; with none
//     declared it is complete whatever it wrote), and reset with
//     RST_STREAM(INTERNAL_ERROR) when its body is not as long as it declared
//     (short of it or over it, or the declaration is not a plain number), so a
//     truncated body never looks complete (RFC 9110 §8.6);
//   - a handler that returns an error having sent nothing, panics (a panic that
//     escapes the router, which answers a handler's panic with a 500 first) or
//     calls runtime.Goexit has its stream reset with RST_STREAM(INTERNAL_ERROR),
//     unless its response was already whole (written whole, or its writer
//     closed: END_STREAM is then sent, or queued behind DATA that waits for the
//     peer's window, and the stream stays until that is out).
//
// The RST_STREAM is queued behind the frames the stream already queued
// (Resetter). H2Config.Logger gets a Warn for a stream the processor resets
// after a return (a body not as long as its declared length, an error return, a
// response the router saw break off), a Debug for one it ends, and an Error with
// the stack for a panic and for a Goexit, which reset the stream with no Warn of
// their own; Processor.OnHandlerPanic, OnHandlerGoexit and OnHandlerUnfinished
// are told, and the processor counts them.
//
// A runtime.Goexit cannot be recovered (recover() returns nil for it), and
// the epoll and io_uring engines run a stream in one of two places, which
// decides what else it costs. A stream runs inline, on the engine's event-loop
// goroutine, unless one of the two cases below applies. Whether the request
// has a body does not matter: every frame path (HEADERS, trailers, the DATA
// frame that ends the stream, a header block completed by CONTINUATION frames)
// marks the request ended before it runs the handler, so a POST with a body is
// as inline as a GET. The checks are Processor.canRunInline (the request has
// ended; the route is not async) and, in Processor.runHandler, the outbound
// budget. A Goexit in an inline handler ends that loop for good, and the engine
// logs one Error, "... goroutine ended without returning" (celeris#848): the
// loop, with the connections it serves, is gone. The stream is reset and the
// Goexit is logged and counted by the processor as above, but the frame goes
// out only if something still drains the connection's write queue, which the
// engine's loop does not after it ended. A panic is recovered here, and the
// connection goes on; through the celeris router a panic is a 500 first.
//
// A stream runs on the h2 worker pool in these two cases only: its route is
// async, or its connection already holds OutboundBudget bytes of response
// DATA for the peer's window (celeris#893), which sends even a sync route to
// the pool. A Goexit there ends one pool goroutine, which the pool replaces
// (h2WorkerPool.run); the stream is reset, the Goexit logged, the connection
// is not closed and keeps serving its other streams, and the loop survives.
//
// On HTTP/1.1 the same Handler runs the request, and the epoll and io_uring
// engines apply the same split. A sync (inline) handler runs on the event-loop
// goroutine: a Goexit ends the loop and the engine logs the same Error
// (celeris#848), and a panic that escapes the handler is not recovered and
// ends the process (the celeris router answers a handler panic with a 500
// first). An async handler runs on its own goroutine: its Goexit or panic
// closes that connection and the engine logs an Error, and the loop keeps
// serving. So a sync handler must not call runtime.Goexit on either protocol.
type Handler interface {
	HandleStream(ctx context.Context, stream *Stream) error
}

// AsyncRouteResolver is an optional interface a Handler may implement to
// let the protocol layer learn, before invoking the handler, whether the
// route matching a given method+path is configured for async dispatch
// (per-route .Async()). The H2 processor uses it to decide per-stream
// whether to run the handler inline on the event loop (sync route) or
// dispatch it to the async worker pool (async route). Handlers that don't
// implement it fall back to the prior characteristic-based heuristic.
type AsyncRouteResolver interface {
	RouteAsync(method, path string) bool
	// HasAsyncRoutes reports whether the server has ANY async-marked
	// route. The processor uses it as a fast-out: when false it skips
	// per-stream route resolution entirely, so a pure-sync H2 server
	// pays zero extra cost on the inline hot path.
	HasAsyncRoutes() bool
}

// HandlerFunc is an adapter to use functions as stream handlers.
type HandlerFunc func(ctx context.Context, stream *Stream) error

// HandleStream calls the handler function.
func (f HandlerFunc) HandleStream(ctx context.Context, stream *Stream) error {
	return f(ctx, stream)
}

// FrameWriter is an interface for writing HTTP/2 frames.
type FrameWriter interface {
	WriteSettings(settings ...http2.Setting) error
	WriteSettingsAck() error
	WriteHeaders(streamID uint32, endStream bool, headerBlock []byte, maxFrameSize uint32) error
	WriteData(streamID uint32, endStream bool, data []byte) error
	WriteWindowUpdate(streamID uint32, increment uint32) error
	WriteRSTStream(streamID uint32, code http2.ErrCode) error
	WriteGoAway(lastStreamID uint32, code http2.ErrCode, debugData []byte) error
	WritePing(ack bool, data [8]byte) error
	WritePushPromise(streamID uint32, promiseID uint32, endHeaders bool, headerBlock []byte) error
}

// ResponseWriter is the minimal interface for writing HTTP responses.
// All engine adapters, test recorders, and stdlib bridges implement this.
type ResponseWriter interface {
	WriteResponse(stream *Stream, status int, headers [][2]string, body []byte) error
}

// FileResponder is an optional interface a ResponseWriter may implement
// to serve a regular file as the response body via a zero-copy path
// (sendfile(2) on the epoll engine). The celeris Context's file-serving
// methods (File, FileFromDir, FileFromFS, and middleware/static) type-
// assert the connection's ResponseWriter for it.
//
// WriteFileResponse emits the status line + headers (with a
// content-length of `length`) and then the slice [offset, offset+length)
// of file as the body. It reports handled=false when it declined to use
// the zero-copy path — e.g. the underlying engine is not sendfile-capable,
// the body is below the size threshold where a buffered write coalesces
// better, or the response is HTTP/2 (framed). On handled=false the caller
// MUST fall back to the buffered read+write path; the writer guarantees it
// emitted nothing in that case. On handled=true it owns the response
// (including the HEAD-request invariant: headers only, no body bytes for
// HEAD). The caller retains ownership of file and may close it as soon as
// WriteFileResponse returns — a sendfile-capable engine that needs the
// file beyond this call dups the descriptor.
type FileResponder interface {
	WriteFileResponse(s *Stream, status int, headers [][2]string, file *os.File, offset, length int64) (handled bool, err error)
}

// H2Controller provides HTTP/2 connection-level control operations.
// Only H2-capable engines implement this; H1 adapters, test recorders,
// and stdlib bridges do not.
type H2Controller interface {
	SendGoAway(lastStreamID uint32, code http2.ErrCode, debug []byte) error
	MarkStreamClosed(streamID uint32)
	IsStreamClosed(streamID uint32) bool
	WriteRSTStreamPriority(streamID uint32, code http2.ErrCode) error
	CloseConn() error
}

// Streamer supports incremental response writing. Engines that support
// streaming implement this interface on their ResponseWriter. The existing
// WriteResponse path is preserved for non-streaming responses (hot path).
type Streamer interface {
	// WriteHeader sends the status line and headers. Must be called once before Write.
	WriteHeader(stream *Stream, status int, headers [][2]string) error
	// Write sends a chunk of the response body. May be called multiple times.
	Write(stream *Stream, data []byte) error
	// Flush asks the engine to send buffered data to the network; an
	// implementation may do nothing (the HTTP/1.1 and HTTP/2 adapters of the
	// native engines do nothing: what a Write queued goes out on the engine's
	// schedule). It is a request, not a delivery guarantee, and a return from
	// Flush does not mean the peer has the bytes.
	Flush(stream *Stream) error
	// Close signals end of the response body.
	Close(stream *Stream) error
}

// Resetter is an optional interface a ResponseWriter may implement to
// reset a stream from the goroutine of its handler (celeris#889). ResetStream
// queues RST_STREAM(code) behind the frames the stream has already queued, so
// the peer never sees it ahead of the stream's own HEADERS or DATA, and
// cancels the stream so nothing more of it is sent. It is safe from any
// goroutine, the event loop and a pool handler's alike. The processor uses it
// when a handler is over without having finished its response; a writer
// without it (a test recorder) falls back to H2Controller.WriteRSTStreamPriority,
// which belongs to the event loop: used for a stream whose handler ran there,
// while a pool stream is only cancelled.
type Resetter interface {
	ResetStream(stream *Stream, code http2.ErrCode)
}

// DetachNotifier is an optional interface a ResponseWriter may implement to
// be told that the handler detached the stream (Context.Detach): from then on
// the stream is the connection's, and runs until the peer or the handler ends
// it, as it does on the native engines, which apply neither ReadTimeout nor
// WriteTimeout to a detached connection. The std engine implements it to clear
// the write deadline net/http armed for the request, and to bound the stream's
// writes one at a time instead (celeris#855). StreamDetached is called once
// per detach, on the goroutine that called Detach, before Detach returns; the
// native engines' writers do not implement it, so the check costs a detach (a
// stream's start) one failed type assertion.
type DetachNotifier interface {
	StreamDetached()
}

// UseStreamer is a Streamer whose calls are tied to one use of a stream
// object (celeris#904). A stream is pooled, and on HTTP/2 its use ends when
// its handler returns, detached or not (Context.Detach); a StreamWriter that a
// goroutine of the handler still holds would otherwise act on whatever the
// object serves next, on any connection. Context.StreamWriter takes the use
// token (Stream.Gen) when the writer is made and presents it with every call;
// a call whose use is over returns ErrStreamEnded (or the stream's context
// error if the peer reset it) and has no effect. Engines whose streams outlive
// their handlers (HTTP/1) implement only Streamer.
type UseStreamer interface {
	Streamer
	WriteHeaderUse(stream *Stream, gen uint64, status int, headers [][2]string) error
	WriteUse(stream *Stream, gen uint64, data []byte) error
	CloseUse(stream *Stream, gen uint64) error
}
