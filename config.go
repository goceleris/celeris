package celeris

import (
	"log/slog"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/resource"
)

// Protocol represents the HTTP protocol version.
type Protocol engine.Protocol

const (
	// Auto enables automatic protocol detection between HTTP/1.1 and H2C (default).
	Auto Protocol = Protocol(engine.Auto)
	// HTTP1 selects HTTP/1.1 only.
	HTTP1 Protocol = Protocol(engine.HTTP1)
	// H2C selects HTTP/2 cleartext (h2c) only.
	H2C Protocol = Protocol(engine.H2C)
)

// String returns the protocol name.
func (p Protocol) String() string { return engine.Protocol(p).String() }

// EngineType identifies which I/O engine implementation is in use.
type EngineType engine.EngineType

const (
	// Adaptive dynamically switches between Epoll and IOUring based on load (default on Linux).
	Adaptive EngineType = EngineType(engine.Adaptive)
	// Epoll uses Linux edge-triggered epoll for I/O (Linux only).
	Epoll EngineType = EngineType(engine.Epoll)
	// IOUring uses Linux io_uring for asynchronous I/O (Linux 5.10+ required).
	IOUring EngineType = EngineType(engine.IOUring)
	// Std uses Go's net/http standard library server (all platforms).
	Std EngineType = EngineType(engine.Std)
)

// String returns the engine type name.
func (t EngineType) String() string { return engine.EngineType(t).String() }

