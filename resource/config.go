package resource

import (
	"fmt"
	"log/slog"
	"net"
	"runtime"
	"time"

	"github.com/goceleris/celeris/engine"
)

// defaultEngine returns Adaptive on Linux and Std on other platforms.
func defaultEngine() engine.EngineType {
	if runtime.GOOS == "linux" {
		return engine.Adaptive
	}
	return engine.Std
}

// Config holds the internal server configuration used by engine implementations.
// Users typically interact with the top-level celeris.Config instead.
type Config struct {
	// Protocol is the HTTP protocol version (HTTP1, H2C, or Auto).
	Protocol engine.Protocol
	// Engine is the I/O engine type (IOUring, Epoll, Adaptive, or Std).
	Engine engine.EngineType
	// Addr is the TCP address to listen on (e.g. ":8080").
	Addr string
	// Resources holds worker, buffer, and connection limit overrides.
	Resources Resources
	// MaxHeaderBytes is the max header block size in bytes (min 4096 if set).
	MaxHeaderBytes int
	// MaxConcurrentStreams limits simultaneous H2 streams per connection.
	MaxConcurrentStreams uint32
	// MaxFrameSize is the max H2 frame payload size (range 16384-16777215).
	MaxFrameSize uint32
	// InitialWindowSize is the H2 initial stream flow-control window size.
	InitialWindowSize uint32
	// ReadTimeout is the max duration for reading the entire request.
	ReadTimeout time.Duration
	// WriteTimeout is the max duration for writing the response.
	WriteTimeout time.Duration
	// IdleTimeout is the max duration a keep-alive connection may be idle.
	IdleTimeout time.Duration
	// DisableKeepAlive disables HTTP keep-alive.
	DisableKeepAlive bool
	// Listener is an optional pre-existing listener for socket inheritance.
	Listener net.Listener
	// MaxRequestBodySize is the maximum allowed request body size in bytes.
	// 0 uses the default (100 MB). -1 disables the limit (unlimited).
	MaxRequestBodySize int64
	// AsyncHandlers dispatches HTTP handlers to spawned goroutines instead
	// of inline execution on LockOSThread'd workers. See celeris.Config.
	AsyncHandlers bool
	// OnExpectContinue is called when an H1 request contains "Expect: 100-continue".
	// If the callback returns false, the server responds with 417 Expectation Failed
	// and skips reading the request body. If nil, the server always sends 100 Continue.
	OnExpectContinue func(method, path string, headers [][2]string) bool
	// OnConnect is called when a new connection is accepted.
	OnConnect func(addr string)
	// OnDisconnect is called when a connection is closed.
	OnDisconnect func(addr string)
	// Logger is the structured logger for engine diagnostics (default slog.Default()).
	Logger *slog.Logger
	// EnableH2Upgrade enables RFC 7540 §3.2 HTTP/1.1→H2C upgrades. Resolved
	// from celeris.Config.EnableH2Upgrade (pointer, may be nil) and Protocol.
	// Always a concrete value after WithDefaults.
	EnableH2Upgrade bool
	// SkipBuiltinScaler suppresses the per-engine dynamic worker scaler
	// loop. Set by the adaptive engine when it constructs its sub-engines —
	// adaptive runs ONE higher-level scaler that delegates to the active
	// sub-engine, so the iouring + epoll built-in scalers must stay quiet
	// to avoid two scalers fighting over the same worker pool.
	SkipBuiltinScaler bool
	// WorkerScaling configures the dynamic worker scaler. Nil disables the
	// scaler (default — workers stays at Resources.Workers and never adapts).
	// When set, the scaler activates and deactivates workers based on
	// observed load to keep CQE/event batching density in the sweet spot.
	// See WorkerScalingConfig for tuning details.
	WorkerScaling *WorkerScalingConfig
}

// WorkerScalingStrategy selects the seed strategy for the dynamic
// worker scaler. The zero value (ScalingStrategyStartHigh) is the
// recommended default: start at NumWorkers active, scale down once
// load is observably low. This preserves SO_REUSEPORT distribution at
// startup, which the spike-B sweep showed is dramatically better on
// ramp / oscil traffic patterns (+34-78 % across all three engines).
type WorkerScalingStrategy int

const (
	// ScalingStrategyStartHigh seeds the scaler at NumWorkers active.
	// Best for production where traffic ramps from idle and bursts.
	// Zero value — selected when the field is unset.
	ScalingStrategyStartHigh WorkerScalingStrategy = 0
	// ScalingStrategyStartLow seeds the scaler at MinActive. Best when
	// the application has a long idle warmup before any conns arrive
	// and saving CPU during that idle period matters more than peak
	// throughput on the first burst.
	ScalingStrategyStartLow WorkerScalingStrategy = 1
)

