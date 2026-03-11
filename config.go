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
	// HTTP1 selects HTTP/1.1 only.
	HTTP1 Protocol = Protocol(engine.HTTP1)
	// H2C selects HTTP/2 cleartext (h2c) only.
	H2C Protocol = Protocol(engine.H2C)
	// Auto enables automatic protocol detection between HTTP/1.1 and H2C.
	Auto Protocol = Protocol(engine.Auto)
)

// String returns the protocol name.
func (p Protocol) String() string { return engine.Protocol(p).String() }

// EngineType identifies which I/O engine implementation is in use.
type EngineType engine.EngineType

const (
	// IOUring uses Linux io_uring for asynchronous I/O (Linux 5.10+ required).
	IOUring EngineType = EngineType(engine.IOUring)
	// Epoll uses Linux edge-triggered epoll for I/O (Linux only).
	Epoll EngineType = EngineType(engine.Epoll)
	// Adaptive dynamically switches between IOUring and Epoll based on load (Linux only).
	Adaptive EngineType = EngineType(engine.Adaptive)
	// Std uses Go's net/http standard library server (all platforms).
	Std EngineType = EngineType(engine.Std)
)

// String returns the engine type name.
func (t EngineType) String() string { return engine.EngineType(t).String() }

// Objective selects a tuning profile that controls I/O and networking parameters.
type Objective resource.ObjectiveProfile

const (
	// Balanced targets a balance between latency and throughput (default).
	Balanced Objective = Objective(resource.BalancedObjective)
	// Latency optimizes for minimum response latency.
	Latency Objective = Objective(resource.LatencyOptimized)
	// Throughput optimizes for maximum requests per second.
	Throughput Objective = Objective(resource.ThroughputOptimized)
)

// String returns the objective name.
func (o Objective) String() string { return resource.ObjectiveProfile(o).String() }

// Config holds the public server configuration.
type Config struct {
	// Addr is the TCP address to listen on (e.g. ":8080").
	Addr string
	// Protocol is the HTTP protocol version (default HTTP1).
	Protocol Protocol
	// Engine is the I/O engine (default Std; IOUring, Epoll, Adaptive require Linux).
	Engine EngineType

	// Workers is the number of I/O worker goroutines (default GOMAXPROCS).
	Workers int
	// Objective is the tuning profile (default Balanced).
	Objective Objective

	// ReadTimeout is the max duration for reading the entire request (zero = no timeout).
	ReadTimeout time.Duration
	// WriteTimeout is the max duration for writing the response (zero = no timeout).
	WriteTimeout time.Duration
	// IdleTimeout is the max duration a keep-alive connection may be idle (zero = no timeout).
	IdleTimeout time.Duration
	// ShutdownTimeout is the max duration to wait for in-flight requests during
	// graceful shutdown via StartWithContext (default 30s).
	ShutdownTimeout time.Duration

	// MaxFormSize is the maximum memory used for multipart form parsing
	// (default 32 MB). Set to -1 to disable the limit.
	MaxFormSize int64

	// DisableMetrics disables the built-in metrics collector. When true,
	// [Server.Collector] returns nil and per-request metric recording is skipped.
	// Default false (metrics enabled).
	DisableMetrics bool

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
		Addr:         c.Addr,
		Protocol:     engine.Protocol(c.Protocol),
		Engine:       engine.EngineType(c.Engine),
		ReadTimeout:  c.ReadTimeout,
		WriteTimeout: c.WriteTimeout,
		IdleTimeout:  c.IdleTimeout,
		Logger:       c.Logger,
	}

	if c.Workers > 0 {
		rc.Resources.Workers = c.Workers
	}

	rc.Objective = resource.ObjectiveProfile(c.Objective)

	return rc
}
