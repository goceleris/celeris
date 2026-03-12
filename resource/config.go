package resource

import (
	"fmt"
	"log/slog"
	"net"
	"runtime"
	"time"

	"github.com/goceleris/celeris/engine"
)

// Config holds server configuration including protocol, engine, address, and resource settings.
type Config struct {
	Protocol             engine.Protocol
	Engine               engine.EngineType
	Addr                 string
	Resources            Resources
	Objective            ObjectiveProfile
	MaxHeaderBytes       int
	MaxConcurrentStreams uint32
	MaxFrameSize         uint32
	InitialWindowSize    uint32
	ReadTimeout          time.Duration
	WriteTimeout         time.Duration
	IdleTimeout          time.Duration
	DisableKeepAlive     bool
	Listener             net.Listener
	Logger               *slog.Logger
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
