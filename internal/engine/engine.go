package engine

import (
	"context"
	"net"
	"os"

	"github.com/goceleris/celeris/internal/engine/internal/errclass"
	"github.com/goceleris/celeris/observe"
)

// Engine is the interface that all I/O engine implementations must satisfy.
// Implementations include io_uring, epoll, adaptive, and the standard library
// net/http server. Engine methods are safe for concurrent use after Listen is called.
type Engine interface {
	// Listen starts the engine and blocks until ctx is canceled or a fatal
	// error occurs. The engine begins accepting connections on the configured address.
	Listen(ctx context.Context) error
	// Shutdown gracefully drains in-flight connections, bounded by ctx. On
	// std Shutdown is the drain: when ctx expires first, it returns ctx's
	// error. On epoll and io_uring the drain runs in Listen once Listen's
	// ctx is cancelled, and Shutdown hands it ctx as its budget and returns
	// nil at once (celeris#759, celeris#760); adaptive hands ctx to its
	// sub-engines, then cancels its own Listen and waits for it, bounded by
	// ctx. An HTTP/1 handler runs to completion on every engine whatever
	// ctx (celeris#753); a handler of an HTTP/2 stream on the shared worker
	// pool can still be running when the native engines close its
	// connection at the end of the budget.
	Shutdown(ctx context.Context) error
	// Metrics returns a point-in-time snapshot of engine performance counters.
	Metrics() EngineMetrics
	// Type returns the engine type identifier (IOUring, Epoll, Adaptive, or Std).
	Type() EngineType
	// Addr returns the bound listener address, or nil if not yet listening.
	Addr() net.Addr
}

// AcceptController is implemented by engines that support dynamic accept
// control, used by the adaptive engine to pause/resume individual sub-engines
// during switches.
type AcceptController interface {
	// PauseAccept stops accepting new connections and returns once the
	// listen sockets are closed. Connections already accepted continue to be
	// served, and those still queued in the kernel's accept queue at the
	// close are drained and served (celeris#663).
	//
	// A connection whose handshake completed before the pause but which had
	// not sent its request yet is carried across it too (celeris#662,
	// celeris#675). While TCP_DEFER_ACCEPT is on, the default, the kernel
	// holds such a connection outside the accept queue until about one
	// second after its SYN, so the epoll and io_uring engines clear the
	// option on the pausing listeners and keep them open, accepting and
	// serving, for about 1.5 s before they close them. PauseAccept blocks
	// for that long. A handshake still in flight at the close is reset, as
	// closing a listen socket always does, and so is a client whose
	// retransmitted SYN-ACK is lost or goes unanswered.
	// resource.Config.DisableDeferAccept turns the option off; the pause
	// then closes the listeners at once.
	PauseAccept() error
	// ResumeAccept resumes accepting new connections after a pause.
	ResumeAccept() error
}

// SwitchFreezer is implemented by the adaptive engine to allow external code
// (e.g., benchmarks) to temporarily prevent engine switches.
type SwitchFreezer interface {
	// FreezeSwitching prevents the adaptive engine from switching.
	FreezeSwitching()
	// UnfreezeSwitching allows engine switching to resume.
	UnfreezeSwitching()
}

// SendfileCapable is an optional interface implemented by engines that
// support zero-copy file responses via sendfile(2). The H1 static-file
// response path type-asserts the engine for it; engines that do not
// implement it (iouring, std) fall back to the buffered read+write path.
// See celeris#317. The epoll engine implements it.
//
// The fdOut argument is the engine's per-connection socket FD. The
// caller drives the connState lifecycle (locking, dirty-list membership,
// timeout tracking, EAGAIN resume) — the engine's sendfile path is a
// syscall shim that advances as far as the kernel send buffer allows and
// reports backpressure for the caller to resume.
//
// offset and length describe the slice of the source file to send.
// length ≤ 0 means "send to EOF" (the implementation stats the file and
// uses size-offset). The headers slice, when non-empty, is flushed via
// write(2) before the sendfile loop begins.
//
// Returns the number of BODY bytes sent on this call and an error. A
// short send under kernel-send-buffer pressure surfaces
// EAGAIN/EWOULDBLOCK (via errors.Is) along with the partial body count,
// so the caller defers and resumes; the returned count is never
// corrupted on EAGAIN. EINTR is retried internally. The HEAD-request
// invariant (never send a body) is the caller's responsibility — the
// caller must not invoke Sendfile for a HEAD request.
type SendfileCapable interface {
	Sendfile(fdOut int, file *os.File, offset, length int64, headers []byte) (int64, error)
}

// EngineMetrics is a point-in-time snapshot of engine-level performance
// counters. It is defined in [observe.EngineMetrics]; every engine fills one
// on each [Engine.Metrics] call.
type EngineMetrics = observe.EngineMetrics //nolint:revive // user-approved name

// FillErrorClasses copies one engine's per-cause error tally into m and
// derives ErrorCount from it. It is the single wiring point between
// [errclass.Snapshot] and the Error* fields of EngineMetrics, so a bucket
// added to the former has exactly one place to be forgotten in — and
// TestFillErrorClassesSetsEveryBucket fails when it is.
//
// ErrorCount is assigned here, from the buckets, and nowhere else. No engine
// keeps a separate running total that could drift from its parts.
//
// The errclass argument type is internal, so this is reachable only from
// within internal/engine/... — the sub-engines that own the counters.
func FillErrorClasses(m *EngineMetrics, s errclass.Snapshot) {
	m.ErrorCount = s.Total()
	m.ErrorAcceptFDLimit = s.AcceptFDLimit
	m.ErrorAcceptCancelled = s.AcceptCancelled
	m.ErrorAcceptOther = s.AcceptOther
	m.ErrorConnTableCap = s.ConnTableCap
	m.ErrorConnRegister = s.ConnRegister
	m.ErrorListenerRecreate = s.ListenerRecreate
	m.ErrorTransplantAdopt = s.TransplantAdopt
	m.ErrorSendPeerGone = s.SendPeerGone
	m.ErrorSend = s.Send
	m.ErrorRequestBody = s.RequestBody
	m.ErrorHandler = s.Handler
}
