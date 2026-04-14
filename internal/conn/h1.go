// Package conn provides shared HTTP/1.1 and HTTP/2 connection handling.
package conn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"

	"github.com/goceleris/celeris/internal/ctxkit"
	h1 "github.com/goceleris/celeris/protocol/h1"
	"github.com/goceleris/celeris/protocol/h2/stream"
)

// ErrHijacked is returned by ProcessH1 when the connection was hijacked.
// The engine must not close or reuse the FD after receiving this error.
var ErrHijacked = errors.New("celeris: connection hijacked")

// errConnectionClose is returned when the client requests Connection: close.
// Pre-allocated to avoid per-request fmt.Errorf allocation.
var errConnectionClose = errors.New("connection close requested")

// continue100Response is sent when the client sends "Expect: 100-continue"
// to signal that the server is willing to accept the request body.
var continue100Response = []byte("HTTP/1.1 100 Continue\r\n\r\n")

// expectation417Response is sent when OnExpectContinue rejects the request.
var expectation417Response = []byte("HTTP/1.1 417 Expectation Failed\r\nContent-Length: 0\r\n\r\n")

// H1State holds per-connection H1 parsing state.
type H1State struct {
	parser             *h1.Parser
	buffer             bytes.Buffer
	req                h1.Request
	rw                 h1ResponseAdapter // embedded — reused per request, avoids heap alloc
	stream             *stream.Stream    // per-connection cached stream (avoids pool Get/Put per request)
	RemoteAddr         string
	HijackFn           func() (net.Conn, error)                            // set by engine; nil if unsupported
	MaxRequestBodySize int64                                               // 0 = use default (100 MB)
	OnExpectContinue   func(method, path string, headers [][2]string) bool // nil = always accept
	OnDetach           func()                                              // set by engine; called on Context.Detach
	Detached           bool                                                // set by OnDetach; breaks pipelining loop

	// WSDataDelivery is set by the WebSocket middleware after upgrade (101 sent).
	// When non-nil and Detached, subsequent reads are delivered as raw bytes
	// to this callback instead of being parsed as H1 requests. The callback
	// is called on the event loop thread — it must not block.
	WSDataDelivery func(data []byte)

	// RawWriteFn is set after Detach. It writes raw bytes to the engine's
	// write buffer, bypassing H1 chunked encoding. Used by WebSocket for
	// frame writes.
	RawWriteFn func([]byte)

	// OnDetachClose is called by the engine when it closes a detached
	// connection (timeout, error, shutdown). The WebSocket middleware sets
	// this to close the io.Pipe and data channel, unblocking the handler
	// goroutine. Called under cs.detachMu — must not block.
	//
	// Detached-connection API surface — stable. The fields below
	// (OnDetachClose, OnError, PauseRecv, ResumeRecv, IdleDeadlineNs) form
	// the contract between the engine layer and any long-lived-connection
	// middleware (WebSocket, SSE, gRPC streaming, etc). They are part of
	// the celeris public API: changes require a major version bump.
	OnDetachClose func()

	// OnError is called by the engine when an I/O failure occurs on a
	// detached connection (read error, write error, EPIPE, ECONNRESET, etc).
	// The WebSocket middleware uses this to surface engine-side errors
	// from the next user-level Read or Write call. Called under cs.detachMu
	// — must not block.
	OnError func(err error)

	// PauseRecv and ResumeRecv are set by the engine in OnDetach. The
	// middleware calls them to apply TCP-level backpressure on the inbound
	// data path. They may be called from any goroutine and are no-ops if
	// the engine cannot pause reads (e.g. std hijacked path).
	PauseRecv  func()
	ResumeRecv func()

	// IdleDeadlineNs holds the absolute deadline (Unix nanoseconds) at
	// which a detached connection should be closed by the engine. The
	// WebSocket middleware updates this after each successful frame read;
	// the engine's idle sweep checks it on detached connections. 0 = no
	// deadline.
	IdleDeadlineNs atomic.Int64
}

// UpdateWriteFn replaces the response adapter's write function. Called by
// OnDetach to route StreamWriter writes through the mutex-guarded writeFn.
func (s *H1State) UpdateWriteFn(fn func([]byte)) {
	s.rw.write = fn
}

func (s *H1State) maxBodySize() int64 {
	return s.MaxRequestBodySize // 0 = unlimited (limit > 0 guard at call sites)
}

// NewH1State creates a new H1 connection state with zero-copy header parsing.
func NewH1State() *H1State {
	p := h1.NewParser()
	p.SetZeroCopy(true)
	return &H1State{
		parser: p,
	}
}

// CloseH1 releases the cached stream and context (if any) back to their pools.
func CloseH1(state *H1State) {
	if state.stream != nil {
		// Release the cached context back to its pool before releasing the stream.
		if state.stream.CachedCtx != nil {
			ctxkit.ReleaseContext(state.stream.CachedCtx)
			state.stream.CachedCtx = nil
		}
		state.stream.Release()
		state.stream = nil
	}
}

