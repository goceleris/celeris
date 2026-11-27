//go:build linux

package redis

import (
	"fmt"
	"testing"

	"github.com/goceleris/celeris/driver/internal/busypolltest"
	"github.com/goceleris/celeris/driver/internal/eventloop"
)

// TestFailedExecLateWriterNoExposure: the clients that have no late writer.
// The engine-backed client reads on the caller's goroutine; single commands
// and Tx on the standalone client take their requests from a pool that a
// failed wait never returns to. Same delays, same per-key checks: zero wrong
// values, with the hook calls counted to show the window was exercised.
func TestFailedExecLateWriterNoExposure(t *testing.T) {
	fake := lateServer(t)
	engine, err := NewClient(fake.Addr(), WithPoolSize(64), WithEngine(busypolltest.StartServer(t, 0)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	standalone, err := NewClient(fake.Addr(), WithPoolSize(64))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = standalone.Close() })
	for _, delay := range lateDelays(t) {
		us := delay.Microseconds()
		t.Run(fmt.Sprintf("engine-pipeline/%dus", us), func(t *testing.T) {
			for _, shape := range []string{"retry", "release"} {
				pause := eventloop.NewPause(10, delay)
				restore := eventloop.SetDispatchPause(pause)
				var cnt lateCounts
				pipelineLate(t, engine, shape, lateSecs(), &cnt)
				restore()
				report(t, fmt.Sprintf("engine-backed shape=%s delay=%dus", shape, us), &cnt, pause.Hits())
			}
		})
		t.Run(fmt.Sprintf("standalone-single-and-tx/%dus", us), func(t *testing.T) {
			pause := eventloop.NewPause(10, delay)
			defer eventloop.SetDispatchPause(pause)()
			var cnt lateCounts
			singleAndTxLate(t, standalone, lateSecs(), &cnt)
			report(t, fmt.Sprintf("standalone single+Tx delay=%dus", us), &cnt, pause.Hits())
		})
	}
}