// Config holds the public server configuration.
type Config struct {
	// Addr is the TCP address to listen on (e.g. ":8080").
	Addr string
	// Protocol is the HTTP protocol version (default Auto — auto-detect H1/H2).
	Protocol Protocol
	// Engine is the I/O engine (default Adaptive on Linux, Std elsewhere).
	Engine EngineType

	// Workers is the number of I/O worker goroutines (default GOMAXPROCS).
	Workers int

	// ReadTimeout is the max duration for reading the entire request.
	// Zero uses the default (60s). Set to -1 for no timeout.
	ReadTimeout time.Duration
	// ReadHeaderTimeout caps the read of just the request line + headers
	// (separately from the request body, which ReadTimeout covers). This
	// is the canonical slow-loris defence: clients that drip headers one
	// byte at a time get killed in seconds instead of holding a worker
	// + listener-backlog slot for the full ReadTimeout window. Zero uses
	// the default (10s); -1 disables.
	//
	// The std engine wires this to http.Server.ReadHeaderTimeout; the
	// iouring/epoll engines enforce the same budget inside their H1
	// header read loop.
	ReadHeaderTimeout time.Duration
	// WriteTimeout is the max duration for writing the response.
	// Zero uses the default (60s). Set to -1 for no timeout.
	WriteTimeout time.Duration
	// IdleTimeout is the max duration a keep-alive connection may be idle.
	// Zero uses the default (600s). Set to -1 for no timeout.
	IdleTimeout time.Duration
	// ShutdownTimeout is the max duration to wait for in-flight requests during
	// graceful shutdown via StartWithContext (default 30s).
	ShutdownTimeout time.Duration

	// MaxFormSize is the maximum memory used for multipart form parsing
	// (default 32 MB). Set to -1 to disable the limit.
	MaxFormSize int64

	// MaxRequestBodySize is the maximum allowed request body size in bytes
	// across all protocols (H1, H2, bridge). 0 uses the default (100 MB).
	// Set to -1 to disable the limit (unlimited).
	MaxRequestBodySize int64

	// MaxConcurrentStreams limits simultaneous H2 streams per connection (default 100).
	MaxConcurrentStreams uint32
	// MaxFrameSize is the max H2 frame payload size (default 16384, range 16384-16777215).
	MaxFrameSize uint32
	// InitialWindowSize is the H2 initial stream window size (default 65535).
	InitialWindowSize uint32
	// MaxHeaderBytes is the max header block size (default 16 MB, min 4096).
	MaxHeaderBytes int

	// DisableKeepAlive disables HTTP keep-alive; each request gets its own connection.
	DisableKeepAlive bool
	// BufferSize is the per-connection I/O buffer size in bytes (0 = engine default).
	BufferSize int
	// SocketRecvBuf sets SO_RCVBUF for accepted connections (0 = OS default).
	SocketRecvBuf int
	// SocketSendBuf sets SO_SNDBUF for accepted connections (0 = OS default).
	SocketSendBuf int
	// MaxConns is the max simultaneous connections per worker (0 = unlimited).
	MaxConns int

	// DisableMetrics disables the built-in metrics collector. When true,
	// [Server.Collector] returns nil and per-request metric recording is skipped.
	// Default false (metrics enabled).
	DisableMetrics bool

	// AsyncHandlers dispatches HTTP handlers to spawned goroutines instead of
	// running them inline on the engine's LockOSThread'd worker. Enabling
	// this is the right choice when handlers do blocking I/O (DB drivers,
	// external HTTP calls, file reads): the worker returns to epoll_wait /
	// io_uring_enter while the handler blocks, so the per-worker serialization
	// ceiling (NumWorkers × 1/RTT) is replaced by goroutine-per-connection
	// parallelism, matching net/http's concurrency model.
	//
	// When AsyncHandlers is set, celeris drivers opened WithEngine(srv) auto-
	// select their direct net.Conn path (Go netpoll parks handler Gs on
	// EPOLLIN without blocking an M), validated on MSR1 at celeris-epoll +
	// celerismc jumping from 64k → 105k rps.
	//
	// The cost on pure-CPU handlers is a goroutine spawn per request (~100ns)
	// plus scheduler overhead — measured regression on a static-response
	// bench is ~3–5%. Keep AsyncHandlers false for latency-critical CPU-only
	// workloads; enable it for any workload that touches a DB, cache, or
	// upstream service.
	//
	// AsyncHandlers is the SERVER-LEVEL default. Individual routes and
	// groups can override it per handler with [Route.Async] /
	// [RouteGroup.Async] (most-specific wins: route > group > this
	// default), so a server with mostly CPU routes + a few DB routes can
	// keep this false and mark just the DB routes .Async(), or set this
	// true and mark hot CPU routes .Async(false). On HTTP/2 the override
	// is honored per-stream (sync routes run inline on the event loop,
	// async routes dispatch to the worker pool). Note: celeris drivers
	// opened WithEngine(srv) consult the server-level AsyncHandlers (not
	// per-route overrides) for their auto-async path selection — set this
	// true when using WithEngine drivers under per-route async.
	//
	// Default: false.
	AsyncHandlers bool

	// OnExpectContinue is called when an H1 request contains "Expect: 100-continue".
	// If the callback returns false, the server responds with 417 Expectation Failed
	// and skips reading the body. If nil, the server always sends 100 Continue.
	OnExpectContinue func(method, path string, headers [][2]string) bool

	// OnConnect is called when a new connection is accepted. The addr is the
	// remote peer address. Must be fast — blocks the event loop.
	OnConnect func(addr string)
	// OnDisconnect is called when a connection is closed. The addr is the
	// remote peer address. Must be fast — blocks the event loop.
	OnDisconnect func(addr string)

	// TrustedProxies is a list of trusted proxy CIDR ranges (e.g., "10.0.0.0/8",
	// "172.16.0.0/12", "192.168.0.0/16"). When set, ClientIP() only trusts
	// X-Forwarded-For hops from these networks. When empty, ClientIP() trusts
	// all proxy headers (legacy behavior).
	TrustedProxies []string

	// Logger is the structured logger (default slog.Default()).
	Logger *slog.Logger

	// EnableH2Upgrade controls whether the server honors RFC 7540 §3.2
	// "HTTP/1.1 Upgrade: h2c" requests, promoting an HTTP/1 connection to
	// cleartext HTTP/2 on the original request's handler (dispatched on
	// stream 1). Resolution follows three cases:
	//   - nil (default): inferred from Protocol — enabled for Auto,
	//     disabled for H2C (clients already speak H2 directly) and for
	//     HTTP1 (upgrade is irrelevant, no H2 stack available).
	//   - non-nil true: force enabled. Useful to opt into upgrade on
	//     Protocol=H2C for clients that prefer to negotiate.
	//   - non-nil false: force disabled, even on Protocol=Auto. Useful when
	//     the engine intentionally only serves HTTP/1.
	EnableH2Upgrade *bool

	// WorkerScaling configures the dynamic worker scaler. As of v1.4.6
	// the scaler is DEFAULT-ON — leaving this field nil resolves to a
	// zero-value [resource.WorkerScalingConfig], which activates the
	// scaler with the data-validated defaults (Strategy=StartHigh,
	// MinActive=max(2, NumCPU/2), TargetConnsPerWorker=20,
	// Interval=250ms, ScaleUpStep=2, ScaleDownStep=1,
	// ScaleDownHysteresis=1, ScaleDownIdleTicks=4). This matches the
	// "just-works" public design intent already in place for the
	// Engine=Adaptive and Protocol=Auto defaults.
	//
	// Pre-v1.4.6 behaviour (scaler always disabled unless explicitly
	// configured) is achievable by passing a non-nil struct that
	// effectively makes the scaler a no-op:
	//
	//	WorkerScaling: &resource.WorkerScalingConfig{
	//	    MinActive: runtime.GOMAXPROCS(0), // pin at NumCPU; never scale down
	//	}
	//
	// Set to a non-nil pointer with custom values to override one or
	// more defaults. See [resource.WorkerScalingConfig] for tuning.
	// The scaler keeps connections-per-active-worker around the target
	// ratio by pausing/resuming workers; this dramatically improves
	// CQE/event batching at low/mid concurrency where the static
	// numCPU default would otherwise lose 30-90 % CPU/req to under-
	// batched syscalls. See PR #257 / issue #281 for the full
	// rationale and benchmark data.
	WorkerScaling *resource.WorkerScalingConfig
}

