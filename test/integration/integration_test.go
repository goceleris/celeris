//go:build linux

package integration

import (
	"testing"

	"github.com/goceleris/celeris/adaptive"
	"github.com/goceleris/celeris/engine"
)

func TestAdaptiveEngineLifecycle(t *testing.T) {
	port := freePort(t)
	cfg := defaultTestConfig(port, engine.Auto)
	cfg.Engine = engine.Adaptive

	e, err := adaptive.New(cfg, &echoHandler{})
	if err != nil {
		t.Skipf("adaptive engine not available: %v", err)
	}

	startEngine(t, e)

	if e.Type() != engine.Adaptive {
		t.Errorf("got type %v, want adaptive", e.Type())
	}

	m := e.Metrics()
	if m.ActiveConnections < 0 {
		t.Error("negative active connections")
	}
}
