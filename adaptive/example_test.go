//go:build linux

package adaptive_test

import (
	"context"
	"time"

	"github.com/goceleris/celeris/adaptive"
	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// fakeCPUMonitor is an external implementation of the PUBLIC engine.CPUMonitor
// interface — proof that callers outside the module can supply their own
// monitor without depending on the internal cpumon package.
type fakeCPUMonitor struct{ util float64 }

func (f fakeCPUMonitor) Sample() (engine.CPUSample, error) {
	return engine.CPUSample{Utilization: f.util, Timestamp: time.Now()}, nil
}

// ExampleNew shows that adaptive.New accepts any engine.CPUMonitor, including
// one defined entirely outside the celeris module. No Output directive: this
// example documents the public signature and is compiled, not executed (it
// would need a live io_uring/epoll-capable kernel to run).
func ExampleNew() {
	var mon engine.CPUMonitor = fakeCPUMonitor{util: 0.9}

	cfg := resource.Config{Addr: ":0", Protocol: engine.HTTP1}
	handler := stream.HandlerFunc(func(_ context.Context, _ *stream.Stream) error { return nil })

	eng, err := adaptive.New(cfg, handler, mon)
	if err != nil {
		return
	}
	_ = eng
}
