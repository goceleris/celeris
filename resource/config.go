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
	if c.Protocol.IsDefault() {
		c.Protocol = engine.Auto
	}
	if c.MaxFrameSize == 0 {
		c.MaxFrameSize = 16384
	}
	if c.InitialWindowSize == 0 {
		c.InitialWindowSize = 65535
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
	switch {
	case c.ReadTimeout == 0:
		c.ReadTimeout = 300 * time.Second
	case c.ReadTimeout < 0:
		c.ReadTimeout = 0 // -1 → no timeout
	}
	switch {
	case c.WriteTimeout == 0:
		c.WriteTimeout = 300 * time.Second
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
