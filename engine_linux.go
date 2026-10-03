//go:build linux

package celeris

import (
	"fmt"

	"github.com/goceleris/celeris/internal/adaptive"
	"github.com/goceleris/celeris/internal/cpumon"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/engine/epoll"
	"github.com/goceleris/celeris/internal/engine/iouring"
	"github.com/goceleris/celeris/internal/engine/std"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

func createEngine(cfg resource.Config, handler stream.Handler, cpuMon cpumon.Monitor) (engine.Engine, error) {
	switch cfg.Engine {
	case engine.IOUring:
		return iouring.New(cfg, handler)
	case engine.Epoll:
		return epoll.New(cfg, handler)
	case engine.Adaptive:
		return adaptive.New(cfg, handler, cpuMon)
	case engine.Std:
		return std.New(cfg, handler)
	default:
		return nil, fmt.Errorf("unknown engine type: %v", cfg.Engine)
	}
}