// ProcessH1 processes incoming H1 data, parsing requests and calling the handler.
// The write callback is used to send response bytes back to the connection.
func ProcessH1(ctx context.Context, data []byte, state *H1State, handler stream.Handler,
	write func([]byte)) error {

	// WebSocket upgrade: deliver raw bytes to the middleware goroutine
	// instead of parsing as H1. The delivery callback writes to an io.Pipe
	// that the goroutine reads from.
	if state.Detached && state.WSDataDelivery != nil {
		state.WSDataDelivery(data)
		return nil
	}

	if state.buffer.Len() == 0 {
		offset := 0
		for offset < len(data) {
			state.parser.Reset(data[offset:])
			state.req.Reset()
			consumed, err := state.parser.ParseRequest(&state.req)
			if err != nil {
				writeErrorResponse(write, 400, "Bad Request")
				return err
			}
			if consumed == 0 {
				state.buffer.Write(data[offset:])
				return nil
			}

			bodyNeeded := int64(0)
			if state.req.ChunkedEncoding {
				bodyNeeded = -1
			} else if state.req.ContentLength > 0 {
				bodyNeeded = state.req.ContentLength
			}

			if bodyNeeded > 0 || bodyNeeded == -1 {
				if bodyNeeded > 0 {
					if limit := state.maxBodySize(); limit > 0 && bodyNeeded > limit {
						writeErrorResponse(write, 413, "Request body too large")
						return fmt.Errorf("content-length %d exceeds %d byte limit", bodyNeeded, limit)
					}
				}
				if state.req.ExpectContinue {
					if state.OnExpectContinue != nil && !safeExpectContinue(state.OnExpectContinue, state.req.Method, state.req.Path, expectHeaders(&state.req)) {
						write(expectation417Response)
						// Close connection after rejection to prevent request
						// smuggling: body bytes already in the buffer would
						// otherwise be parsed as a new request.
						return errConnectionClose
					}
					write(continue100Response)
					state.req.ExpectContinue = false
				}
				state.buffer.Write(data[offset:])
				break
			}

			if err := handleH1Request(ctx, state, nil, handler, write); err != nil {
				return err
			}
			if state.Detached {
				// Handler called Detach — a goroutine now writes through
				// the mutex-guarded writeFn. We must NOT continue parsing
				// pipelined requests with the stale `write` parameter
				// (captured before Detach replaced cs.writeFn), as that
				// would race with the goroutine on writeBuf.
				return nil
			}
			if !state.req.KeepAlive {
				return errConnectionClose
			}
			offset += consumed
		}
	} else {
		state.buffer.Write(data)
	}

	// Buffered path
	for state.buffer.Len() > 0 {
		state.parser.Reset(state.buffer.Bytes())
		state.req.Reset()
		consumed, err := state.parser.ParseRequest(&state.req)
		if err != nil {
			writeErrorResponse(write, 400, "Bad Request")
			return err
		}
		if consumed == 0 {
			break
		}

		bodyNeeded := int64(0)
		if state.req.ChunkedEncoding {
			bodyNeeded = -1
		} else if state.req.ContentLength > 0 {
			bodyNeeded = state.req.ContentLength
		}

		if state.req.ExpectContinue && (bodyNeeded > 0 || bodyNeeded == -1) {
			if state.OnExpectContinue != nil && !safeExpectContinue(state.OnExpectContinue, state.req.Method, state.req.Path, expectHeaders(&state.req)) {
				write(expectation417Response)
				// Close connection after rejection to prevent request
				// smuggling: body bytes already in the buffer would
				// otherwise be parsed as a new request.
				return errConnectionClose
			}
			write(continue100Response)
			state.req.ExpectContinue = false
		}

		if bodyNeeded > 0 {
			if limit := state.maxBodySize(); limit > 0 && bodyNeeded > limit {
				writeErrorResponse(write, 413, "Request body too large")
				return fmt.Errorf("content-length %d exceeds %d byte limit", bodyNeeded, limit)
			}
		}

		var bodyData []byte
		switch {
		case bodyNeeded > 0:
			available := int64(state.buffer.Len() - consumed)
			if available < bodyNeeded {
				return nil
			}
			state.buffer.Next(consumed)
			buf := state.buffer.Bytes()
			bodyData = buf[:bodyNeeded]
			state.buffer.Next(int(bodyNeeded))
		case bodyNeeded == -1:
			state.buffer.Next(consumed)
			var chunks bytes.Buffer
			for {
				state.parser.Reset(state.buffer.Bytes())
				chunk, chunkConsumed, cerr := state.parser.ParseChunkedBody()
				if cerr != nil {
					writeErrorResponse(write, 400, "Invalid chunked encoding")
					return cerr
				}
				if chunkConsumed == 0 {
					return nil
				}
				state.buffer.Next(chunkConsumed)
				if chunk == nil {
					break
				}
				chunks.Write(chunk)
				if limit := state.maxBodySize(); limit > 0 && int64(chunks.Len()) > limit {
					writeErrorResponse(write, 413, "Request body too large")
					return fmt.Errorf("chunked body exceeds %d byte limit", limit)
				}
			}
			bodyData = chunks.Bytes()
		default:
			state.buffer.Next(consumed)
		}

		if err := handleH1Request(ctx, state, bodyData, handler, write); err != nil {
			return err
		}
		if state.Detached {
			return nil // see fast-path comment above
		}
		if !state.req.KeepAlive {
			return errConnectionClose
		}
	}
	return nil
}

