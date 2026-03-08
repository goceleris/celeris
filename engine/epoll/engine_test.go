//go:build linux

package epoll

import (
	"testing"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/resource"
)

func TestNewEngine(t *testing.T) {
	cfg := resource.Config{
		Addr:     ":8099",
		Protocol: engine.HTTP1,
	}
	e, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if e.Type() != engine.Epoll {
		t.Errorf("Type: got %v, want Epoll", e.Type())
	}
}

func TestMetrics(t *testing.T) {
	cfg := resource.Config{
		Addr:     ":8099",
		Protocol: engine.HTTP1,
	}
	e, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m := e.Metrics()
	if m.RequestCount != 0 {
		t.Errorf("initial RequestCount: got %d, want 0", m.RequestCount)
	}
}
