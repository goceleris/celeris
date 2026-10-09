//go:build linux

package spec

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
		registerEngine(specEngine{
			name: "iouring",
			typ:  engine.IOUring,
			new: func(cfg resource.Config, h stream.Handler) (engine.Engine, error) {
				return iouringengine.New(cfg, h)
			},
		})
	}

	registerEngine(specEngine{
		name: "epoll",
		typ:  engine.Epoll,
		new: func(cfg resource.Config, h stream.Handler) (engine.Engine, error) {
			return epollengine.New(cfg, h)
		},
	})
}
