// Package conn provides shared HTTP/1.1 and HTTP/2 connection handling.
package conn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris/internal/ctxkit"
	h1 "github.com/goceleris/celeris/protocol/h1"
	"github.com/goceleris/celeris/protocol/h2/stream"
)

// timeNow is a package-level alias so tests can stub the clock when
// exercising the read-header-deadline path. Production = time.Now.
var timeNow = time.Now

func init() {
	// Wire the H1-package helpers so protocol/h2/stream can lazily
	// materialize request headers from raw bytes without taking a
	// build-time dependency on protocol/h1.
	stream.SetLazyHeaderHelpers(h1.UnsafeLowerHeader, h1.UnsafeString)
}

// ErrAsyncDispatch is returned by ProcessH1 when, running inline on the
// worker (H1State.InlineMode), it parses a request whose route is async
// (H1State.RouteAsync). The handler has NOT run; the unconsumed bytes
// (this request onward) are stashed in state.buffer. The engine must hand
// the connection to its per-conn dispatch goroutine and re-run ProcessH1
// with InlineMode=false. Mirrors the ErrUpgradeH2C handoff pattern.
var ErrAsyncDispatch = errors.New("celeris: route requires async dispatch")

// ErrHijacked is returned by ProcessH1 when the connection was hijacked.
// The engine must not close or reuse the FD after receiving this error.
var ErrHijacked = errors.New("celeris: connection hijacked")

// ClearHeaderDeadline drops the slowloris-defence read-header deadline
// — called after a successful ParseRequest signals that the next state
// is request handling, not header reading. The engine's checkTimeouts
// sweep treats 0 as "no deadline" and skips the check.
func (s *H1State) ClearHeaderDeadline() {
	s.HeaderDeadlineNs.Store(0)
}

