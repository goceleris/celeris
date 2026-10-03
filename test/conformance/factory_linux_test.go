//go:build linux

package conformance

import (
	"github.com/goceleris/celeris/internal/engine"
	epollengine "github.com/goceleris/celeris/internal/engine/epoll"
	iouringengine "github.com/goceleris/celeris/internal/engine/iouring"
	"github.com/goceleris/celeris/internal/probe"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

func init() {
	profile := probe.Probe()
	if profile.IOUringTier >= engine.Base {
		RegisterFactory(EngineFactory{
			Name:      "iouring",
			Type:      engine.IOUring,
			Available: func() bool { return true },
			New: func(cfg resource.Config, handler stream.Handler) (engine.Engine, error) {
				return iouringengine.New(cfg, handler)
			},
		})
	}

	RegisterFactory(EngineFactory{
		Name:      "epoll",
		Type:      engine.Epoll,
		Available: func() bool { return true },
		New: func(cfg resource.Config, handler stream.Handler) (engine.Engine, error) {
			return epollengine.New(cfg, handler)
		},
	})
}
