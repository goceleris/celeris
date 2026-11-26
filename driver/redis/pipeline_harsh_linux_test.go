//go:build linux

package redis

import (
	"testing"

	"github.com/goceleris/celeris/driver/internal/busypolltest"
)

// TestPipelineHarshEngineBacked runs the harsh setup (see pipeline_harsh_test.go)
// on an engine-backed client: a started epoll engine, whose command conns read
// on the caller's goroutine.
func TestPipelineHarshEngineBacked(t *testing.T) {
	fake := harshServer(t)
	c, err := NewClient(fake.Addr(), WithPoolSize(16), WithEngine(busypolltest.StartServer(t, 0)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	runHarshPipeline(t, "engine-backed", c, harshSecs(t))
}