// EngineMetrics is a point-in-time snapshot of engine-level performance counters.
type EngineMetrics = engine.EngineMetrics

// EngineInfo provides read-only information about the running engine.
type EngineInfo struct {
	// Type identifies the active I/O engine (IOUring, Epoll, Adaptive, or Std).
	Type EngineType
	// Metrics is a point-in-time snapshot of engine-level performance counters.
	Metrics EngineMetrics
}

func (c Config) toResourceConfig() resource.Config {
	rc := resource.Config{
		Addr:                 c.Addr,
		Protocol:             engine.Protocol(c.Protocol),
		Engine:               engine.EngineType(c.Engine),
		ReadTimeout:          c.ReadTimeout,
		ReadHeaderTimeout:    c.ReadHeaderTimeout,
		WriteTimeout:         c.WriteTimeout,
		IdleTimeout:          c.IdleTimeout,
		MaxHeaderBytes:       c.MaxHeaderBytes,
		MaxConcurrentStreams: c.MaxConcurrentStreams,
		MaxFrameSize:         c.MaxFrameSize,
		InitialWindowSize:    c.InitialWindowSize,
		DisableKeepAlive:     c.DisableKeepAlive,
		Logger:               c.Logger,
	}

	if c.Workers > 0 {
		rc.Resources.Workers = c.Workers
	}
	if c.BufferSize > 0 {
		rc.Resources.BufferSize = c.BufferSize
	}
	if c.SocketRecvBuf > 0 {
		rc.Resources.SocketRecv = c.SocketRecvBuf
	}
	if c.SocketSendBuf > 0 {
		rc.Resources.SocketSend = c.SocketSendBuf
	}
	if c.MaxConns > 0 {
		rc.Resources.MaxConns = c.MaxConns
	}

	rc.MaxRequestBodySize = c.MaxRequestBodySize
	rc.AsyncHandlers = c.AsyncHandlers
	rc.OnExpectContinue = c.OnExpectContinue
	rc.OnConnect = c.OnConnect
	rc.OnDisconnect = c.OnDisconnect
	// Dynamic worker scaler default-on (issue #281). A nil
	// WorkerScaling field — i.e. the user did not configure it — now
	// resolves to a zero-value struct so the scaler activates with
	// the data-validated defaults. Opt-out is documented as setting
	// MinActive=NumCPU (no-op scaler), which preserves backward
	// compatibility for users who really do want pre-v1.4.6 behaviour.
	if c.WorkerScaling != nil {
		rc.WorkerScaling = c.WorkerScaling
	} else {
		rc.WorkerScaling = &resource.WorkerScalingConfig{}
	}

	// h2c upgrade resolution. Nil → protocol-dependent default (Auto → true,
	// HTTP1/H2C → false). Non-nil → user override honored verbatim.
	if c.EnableH2Upgrade != nil {
		rc.EnableH2Upgrade = *c.EnableH2Upgrade
	} else {
		p := engine.Protocol(c.Protocol)
		rc.EnableH2Upgrade = p.IsDefault() || p == engine.Auto
	}

	return rc
}
