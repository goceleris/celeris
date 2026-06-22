package engine

import (
	"context"
	"net"
	"os"
)

// Engine is the interface that all I/O engine implementations must satisfy.
// Implementations include io_uring, epoll, adaptive, and the standard library
// net/http server. Engine methods are safe for concurrent use after Listen is called.
type Engine interface {
	// Listen starts the engine and blocks until ctx is canceled or a fatal
	// error occurs. The engine begins accepting connections on the configured address.
	Listen(ctx context.Context) error
	// Shutdown gracefully drains in-flight connections. The provided context
	// controls the deadline; if it expires, remaining connections are closed.
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
	// PauseAccept stops accepting new connections. Existing connections continue.
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
// counters. Each engine maintains internal atomic counters and populates a
// fresh snapshot on each [Engine.Metrics] call.
//
// v1.5.0: LatencyP50 / LatencyP99 / LatencyP999 were removed (celeris#321).
// The fields were declared but never written by any sub-engine (the
// sub-engines don't track per-request latency histograms — they only
// count requests). The adaptive engine's aggregator at
// adaptive/engine.go was the only reader and it received zeros from
// both sub-engines, so removing the fields changes nothing observable.
// SyscallRate, referenced by the issue, never existed in the tree.
type EngineMetrics struct { //nolint:revive // user-approved name
	// RequestCount is the cumulative number of requests handled by this engine.
	RequestCount uint64
	// ActiveConnections is the current number of open connections.
	ActiveConnections int64
	// ErrorCount is the cumulative number of connection-level or protocol errors.
	ErrorCount uint64
	// Throughput is the recent requests-per-second rate.
	Throughput float64
	// AsyncRoutes is the count of routes registered with .Async(true) on
	// this engine's handler. Static after Listen — derived from the
	// router's per-route async flags and exposed for diagnostics so
	// operators can see how many handlers run on the per-conn dispatch
	// goroutine vs inline on the worker. Zero on engines whose handler
	// does not implement [github.com/goceleris/celeris/protocol/h2/stream.AsyncRouteResolver].
	AsyncRoutes int
	// AsyncPromotedConns is the cumulative number of connections that
	// have been promoted from inline-on-worker to the per-conn dispatch
	// goroutine via per-handler async (celeris #300). Counts promotions,
	// not currently-promoted conns — every fresh async-mode connection
	// that hits an async route increments this once. Useful to observe
	// how often the inline → goroutine handoff fires vs the pure-sync
	// inline fast path.
	AsyncPromotedConns uint64
	// Workers is the number of I/O workers (io_uring) or event loops
	// (epoll) the engine is running. Static after Listen. The adaptive
	// controller divides ActiveConnections by it to derive the
	// conns-per-worker load signal that drives engine selection.
	Workers int
	// AcceptCount is the cumulative number of connections accepted by this
	// engine since start. Together with elapsed time it yields the accept
	// rate (new-connection arrival rate) used as a secondary load signal.
	AcceptCount uint64
	// CloseCount is the cumulative number of connections closed by this
	// engine since start. AcceptCount - CloseCount tracks the live count;
	// a high close rate relative to accepts indicates short-lived
	// churn-style connections.
	CloseCount uint64
	// BytesRead is the cumulative number of payload bytes received from
	// the network across all connections. Used with BytesWritten and
	// RequestCount to derive the average bytes-per-request signal that
	// suppresses io_uring selection for link-bound (large-payload)
	// workloads where the engines tie.
	BytesRead uint64
	// BytesWritten is the cumulative number of payload bytes sent to the
	// network across all connections. See BytesRead.
	BytesWritten uint64
	// AdaptiveSwitches is the cumulative count of completed epoll⇄io_uring
	// switches performed by the adaptive engine. Zero for non-adaptive
	// engines, which never switch. Live switching is the adaptive engine's
	// most complex path; surfacing the count lets ops and benchmarks
	// correlate a throughput or tail-latency anomaly with switching activity
	// (a rare switch transient can skew a single benchmark pass).
	AdaptiveSwitches uint64
}
