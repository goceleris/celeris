//go:build linux

package adaptive

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// Engine.Metrics sums the two sub-engines' Metrics, and the sub-engine that
// serves first is epoll, whose Metrics read its loop count while Listen
// assigned it, with no lock between the two. A monitoring goroutine started
// before the server is up (probatorium's instrument polls /debug/sched, which
// reads Server.EngineInfo, during startup) therefore raced with the active
// sub-engine's Listen through the adaptive Metrics too: the same pair, with
// one more frame on top. The adaptive slots themselves are read under slotsMu
// (celeris#886), so they are not what this guards; the sum through the live
// sub-engine is.
//
// The reader starts before Listen and keeps reading after the address is
// published, over several engines started and stopped in turn. The race
// detector needs no overlap in time, only that the read and the write are not
// ordered; the assertion is the detector's, so this proves nothing without
// -race.
func TestMetricsDuringListenIsRaceFree(t *testing.T) {
	for i := range 5 {
		e, err := New(resource.Config{
			Addr:      "127.0.0.1:0",
			Protocol:  engine.HTTP1,
			Resources: resource.Resources{Workers: 2},
		}, respHandler{}, nil)
		if err != nil {
			t.Fatalf("adaptive.New: %v", err)
		}

		// started closes after the reader's FIRST call and Listen waits on
		// it, so the reader is running before the loops are published.
		var stop atomic.Bool
		started := make(chan struct{})
		readerDone := make(chan struct{})
		go func() {
			defer close(readerDone)
			first := true
			for !stop.Load() {
				_ = e.Metrics()
				if first {
					first = false
					close(started)
				}
			}
		}()
		<-started

		ctx, cancel := context.WithCancel(t.Context())
		errCh := make(chan error, 1)
		go func() { errCh <- e.Listen(ctx) }()

		var listenErr error
		deadline := time.Now().Add(15 * time.Second)
	wait:
		for e.Addr() == nil && time.Now().Before(deadline) {
			select {
			case listenErr = <-errCh:
				break wait
			case <-time.After(time.Millisecond):
			}
		}
		bound := e.Addr() != nil
		if bound {
			time.Sleep(5 * time.Millisecond) // reads after the publish, too
		}
		stop.Store(true)
		<-readerDone
		cancel()
		if listenErr == nil {
			select {
			case listenErr = <-errCh:
			case <-time.After(10 * time.Second):
				t.Errorf("iteration %d: Listen did not return after cancel", i)
			}
		}
		if !bound {
			t.Fatalf("iteration %d: engine never bound (Listen: %v)", i, listenErr)
		}
	}
}