// ArmHeaderDeadline arms the slowloris-defence read-header deadline
// to now + ReadHeaderTimeoutNs. Called by:
//   - the engine at conn accept (covers "client never sends a byte"),
//   - ProcessH1 when it observes data arriving on a conn whose deadline
//     was cleared (post-handler-idle keep-alive state).
//
// Idempotent: if HeaderDeadlineNs is already non-zero, this is a no-op
// — the deadline is an ABSOLUTE budget for header completion, NOT
// per-read. Resetting on every read would let a slow-drip client
// extend the budget indefinitely (the bug this fix is meant to defeat).
//
// No-op if ReadHeaderTimeoutNs == 0 (config disabled or std-engine
// compat mode).
func (s *H1State) ArmHeaderDeadline() {
	if s.ReadHeaderTimeoutNs == 0 {
		return
	}
	if s.HeaderDeadlineNs.Load() != 0 {
		// Already armed; don't reset (slowloris-bypass guard).
		return
	}
	s.HeaderDeadlineNs.Store(timeNow().UnixNano() + s.ReadHeaderTimeoutNs)
	// Notify the engine so it can submit a kernel-enforced timer
	// (iouring IORING_OP_TIMEOUT SQE) — see field docstring.
	if s.OnHeaderDeadlineArmed != nil {
		s.OnHeaderDeadlineArmed()
	}
}

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
	parser *h1.Parser
	buffer bytes.Buffer
	// bodyBuf holds an in-progress fixed-length body that spans multiple
	// ProcessH1 calls. Reused across requests so each connection allocates
	// once at its peak body size. Using a dedicated slice bypasses the
	// state.buffer (*bytes.Buffer) doubling-grow and the paired
	// cs.buf→state.buffer memcpy that previously dominated the 1 MiB POST
	// hot path.
	bodyBuf            []byte
	bodyNeeded         int
	req                h1.Request
	rw                 h1ResponseAdapter // embedded — reused per request, avoids heap alloc
	stream             *stream.Stream    // per-connection cached stream (avoids pool Get/Put per request)
	RemoteAddr         string
	HijackFn           func() (net.Conn, error)                            // set by engine; nil if unsupported
	MaxRequestBodySize int64                                               // 0 = use default (100 MB)
	OnExpectContinue   func(method, path string, headers [][2]string) bool // nil = always accept
	OnDetach           func()                                              // set by engine; called on Context.Detach
	Detached           atomic.Bool                                         // set by OnDetach; breaks pipelining loop. atomic: written on the dispatch/handler goroutine (Context.Detach -> OnDetach), read on the engine worker goroutine.

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

	// HeaderDeadlineNs holds the absolute deadline (Unix nanoseconds) by
	// which the next request's headers must be fully received, or the
	// engine must close the connection. This is the canonical slowloris
	// defence on the iouring + epoll engines (std wires the equivalent
	// via http.Server.ReadHeaderTimeout). Set by the engine at conn-
	// accept and after each successful request parse (so the next
	// pipelined / keep-alive request gets a fresh budget). Cleared when
	// ProcessH1 returns having fully parsed a request. 0 = no deadline
	// (e.g. -1 in config explicitly disables; ReadHeaderTimeout was
	// otherwise unenforced on iouring + epoll prior to celeris v1.4.11).
	HeaderDeadlineNs atomic.Int64

	// ReadHeaderTimeoutNs is the configured ReadHeaderTimeout in
	// nanoseconds, supplied by the engine at initProtocol. Used by
	// ProcessH1 to re-arm HeaderDeadlineNs after each request parse —
	// keeps the deadline logic local to where it matters (the parsing
	// loop) without ProcessH1 needing access to the engine's *cfg.
	// 0 = no header deadline (config disabled).
	ReadHeaderTimeoutNs int64

	// OnHeaderDeadlineArmed is invoked synchronously by ArmHeaderDeadline
	// whenever the deadline transitions from 0 → armed. The iouring
	// engine sets this to submit an IORING_OP_TIMEOUT SQE so the kernel
	// closes the conn at the deadline regardless of sweep cadence. The
	// epoll engine leaves it nil (uses per-worker timerfd instead). nil
	// = no-op.
	//
	// The callback runs on the engine's worker thread (ProcessH1's
	// caller). It MUST NOT block — SQE submission only.
	OnHeaderDeadlineArmed func()

	// EnableH2Upgrade, when true, permits this connection to honor a
	// valid RFC 7540 §3.2 h2c upgrade request. Set by the engine at
	// initProtocol time from resource.Config.EnableH2Upgrade.
	EnableH2Upgrade bool
	// UpgradeInfo is populated by ProcessH1 just before returning
	// ErrUpgradeH2C. The engine consumes it via switchToH2 and then clears
	// it. Always nil on a clean (non-upgrade) connection.
	UpgradeInfo *UpgradeInfo

	// WorkerID is the engine worker ID owning the connection (-1 for
	// unset / std engine). Set by the engine at initProtocol; copied to
	// the stream by populateCachedStream so HandleStream can read it
	// without a per-request ctx.Value() walk.
	WorkerID    int32
	WorkerIDSet bool

	// NowNs is the engine's worker-local cached time.Now().UnixNano()
	// for the most recent recv. The engine writes it just before calling
	// ProcessH1; populateCachedStream copies it to the stream so the
	// handler can record start time without a per-request time.Now() vDSO.
	NowNs int64

	// InlineMode + RouteAsync implement per-handler async dispatch on the
	// H1 path (celeris #300). When the engine runs ProcessH1 inline on
	// the worker thread (InlineMode=true) and RouteAsync reports that the
	// just-parsed request's route is async, ProcessH1 stops BEFORE
	// invoking the handler, stashes the unconsumed bytes (this request
	// onward) in state.buffer, and returns ErrAsyncDispatch. The engine
	// then hands the connection to its per-conn dispatch goroutine, which
	// re-runs ProcessH1 with InlineMode=false (every request runs on the
	// goroutine). This lets sync routes run inline on the worker (fast,
	// no handoff) while async routes never block it — per-request, on a
	// server that mixes both. Both nil/false on the dispatch-goroutine
	// path and on pure-sync / pure-async servers (no behavior change).
	InlineMode bool
	RouteAsync func(method, path string) bool
}

