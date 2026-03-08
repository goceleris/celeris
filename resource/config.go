package resource

import (
	"fmt"
	"log/slog"
	"net"
	"runtime"
	"time"

	"github.com/goceleris/celeris/engine"
)

type Config struct {
	Protocol            engine.Protocol
	Engine              engine.EngineType
	Addr                string
	Resources           Resources
	Objective           ObjectiveProfile
	MaxHeaderBytes      int
	MaxConcurrentStreams uint32
	MaxFrameSize        uint32
	InitialWindowSize   uint32
	ReadTimeout         time.Duration
	WriteTimeout        time.Duration
	IdleTimeout         time.Duration
	DisableKeepAlive    bool
	Logger              *slog.Logger
}

func (c Config) Validate() []error {
	var errs []error

	if c.Addr != "" {
		_, port, err := net.SplitHostPort(c.Addr)
		if err != nil {
			errs = append(errs, fmt.Errorf("invalid addr %q: %w", c.Addr, err))
		} else {
			var p int
			if _, err := fmt.Sscanf(port, "%d", &p); err != nil || p < 1 || p > 65535 {
				errs = append(errs, fmt.Errorf("port must be 1-65535, got %q", port))
			}
		}
	}

	if c.MaxFrameSize != 0 && (c.MaxFrameSize < 16384 || c.MaxFrameSize > 16777215) {
		errs = append(errs, fmt.Errorf("MaxFrameSize must be 16384-16777215, got %d", c.MaxFrameSize))
	}

	if c.InitialWindowSize > 2147483647 {
		errs = append(errs, fmt.Errorf("InitialWindowSize must be 0-2147483647, got %d", c.InitialWindowSize))
	}

	if c.Resources.Workers != 0 && c.Resources.Workers < MinWorkers {
		errs = append(errs, fmt.Errorf("Workers must be >= %d if set, got %d", MinWorkers, c.Resources.Workers))
	}

	if c.Resources.BufferSize != 0 && c.Resources.BufferSize < MinBufferSize {
		errs = append(errs, fmt.Errorf("BufferSize must be >= %d if set, got %d", MinBufferSize, c.Resources.BufferSize))
	}

	if c.ReadTimeout < 0 {
		errs = append(errs, fmt.Errorf("ReadTimeout must be >= 0, got %v", c.ReadTimeout))
	}
	if c.WriteTimeout < 0 {
		errs = append(errs, fmt.Errorf("WriteTimeout must be >= 0, got %v", c.WriteTimeout))
	}
	if c.IdleTimeout < 0 {
		errs = append(errs, fmt.Errorf("IdleTimeout must be >= 0, got %v", c.IdleTimeout))
	}

	if runtime.GOOS != "linux" {
		if c.Engine == engine.IOUring || c.Engine == engine.Epoll {
			errs = append(errs, fmt.Errorf("engine %s requires Linux", c.Engine))
		}
	}

	return errs
}

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
		c.MaxHeaderBytes = 1 << 20
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.ReadTimeout == 0 {
		c.ReadTimeout = 30 * time.Second
	}
	if c.WriteTimeout == 0 {
		c.WriteTimeout = 30 * time.Second
	}
	if c.IdleTimeout == 0 {
		c.IdleTimeout = 120 * time.Second
	}
	return c
}
