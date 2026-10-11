//go:build linux

package epoll

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	celerisengine "github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// BenchmarkListenCycle895 is the cost of what celeris#895 changed:
// the engine's start (Listen called until Addr() is published, which resolves
// a port 0) and its stop (the context cancelled until Listen has returned,
// which closes the loops). Nothing on
// the request path was touched; this is the whole of the start/stop path.
// Run it at memlock unlimited with -benchtime=200x.
func BenchmarkListenCycle895(b *testing.B) {
	var startNs, stopNs int64
	for range b.N {
		e, err := New(resource.Config{
			Addr:      "127.0.0.1:0",
			Engine:    celerisengine.Epoll,
			Protocol:  celerisengine.HTTP1,
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			Resources: resource.Resources{Workers: 2},
		}, respondingHandler{})
		if err != nil {
			b.Skipf("New: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		t0 := time.Now()
		go func() { done <- e.Listen(ctx) }()
		for e.Addr() == nil {
			select {
			case err := <-done:
				b.Fatalf("Listen returned before publishing an address: %v", err)
			default:
			}
			time.Sleep(20 * time.Microsecond)
		}
		t1 := time.Now()
		cancel()
		<-done
		t2 := time.Now()
		startNs += t1.Sub(t0).Nanoseconds()
		stopNs += t2.Sub(t1).Nanoseconds()
	}
	b.ReportMetric(float64(startNs)/float64(b.N)/1e3, "start-us/op")
	b.ReportMetric(float64(stopNs)/float64(b.N)/1e3, "stop-us/op")
}
