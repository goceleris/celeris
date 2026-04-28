package engine

import (
	"context"
	"net"
	"time"
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

// EngineMetrics is a point-in-time snapshot of engine-level performance
// counters. Each engine maintains internal atomic counters and populates a
// fresh snapshot on each [Engine.Metrics] call.
type EngineMetrics struct { //nolint:revive // user-approved name
	// RequestCount is the cumulative number of requests handled by this engine.
	RequestCount uint64
	// ActiveConnections is the current number of open connections.
	ActiveConnections int64
	// ErrorCount is the cumulative number of connection-level or protocol errors.
	ErrorCount uint64
	// Throughput is the recent requests-per-second rate.
	Throughput float64
	// LatencyP50 is the 50th-percentile (median) request latency.
	LatencyP50 time.Duration
	// LatencyP99 is the 99th-percentile request latency.
	LatencyP99 time.Duration
	// LatencyP999 is the 99.9th-percentile request latency.
	LatencyP999 time.Duration
}