// TakeBufferedBytes returns a copy of any bytes ProcessH1 stashed in the
// parse buffer (notably the request that triggered ErrAsyncDispatch, plus
// any pipelined bytes) and clears the buffer. The engine uses it to hand
// the stashed request to the per-conn dispatch goroutine. Returns nil when
// the buffer is empty.
func (s *H1State) TakeBufferedBytes() []byte {
	if s.buffer.Len() == 0 {
		return nil
	}
	b := make([]byte, s.buffer.Len())
	copy(b, s.buffer.Bytes())
	s.buffer.Reset()
	return b
}

// HasPendingData reports whether ProcessH1 left partial-request state that a
// subsequent recv must continue (buffered headers awaiting more bytes, or a
// fixed-length body still accumulating). The engine uses it after an inline
// ProcessH1 to decide whether to promote the conn to its dispatch goroutine
// for the continuation (so the partial-state parse paths never run inline).
func (s *H1State) HasPendingData() bool {
	return s.buffer.Len() > 0 || s.bodyNeeded > 0
}

// HasPendingDispatchState reports whether ProcessH1 left partial state that
// MUST be continued on the dispatch goroutine (InlineMode=false): buffered
// partial headers or a chunked body in progress, both of which resume via the
// buffered parse path that does not re-run the per-route async check. A
// fixed-length body in progress (bodyNeeded > 0) is deliberately EXCLUDED: its
// continuation re-parses the already-async-checked (so provably non-async)
// request and dispatches the handler inline on the worker — exactly as the
// sync engine does — so it must NOT force the conn onto the slower async
// dispatch path. Promoting on it permanently poisoned keep-alive conns that
// hit even one split-across-recvs body (the post-4k regression).
func (s *H1State) HasPendingDispatchState() bool {
	return s.buffer.Len() > 0 && s.bodyNeeded <= 0
}

// UpdateWriteFn replaces the response adapter's write function. Called by
// OnDetach to route StreamWriter writes through the mutex-guarded writeFn.
func (s *H1State) UpdateWriteFn(fn func([]byte)) {
	s.rw.write = fn
}

// SetWriteBodyFn installs a scatter-gather body writer on the response
// adapter. When non-nil, WriteResponse for large bodies bypasses the
// respBuf → cs.writeBuf copy and hands the body slice straight to the
// engine for a single WRITEV/sendmsg submission. Callers must not mutate
// the body slice after a writeBody call until the response returns (the
// engine keeps a reference until the SEND CQE fires). The std engine and
// adapters that cannot express scatter-gather may leave this unset; the
// adapter then falls back to the single-buffer path.
func (s *H1State) SetWriteBodyFn(fn func([]byte)) {
	s.rw.writeBody = fn
}

// SetSendFileFn installs a zero-copy sendfile(2) hook on the response
// adapter. When non-nil, h1ResponseAdapter.WriteFileResponse routes
// large file bodies through it (the engine takes the file slice and dups
// the descriptor); when nil, WriteFileResponse declines and the caller
// falls back to the buffered read+write path. Installed only by sendfile-
// capable engines (epoll) and only in non-async mode — the async dispatch
// goroutine would race the worker on the per-connection sendfile state.
func (s *H1State) SetSendFileFn(fn func(header []byte, file *os.File, offset, length int64) error) {
	s.rw.sendFile = fn
}

func (s *H1State) maxBodySize() int64 {
	return s.MaxRequestBodySize // 0 = unlimited (limit > 0 guard at call sites)
}

// NextRecvBuf returns the tail of the pending body buffer when H1 is in a
// partial-body state, so the engine can recv directly into bodyBuf and
// skip the cs.buf → bodyBuf memcpy. Returns nil when the engine should
// use its own per-connection read buffer.
//
// Callers MUST pair a successful non-nil read with ConsumeBodyRecv(n) so
// H1State can detect body completion and trigger handler dispatch.
func (s *H1State) NextRecvBuf() []byte {
	if s.bodyNeeded <= 0 {
		return nil
	}
	free := cap(s.bodyBuf) - len(s.bodyBuf)
	if free <= 0 {
		return nil
	}
	return s.bodyBuf[len(s.bodyBuf) : len(s.bodyBuf)+free]
}

// ConsumeBodyRecv extends bodyBuf by n bytes and reports whether the body
// is now complete. Paired with NextRecvBuf on the engine side.
func (s *H1State) ConsumeBodyRecv(n int) (complete bool) {
	s.bodyBuf = s.bodyBuf[:len(s.bodyBuf)+n]
	return len(s.bodyBuf) >= s.bodyNeeded
}

