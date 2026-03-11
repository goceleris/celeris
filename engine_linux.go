//go:build linux

package celeris

import (
	"fmt"

	"github.com/goceleris/celeris/adaptive"
	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/engine/epoll"
	"github.com/goceleris/celeris/engine/iouring"
	"github.com/goceleris/celeris/engine/std"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

func createEngine(cfg resource.Config, handler stream.Handler) (engine.Engine, error) {
	switch cfg.Engine {
	case engine.IOUring:
		return iouring.New(cfg, handler)
	case engine.Epoll:
		return epoll.New(cfg, handler)
	case engine.Adaptive:
		return adaptive.New(cfg, handler)
	case engine.Std:
		return std.New(cfg, handler)
	default:
		return nil, fmt.Errorf("unknown engine type: %v", cfg.Engine)
	}
}
