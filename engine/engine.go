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

// WorkerScaler is implemented by engines that support per-worker pause/resume
// for dynamic capacity adjustment based on load. Used by the higher-level
// scaler in the adaptive engine to delegate worker activation to whichever
// sub-engine is currently active. Per-worker pause is asynchronous — the
// worker drains in-flight connections before going SUSPENDED. Resume wakes a
// suspended worker so it re-creates its listen socket and rejoins the
// SO_REUSEPORT group.
type WorkerScaler interface {
	// NumWorkers returns the total worker pool size (max active count).
	NumWorkers() int
	// PauseWorker deactivates worker i. Asynchronous; returns immediately.
	// Idempotent — pausing an already-paused worker is a no-op.
	PauseWorker(i int)
	// ResumeWorker reactivates worker i. Wakes the worker if SUSPENDED.
	// Idempotent — resuming an active worker is a no-op.
	ResumeWorker(i int)
}

// SendfileCapable is an optional interface implemented by engines that
// support zero-copy file responses via sendfile(2) (epoll) or the io_uring
// equivalent. The response adapter type-asserts; engines that do not
// implement it fall back to read+write. See celeris#317.
//
// The fdOut argument is the engine's per-connection socket FD; the
// caller is responsible for the connState lifecycle (locking, dirty-list
// membership, timeout tracking) — the engine's sendfile path is purely
// a syscall shim.
//
// offset and length describe the slice of the source file to send.
// length ≤ 0 means "send to EOF." The headers slice is flushed via
// write(2) before the sendfile loop; engines that can fuse headers
// into a single iovec may do so.
//
// Returns the number of body bytes sent (0 on error) and an error.
// EAGAIN/EWOULDBLOCK are surfaced as errors for the caller to defer.
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
}
