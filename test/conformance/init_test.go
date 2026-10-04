package conformance

import (
	"github.com/goceleris/celeris/internal/engine"
	stdengine "github.com/goceleris/celeris/internal/engine/std"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

func init() {
	RegisterFactory(EngineFactory{
		Name:      "std",
		Type:      engine.Std,
		Available: func() bool { return true },
		New: func(cfg resource.Config, handler stream.Handler) (engine.Engine, error) {
			return stdengine.New(cfg, handler)
		},
	})
}