// DispatchBufferedBody runs the handler against the fully-buffered body
// that NextRecvBuf / ConsumeBodyRecv accumulated. The engine must have
// observed complete=true from ConsumeBodyRecv. Returns the residual
// unread bytes if the recv overshot into the next pipelined request, or
// nil when nothing is left. KeepAlive / Detached state lives on s.req —
// same semantics as ProcessH1's equivalent exit.
func (s *H1State) DispatchBufferedBody(ctx context.Context, handler stream.Handler, write func([]byte)) ([]byte, error) {
	overflow := len(s.bodyBuf) - s.bodyNeeded
	var rest []byte
	if overflow > 0 {
		rest = append([]byte(nil), s.bodyBuf[s.bodyNeeded:]...)
		s.bodyBuf = s.bodyBuf[:s.bodyNeeded]
	}
	s.parser.Reset(s.buffer.Bytes())
	s.req.Reset()
	if _, err := s.parser.ParseRequest(&s.req); err != nil {
		writeErrorResponse(write, 400, "Bad Request")
		return nil, err
	}
	s.buffer.Reset()
	bodyData := s.bodyBuf
	if err := tryUpgradeH2C(s, bodyData, rest, write); err != nil {
		return nil, err
	}
	if s.UpgradeInfo != nil {
		return nil, ErrUpgradeH2C
	}
	if err := handleH1Request(ctx, s, bodyData, handler, write); err != nil {
		return nil, err
	}
	s.bodyBuf = s.bodyBuf[:0]
	s.bodyNeeded = 0
	if s.Detached.Load() {
		return nil, nil
	}
	if !s.req.KeepAlive {
		return nil, errConnectionClose
	}
	return rest, nil
}

// NewH1State creates a new H1 connection state with zero-copy header parsing.
func NewH1State() *H1State {
	p := h1.NewParser()
	p.SetZeroCopy(true)
	return &H1State{
		parser: p,
	}
}

// DisableH2CDetect tells the H1 parser it can skip per-header h2c-upgrade
// detection. The engine calls this on connections whose config has
// EnableH2Upgrade=false; the upgrade path is impossible in that mode and
// parsing every header against Upgrade / HTTP2-Settings / Connection is
// wasted work on the recv hot path.
func (s *H1State) DisableH2CDetect() {
	s.parser.SetDisableH2CDetect(true)
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
	// Free the body buffer — for a conn that handled a 1 MiB POST, this
	// is where we relinquish the arena. Reuse across requests on the
	// same conn is fine (the 10 k idle-conn × 1 MiB scenario only
	// triggers if every conn sustained a huge POST, which implies the
	// memory was already being paid for real traffic).
	state.bodyBuf = nil
	state.bodyNeeded = 0
}