// WorkerScalingConfig controls the dynamic worker scaler used by the
// iouring, epoll, and adaptive engines. Zero values mean "use the
// scaler's default" — see field comments for what those are. Pass via
// celeris.Config.WorkerScaling to enable; nil disables the scaler
// entirely (the engine runs all configured workers all the time, like
// versions before the scaler was introduced).
//
// The scaler tracks the engine's activeConns counter and adjusts the
// number of "active" workers (workers participating in the SO_REUSEPORT
// group) so that conns / active is roughly TargetConnsPerWorker. Scale-up
// is reactive (next tick after a load increase). Scale-down is hysteretic
// — must observe ScaleDownIdleTicks consecutive ticks below the
// hysteresis threshold before reducing one worker.
type WorkerScalingConfig struct {
	// Strategy picks the seed-state strategy. Zero value is
	// ScalingStrategyStartHigh, which is the data-validated default for
	// most production workloads. See WorkerScalingStrategy for tuning.
	Strategy WorkerScalingStrategy
	// MinActive is the floor on the active worker count. The scaler will
	// never reduce active workers below this. Defaults to max(2, NumCPU/2).
	// Set to NumCPU to force the scaler to always run at full capacity
	// (effectively a static-w=NumCPU configuration).
	MinActive int
	// TargetConnsPerWorker is the active-worker scaling target. The scaler
	// computes desired = ceil(activeConns / TargetConnsPerWorker), clamps
	// to [MinActive, NumWorkers], and steers active toward that. Default 20.
	// Higher values keep more conns per worker (better batching, less
	// parallelism). Lower values prefer parallelism over batching.
	TargetConnsPerWorker int
	// Interval controls how often the scaler reevaluates active count.
	// Default 250ms. Lower values respond to load changes faster but burn
	// more CPU on the controller goroutine.
	Interval time.Duration
	// ScaleUpStep is the maximum number of workers the scaler will
	// resume per tick. Default 2 — wider bursts disrupt SO_REUSEPORT
	// load balancing more than they help. Bigger values are tempting on
	// SPIKE workloads but produce worse throughput per the v1.4.1
	// SPIKE-test sweep (upStep=4 and upStep=8 both lost to upStep=2).
	ScaleUpStep int
	// ScaleDownStep is the maximum number of workers the scaler will
	// pause per tick when load drops. Default 1 — scale-down too quickly
	// and you can't recover throughput when load comes back.
	ScaleDownStep int
	// ScaleDownHysteresis adds a buffer between desired and active
	// before scale-down fires: scale-down only if desired ≤ active -
	// ScaleDownHysteresis - 1. Default 1 (so a desired-of-N triggers
	// scale-down only when active is N+2 or higher).
	ScaleDownHysteresis int
	// ScaleDownIdleTicks is how many consecutive sub-threshold ticks
	// must pass before a single scale-down step fires. Default 4
	// (= 1 second at the default 250ms interval). Tunes how patient
	// the scaler is about temporary lulls: a request-rate dip of one
	// tick will not trigger scale-down.
	ScaleDownIdleTicks int
	// Trace logs every scaler decision (active, desired, idle_ticks).
	// Default false. Use when diagnosing scaling behaviour.
	Trace bool
}

// Validate checks all config fields and returns any validation errors.
func (c Config) Validate() []error {
	var errs []error

	if c.Addr != "" {
		_, port, err := net.SplitHostPort(c.Addr)
		if err != nil {
			errs = append(errs, fmt.Errorf("invalid addr %q: %w", c.Addr, err))
		} else {
			var p int
			if _, err := fmt.Sscanf(port, "%d", &p); err != nil || p < 0 || p > 65535 {
				errs = append(errs, fmt.Errorf("port must be 0-65535, got %q", port))
			}
		}
	}

	if c.MaxFrameSize != 0 && (c.MaxFrameSize < 16384 || c.MaxFrameSize > 16777215) {
		errs = append(errs, fmt.Errorf("maxFrameSize must be 16384-16777215, got %d", c.MaxFrameSize))
	}

	if c.InitialWindowSize > 2147483647 {
		errs = append(errs, fmt.Errorf("initialWindowSize must be 0-2147483647, got %d", c.InitialWindowSize))
	}

	if c.MaxConcurrentStreams > 0x7fffffff {
		errs = append(errs, fmt.Errorf("maxConcurrentStreams must be <= 2147483647, got %d", c.MaxConcurrentStreams))
	}

	if c.MaxHeaderBytes != 0 && c.MaxHeaderBytes < 4096 {
		errs = append(errs, fmt.Errorf("maxHeaderBytes must be >= 4096 if set, got %d", c.MaxHeaderBytes))
	}

	if c.Resources.Workers != 0 && c.Resources.Workers < MinWorkers {
		errs = append(errs, fmt.Errorf("workers must be >= %d if set, got %d", MinWorkers, c.Resources.Workers))
	}

	if c.Resources.BufferSize != 0 && c.Resources.BufferSize < MinBufferSize {
		errs = append(errs, fmt.Errorf("bufferSize must be >= %d if set, got %d", MinBufferSize, c.Resources.BufferSize))
	}

	if c.ReadTimeout < -1 {
		errs = append(errs, fmt.Errorf("readTimeout must be >= -1, got %v", c.ReadTimeout))
	}
	if c.WriteTimeout < -1 {
		errs = append(errs, fmt.Errorf("writeTimeout must be >= -1, got %v", c.WriteTimeout))
	}
	if c.IdleTimeout < -1 {
		errs = append(errs, fmt.Errorf("idleTimeout must be >= -1, got %v", c.IdleTimeout))
	}

	if runtime.GOOS != "linux" {
		if c.Engine == engine.IOUring || c.Engine == engine.Epoll {
			errs = append(errs, fmt.Errorf("engine %s requires Linux", c.Engine))
		}
		if c.Engine == engine.Adaptive {
			errs = append(errs, fmt.Errorf("engine adaptive requires Linux"))
		}
	}

	// Listener + explicit Addr with a concrete non-zero port is
	// ambiguous — the runtime silently prefers Listener.Addr().
	// Warn so the caller notices the discard at config time rather
	// than observing a mismatched port in logs. Allow "<host>:0"
	// (pick-any-port) since it's a common pattern when the caller
	// intentionally delegates port selection to the pre-bound listener.
	if c.Listener != nil && c.Addr != "" && c.Addr != ":8080" {
		if _, port, splitErr := net.SplitHostPort(c.Addr); splitErr == nil && port != "0" {
			if lnAddr := c.Listener.Addr().String(); lnAddr != c.Addr {
				errs = append(errs, fmt.Errorf(
					"ambiguous configuration: Addr=%q but Listener is bound to %q; the explicit Addr will be discarded",
					c.Addr, lnAddr))
			}
		}
	}

	return errs
}

