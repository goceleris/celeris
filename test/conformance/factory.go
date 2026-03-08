package conformance

import (
	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// EngineFactory creates engine instances for conformance testing.
type EngineFactory struct {
	Name      string
	Type      engine.EngineType
	Available func() bool
	New       func(cfg resource.Config, handler stream.Handler) (engine.Engine, error)
}

var factories []EngineFactory

// RegisterFactory registers an engine factory for conformance testing.
func RegisterFactory(f EngineFactory) {
	factories = append(factories, f)
}

// Factories returns all registered engine factories.
func Factories() []EngineFactory {
	return factories
}
