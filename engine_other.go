//go:build !linux

package celeris

import (
	"fmt"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/engine/std"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

func createEngine(cfg resource.Config, handler stream.Handler) (engine.Engine, error) {
	switch cfg.Engine {
	case engine.Std:
		return std.New(cfg, handler)
	case engine.IOUring, engine.Epoll, engine.Adaptive:
		return nil, fmt.Errorf("engine %v requires Linux", cfg.Engine)
	default:
		return nil, fmt.Errorf("unknown engine type: %v", cfg.Engine)
	}
}