// ProcessH1 processes incoming H1 data, parsing requests and calling the handler.
// The write callback is used to send response bytes back to the connection.
func ProcessH1(ctx context.Context, data []byte, state *H1State, handler stream.Handler,
	write func([]byte)) (retErr error) {

	// WebSocket upgrade: deliver raw bytes to the middleware goroutine
	// instead of parsing as H1. The delivery callback writes to an io.Pipe
	// that the goroutine reads from.
	if state.Detached.Load() && state.WSDataDelivery != nil {
		state.WSDataDelivery(data)
		return nil
	}

	// Slowloris defence — ReadHeaderTimeout state machine.
	//
	// Semantic: the deadline tracks the absolute time by which the
	// current request's HEADERS must be fully received. It must NOT
	// be confused with keep-alive idle (which IdleTimeout handles).
	//
	//   - conn-accept: engine arms once (covers "client never sends")
	//   - ProcessH1 entry with data arriving on a request not yet in
	//     body/parsed state, and deadline currently 0 (post-clear or
	//     post-handler-idle): arm. Don't reset an already-armed
	//     deadline — that would let a slow-drip client extend its
	//     budget indefinitely.
	//   - ParseRequest success (consumed > 0): clear. Body / handler /
	//     keep-alive idle phases do NOT have a header deadline.
	//
	// Why entry-arm-if-zero (not entry-clear-then-defer-arm): the
	// keep-alive idle window between requests must NOT count against
	// ReadHeaderTimeout. With defer-arm, every successful request
	// would re-arm the deadline; the next keep-alive idle conn would
	// get killed at ReadHeaderTimeout instead of IdleTimeout.
	if state.HeaderDeadlineNs.Load() == 0 && state.bodyNeeded == 0 && !state.Detached.Load() {
		state.ArmHeaderDeadline()
	}

	// In-progress fixed-length body spanning multiple reads. Append into
	// the dedicated bodyBuf (no state.buffer memcpy), then re-parse the
	// headers from state.buffer (kept stable across reads since the H1
	// parser runs in zero-copy mode and state.req slices reference the
	// per-call `data` buffer that the engine reuses). When the body is
	// full, dispatch the handler.
	if state.bodyNeeded > 0 {
		need := state.bodyNeeded - len(state.bodyBuf)
		if len(data) < need {
			state.bodyBuf = append(state.bodyBuf, data...)
			return nil
		}
		state.bodyBuf = append(state.bodyBuf, data[:need]...)
		rest := data[need:]
		// Re-parse headers from state.buffer so req fields point at
		// stable memory (the earlier partial-body branch stashed them).
		state.parser.Reset(state.buffer.Bytes())
		state.req.Reset()
		if _, err := state.parser.ParseRequest(&state.req); err != nil {
			writeErrorResponse(write, 400, "Bad Request")
			return err
		}
		// Headers complete (this is a re-parse from buffer, so the
		// original parse already succeeded — clear is idempotent).
		state.ClearHeaderDeadline()
		state.buffer.Reset()
		bodyData := state.bodyBuf
		if err := tryUpgradeH2C(state, bodyData, rest, write); err != nil {
			return err
		}
		if state.UpgradeInfo != nil {
			return ErrUpgradeH2C
		}
		if err := handleH1Request(ctx, state, bodyData, handler, write); err != nil {
			return err
		}
		state.bodyBuf = state.bodyBuf[:0]
		state.bodyNeeded = 0
		if state.Detached.Load() {
			return nil
		}
		if !state.req.KeepAlive {
			return errConnectionClose
		}
		if len(rest) == 0 {
			return nil
		}
		data = rest
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
			// Headers complete — clear the slowloris deadline. The
			// body / handler / keep-alive idle that follow are NOT
			// header-deadline-eligible. Re-arm happens at the next
			// ProcessH1 entry that observes deadline == 0.
			state.ClearHeaderDeadline()

			// Per-handler async (celeris #300): running inline on the
			// worker, if this request's route is async, stop before the
			// handler runs, stash this request (+ any pipelined bytes)
			// in state.buffer, and signal the engine to promote the conn
			// to its dispatch goroutine. data[offset:] starts at this
			// request (offset is the request head; consumed has not been
			// applied to offset yet). The engine re-runs ProcessH1 with
			// InlineMode=false, draining state.buffer via the buffered
			// path below — every request then runs on the goroutine.
			if state.InlineMode && state.RouteAsync != nil &&
				state.RouteAsync(state.req.Method, state.req.Path) {
				state.buffer.Reset()
				state.buffer.Write(data[offset:])
				return ErrAsyncDispatch
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
				// Zero-copy body fast path: if the entire content-length
				// body is already in `data`, dispatch directly from the
				// engine's read buffer — skipping a full-body memcpy into
				// state.buffer. Only applies to fixed-length bodies;
				// chunked encoding keeps going through the buffered path
				// so ParseChunkedBody can accumulate across reads.
				if bodyNeeded > 0 {
					remaining := len(data) - offset - consumed
					if int64(remaining) >= bodyNeeded {
						bodyStart := offset + consumed
						bodyEnd := bodyStart + int(bodyNeeded)
						bodyData := data[bodyStart:bodyEnd]
						if err := tryUpgradeH2C(state, bodyData, data[bodyEnd:], write); err != nil {
							return err
						}
						if state.UpgradeInfo != nil {
							return ErrUpgradeH2C
						}
						if err := handleH1Request(ctx, state, bodyData, handler, write); err != nil {
							return err
						}
						if state.Detached.Load() {
							return nil
						}
						if !state.req.KeepAlive {
							return errConnectionClose
						}
						offset = bodyEnd
						continue
					}
					// Partial body: accumulate body bytes into bodyBuf
					// (reused across requests on this connection), and
					// stash the header bytes in state.buffer so state.req
					// can be re-parsed from stable memory when the body
					// completes across subsequent reads (req slices today
					// point into the per-call `data`, which the engine
					// reuses).
					need := int(bodyNeeded)
					if cap(state.bodyBuf) < need {
						state.bodyBuf = make([]byte, 0, need)
					} else {
						state.bodyBuf = state.bodyBuf[:0]
					}
					if remaining > 0 {
						state.bodyBuf = append(state.bodyBuf, data[offset+consumed:]...)
					}
					state.buffer.Reset()
					state.buffer.Write(data[offset : offset+consumed])
					state.bodyNeeded = need
					return nil
				}
				state.buffer.Write(data[offset:])
				break
			}

			if err := tryUpgradeH2C(state, nil, data[offset+consumed:], write); err != nil {
				return err
			}
			if state.UpgradeInfo != nil {
				return ErrUpgradeH2C
			}

			if err := handleH1Request(ctx, state, nil, handler, write); err != nil {
				return err
			}
			if state.Detached.Load() {
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
		// Headers complete — clear the slowloris deadline. See the
		// state-machine comment at the top of ProcessH1.
		state.ClearHeaderDeadline()

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
				// Body is incomplete. Move any body bytes currently in
				// state.buffer into bodyBuf, then shrink state.buffer
				// back to just the header bytes. Subsequent ProcessH1
				// calls will take the state.bodyNeeded > 0 short-circuit
				// and append directly to bodyBuf, avoiding the
				// bytes.Buffer doubling-grow + cs.buf→state.buffer
				// memcpy on each partial-body read.
				need := int(bodyNeeded)
				if cap(state.bodyBuf) < need {
					state.bodyBuf = make([]byte, 0, need)
				} else {
					state.bodyBuf = state.bodyBuf[:0]
				}
				if available > 0 {
					bufBytes := state.buffer.Bytes()
					state.bodyBuf = append(state.bodyBuf, bufBytes[consumed:]...)
					// Trim body bytes off the buffer; headers remain.
					state.buffer.Truncate(consumed)
				}
				state.bodyNeeded = need
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

		if err := tryUpgradeH2C(state, bodyData, state.buffer.Bytes(), write); err != nil {
			return err
		}
		if state.UpgradeInfo != nil {
			return ErrUpgradeH2C
		}

		if err := handleH1Request(ctx, state, bodyData, handler, write); err != nil {
			return err
		}
		if state.Detached.Load() {
			return nil // see fast-path comment above
		}
		if !state.req.KeepAlive {
			return errConnectionClose
		}
	}
	return nil
}

// tryUpgradeH2C inspects the just-parsed request for an h2c upgrade. On a
// valid request it writes the 101 Switching Protocols response, captures
// the upgrade state into state.UpgradeInfo (which triggers the caller to
// return ErrUpgradeH2C), and returns nil. On any failure (settings decode,
// feature disabled, not-a-upgrade) it returns nil and leaves UpgradeInfo
// nil so the caller falls through to the normal handler path.
//
// remaining holds bytes in the recv buffer AFTER the just-consumed H1
// request (and its body). These are preserved for H2 — they may already
// contain the H2 client preface and initial SETTINGS.
func tryUpgradeH2C(state *H1State, body, remaining []byte, write func([]byte)) error {
	if !state.EnableH2Upgrade || !state.req.UpgradeH2C {
		return nil
	}
	settings, err := DecodeHTTP2Settings(state.req.HTTP2Settings)
	if err != nil {
		// Malformed settings: silently fall through to normal H1 handling.
		// This matches the spec's tolerance requirement and avoids a 400
		// on a merely-noisy client.
		return nil
	}

	// Write 101 response. Connection/Upgrade headers per RFC 7540 §3.2.
	write([]byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: h2c\r\n\r\n"))

	// Acquire a pooled UpgradeInfo. Body and Remaining alias state.buffer's
	// internal storage — that buffer is NOT modified between our return
	// (with ErrUpgradeH2C) and the engine's switchToH2 call that consumes
	// the info, so aliasing is safe. ReleaseUpgradeInfo nils these fields
	// before returning the struct to the pool so we don't pin buffers
	// beyond the upgrade's lifetime.
	info := acquireUpgradeInfo()
	info.Settings = settings
	info.Method = state.req.Method
	info.URI = state.req.Path
	info.Headers = appendH2CHeaders(info.Headers[:0], &state.req)
	info.Body = body
	info.Remaining = remaining
	state.UpgradeInfo = info
	return nil
}

// copyH2CHeaders returns a defensive copy of the request's headers with
// hop-by-hop + upgrade-mechanism headers stripped. These must NOT appear
// on H2 stream 1 (RFC 7540 §8.1.2.2 forbids connection-specific headers).
//
// The returned strings MUST be heap-owned copies: the H1 recv buffer
// (which backs rh[0] and rh[1]) is reused after switchToH2 releases the
// H1 state. Using h1.UnsafeLowerHeader here would alias that buffer on
// uncommon names, so we force an allocating ToLower for the header name
// and a string copy for the value.
func copyH2CHeaders(req *h1.Request) [][2]string {
	return appendH2CHeaders(make([][2]string, 0, len(req.RawHeaders)), req)
}

// appendH2CHeaders appends the non-hop-by-hop H1 request headers (lowercased
// name + owned-string value) to out. Exposed so the upgrade path can reuse
// a pooled UpgradeInfo.Headers backing array — on pooled entry the slice
// arrives with zero length but non-zero capacity.
func appendH2CHeaders(out [][2]string, req *h1.Request) [][2]string {
	for _, rh := range req.RawHeaders {
		name := h1.LowerHeaderCopy(rh[0])
		switch name {
		case "upgrade", "connection", "http2-settings":
			continue
		}
		out = append(out, [2]string{name, string(rh[1])})
	}
	return out
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
	// per request. Reset per-request fields. write and hijackFn are stable
	// across requests on the same connection (write is the engine's
	// per-conn writeFn closure, hijackFn is set once at initProtocol), so
	// wire them on the first request and skip the re-assignment after.
	// UpdateWriteFn handles the rare detach swap if it happens.
	rw := &state.rw
	if rw.write == nil {
		rw.write = write
		rw.hijackFn = state.HijackFn
	}
	rw.keepAlive = req.KeepAlive
	rw.isHEAD = req.Method == "HEAD"
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
	s.WorkerID = state.WorkerID
	s.WorkerIDSet = state.WorkerIDSet
	s.StartTimeNs = state.NowNs
	// Pseudo-headers (:method/:path/:scheme/:authority) live on dedicated
	// Stream fields so Context.extractRequestInfo / Context.Host read them
	// without walking s.Headers. The slice append + 4 pseudo-header
	// allocations are deferred to Stream.MaterializeHeaders, which the
	// Context only triggers when a handler actually reads request headers
	// via Header / RequestHeaders / ForEachHeader. The same goes for the
	// raw-header lowercase loop (LazyRawHeaders). Bench-style handlers
	// that read only c.method / c.path skip both passes entirely.
	s.Headers = s.Headers[:0]
	s.Method = req.Method
	s.Path = req.Path
	s.Scheme = "http"
	s.Authority = req.Host
	s.LazyRawHeaders = req.RawHeaders
	s.IsHEAD = req.Method == "HEAD"

	if len(body) > 0 {
		// Zero-copy: body is a slice into the H1 read buffer that stays
		// stable for the handler's synchronous lifetime (state.buffer is
		// only advanced past these bytes before handler dispatch).
		s.SetRawBody(body)
	}
	s.EndStream = true
	// Direct assignment — no mutex needed. H1 streams are single-threaded
	// (no manager), and the stream is not yet visible to any handler.
	// Direct atomic store — H1 streams are single-threaded with no manager,
	// so we skip SetState's atomic.Swap + manager.updateActiveCount overhead.
	s.StoreState(stream.StateHalfClosedRemote)
	return s
}
