//go:build linux

package spec

import (
	"github.com/goceleris/celeris/engine"
	epollengine "github.com/goceleris/celeris/engine/epoll"
	iouringengine "github.com/goceleris/celeris/engine/iouring"
	"github.com/goceleris/celeris/probe"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
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
