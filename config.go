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
	// Zero uses the default (300s). Set to -1 for no timeout.
	ReadTimeout time.Duration
	// WriteTimeout is the max duration for writing the response.
	// Zero uses the default (300s). Set to -1 for no timeout.
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
	rc.OnExpectContinue = c.OnExpectContinue
	rc.OnConnect = c.OnConnect
	rc.OnDisconnect = c.OnDisconnect

	return rc
}