// safeExpectContinue calls the OnExpectContinue callback with panic recovery.
// A panicking callback is treated as rejection (returns false) to avoid
// crashing the event loop worker goroutine.
func safeExpectContinue(fn func(string, string, [][2]string) bool, method, path string, headers [][2]string) (accepted bool) {
	defer func() {
		if r := recover(); r != nil {
			accepted = false
		}
	}()
	return fn(method, path, headers)
}

// expectHeaders converts raw H1 headers to [][2]string for the OnExpectContinue
// callback. Zero-copy strings are safe because the callback runs synchronously
// on the event loop thread before the read buffer is reused.
func expectHeaders(req *h1.Request) [][2]string {
	if len(req.RawHeaders) == 0 {
		return nil
	}
	hdrs := make([][2]string, len(req.RawHeaders))
	for i, rh := range req.RawHeaders {
		hdrs[i] = [2]string{h1.UnsafeLowerHeader(rh[0]), h1.UnsafeString(rh[1])}
	}
	return hdrs
}

func handleH1Request(ctx context.Context, state *H1State, body []byte,
	handler stream.Handler, write func([]byte)) error {

	req := &state.req
	s := populateCachedStream(state, req, body)

	// Reuse the connection-scoped response adapter — avoids a heap allocation
	// per request. Reset per-request fields; hijackFn/write are stable.
	rw := &state.rw
	rw.write = write
	rw.keepAlive = req.KeepAlive
	rw.isHEAD = req.Method == "HEAD"
	rw.hijackFn = state.HijackFn
	rw.hijacked = false
	s.ResponseWriter = rw

	if err := handler.HandleStream(ctx, s); err != nil {
		if rw.hijacked {
			// On hijack, release the cached stream since the connection
			// is being taken over and won't be reused normally.
			state.stream.Release()
			state.stream = nil
			return ErrHijacked
		}
		writeErrorResponse(write, 500, "Internal Server Error")
		return err
	}
	if rw.hijacked {
		state.stream.Release()
		state.stream = nil
		return ErrHijacked
	}
	return nil
}

// populateCachedStream reuses the per-connection cached stream, avoiding
// sync.Pool Get/Put per request. The stream is acquired from the pool on the
// first request and retained for the connection's lifetime.
func populateCachedStream(state *H1State, req *h1.Request, body []byte) *stream.Stream {
	s := state.stream
	if s == nil {
		s = stream.NewH1Stream(1)
		state.stream = s
		// WS callbacks capture `state`, which is per-connection and
		// stable across requests. Install once on stream creation so
		// the hot path reuses the same closures.
		s.OnWSUpgrade = func(delivery func([]byte)) {
			state.WSDataDelivery = delivery
		}
		s.OnWSRawWrite = func() func([]byte) {
			return state.RawWriteFn
		}
		s.OnWSDetachClose = func(closeFn func()) {
			state.OnDetachClose = closeFn
		}
		s.OnWSSetError = func(errFn func(error)) {
			state.OnError = errFn
		}
		s.OnWSReadPauser = func() (func(), func()) {
			return state.PauseRecv, state.ResumeRecv
		}
		s.OnWSSetIdleDeadline = func(ns int64) {
			state.IdleDeadlineNs.Store(ns)
		}
	} else {
		// Reset per-request fields. The stream is reused, so clear state
		// from the previous request without returning to the pool.
		stream.ResetH1Stream(s)
	}
	s.RemoteAddr = state.RemoteAddr
	s.OnDetach = state.OnDetach
	// Reuse the stream's existing header slice capacity.
	hdrs := s.Headers[:0]
	needed := len(req.RawHeaders) + 4
	if cap(hdrs) < needed {
		hdrs = make([][2]string, 0, needed)
	}
	hdrs = append(hdrs,
		[2]string{":method", req.Method},
		[2]string{":path", req.Path},
		[2]string{":scheme", "http"},
		[2]string{":authority", req.Host},
	)
	// Zero-copy header conversion: lowercase names in-place, then create
	// strings backed by the read buffer. Safe because H1 handlers run
	// synchronously — the buffer isn't reused until after the stream is released.
	for _, rh := range req.RawHeaders {
		hdrs = append(hdrs, [2]string{
			h1.UnsafeLowerHeader(rh[0]),
			h1.UnsafeString(rh[1]),
		})
	}
	s.Headers = hdrs
	s.IsHEAD = req.Method == "HEAD"

	if len(body) > 0 {
		_, _ = s.GetBuf().Write(body)
	}
	s.EndStream = true
	// Direct assignment — no mutex needed. H1 streams are single-threaded
	// (no manager), and the stream is not yet visible to any handler.
	// Direct atomic store — H1 streams are single-threaded with no manager,
	// so we skip SetState's atomic.Swap + manager.updateActiveCount overhead.
	s.StoreState(stream.StateHalfClosedRemote)
	return s
}