// WithDefaults returns a copy of Config with zero-value fields set to sensible defaults.
func (c Config) WithDefaults() Config {
	if c.Addr == "" {
		c.Addr = ":8080"
	}
	if c.Engine.IsDefault() {
		c.Engine = defaultEngine()
	}
	// Resolve h2c-upgrade default. Auto protocol (including the implicit
	// default) enables h2c upgrade; HTTP1/H2C don't unless the caller
	// explicitly set EnableH2Upgrade=true before calling WithDefaults.
	// Callers who want upgrade disabled on Auto must go through the root
	// celeris.Config path where EnableH2Upgrade is a *bool.
	wasAutoOrDefault := c.Protocol.IsDefault() || c.Protocol == engine.Auto
	if c.Protocol.IsDefault() {
		c.Protocol = engine.Auto
	}
	if wasAutoOrDefault && !c.EnableH2Upgrade {
		c.EnableH2Upgrade = true
	}
	if c.MaxFrameSize == 0 {
		// 1 MiB matches golang.org/x/net/http2's defaultMaxReadFrameSize
		// and fasthttp2 / hertz. RFC 7540 §4.2 permits up to 16 MiB. The
		// 16 KiB default previously rejected 32 KiB+ DATA frames from
		// clients that pre-negotiate their send size (Go std http2
		// client, loadgen, browser upload flows over H2).
		c.MaxFrameSize = 1 << 20
	}
	if c.InitialWindowSize == 0 {
		// 1 MiB matches golang.org/x/net/http2 and fasthttp2: a 64 KiB +
		// one-byte body POST would stall on the default 65 535-byte
		// window because the server's WINDOW_UPDATE lands only after it
		// finishes reading the full body. RFC 7540 allows up to 2^31-1.
		c.InitialWindowSize = 1 << 20
	}
	if c.MaxConcurrentStreams == 0 {
		c.MaxConcurrentStreams = 100
	}
	if c.MaxHeaderBytes == 0 {
		c.MaxHeaderBytes = 16 << 20
	}
	switch {
	case c.MaxRequestBodySize == 0:
		c.MaxRequestBodySize = 100 << 20 // 100 MB
	case c.MaxRequestBodySize < 0:
		c.MaxRequestBodySize = 0 // 0 internally means unlimited
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	// Read/Write defaults. Previous 300s was too permissive for
	// a latency-focused engine — a slow-loris client could hold a
	// worker M / fd for 5 minutes before eviction. 60s matches
	// nginx's client_header_timeout / client_body_timeout and
	// covers legitimate slow-network cases. Users who need longer
	// (streaming uploads, big downloads) should set explicit values.
	switch {
	case c.ReadTimeout == 0:
		c.ReadTimeout = 60 * time.Second
	case c.ReadTimeout < 0:
		c.ReadTimeout = 0 // -1 → no timeout
	}
	switch {
	case c.WriteTimeout == 0:
		c.WriteTimeout = 60 * time.Second
	case c.WriteTimeout < 0:
		c.WriteTimeout = 0 // -1 → no timeout
	}
	switch {
	case c.IdleTimeout == 0:
		c.IdleTimeout = 600 * time.Second
	case c.IdleTimeout < 0:
		c.IdleTimeout = 0 // -1 → no timeout
	}
	return c
}
