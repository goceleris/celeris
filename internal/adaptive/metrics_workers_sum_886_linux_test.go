//go:build linux

package adaptive

import (
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
)

// celeris#886 item 31 (from PR #1062): on a big.LITTLE host the two
// sub-engines can start different numbers of loops (PR #1062's NIC interrupt
// placement can add one or two pinned loops to one of them), and the only test
// of Metrics().Workers summed 12 + 12.

// TestMetricsWorkersSumsUnequalSubEngines886: 8 loops on epoll and 10 on
// io_uring (8 plus two extra pinned loops) are 18, the divisor that matches the
// summed ActiveConnections; a standby that has not been built yet adds none.
func TestMetricsWorkersSumsUnequalSubEngines886(t *testing.T) {
	e, _ := newAdaptiveStartingOnEpoll(t)
	e.primary.(*mockEngine).SetMetrics(engine.EngineMetrics{Workers: 8, ActiveConnections: 96})
	e.secondary.(*mockEngine).SetMetrics(engine.EngineMetrics{Workers: 10, ActiveConnections: 30})

	m := e.Metrics()
	if m.Workers != 18 {
		t.Errorf("Metrics().Workers = %d for 8 + 10 loops, want 18", m.Workers)
	}
	if m.ActiveConnections != 126 {
		t.Errorf("Metrics().ActiveConnections = %d, want 126", m.ActiveConnections)
	}

	// The lazy shape: the io_uring standby is built at the first switch.
	e.slotsMu.Lock()
	e.secondary = nil
	e.slotsMu.Unlock()
	if got := e.Metrics().Workers; got != 8 {
		t.Errorf("Metrics().Workers = %d with the standby not built, want 8", got)
	}
}

// TestControllerDividesByTheActiveEnginesOwnLoops886: the controller's
// conns-per-worker is the ACTIVE engine's connections over the ACTIVE engine's
// loops, not over the adaptive sum. It is driven through ctrl.evaluate, so that
// it is the engine the controller hands to the sampler that is under test. This
// is the divisor 886 item 30 (2) asks about on a big.LITTLE host (8 loops, not
// GOMAXPROCS 12); the thresholds are not changed by this test, only the
// arithmetic is pinned.
func TestControllerDividesByTheActiveEnginesOwnLoops886(t *testing.T) {
	epoll, iou := newMockEngine(engine.Epoll), newMockEngine(engine.IOUring)
	epoll.SetMetrics(engine.EngineMetrics{Workers: 8, ActiveConnections: 192})
	iou.SetMetrics(engine.EngineMetrics{Workers: 10, ActiveConnections: 1000})
	sampler := &lastSnapSampler{TelemetrySampler: newLiveSampler(nil)}
	c := newController(epoll, iou, sampler, testLogger())
	c.cooldown = 0
	c.evaluate(time.Now(), false)
	// 192 connections are 24/worker on 8 loops, at the up-threshold. The same
	// 192 connections on the 12 loops a host of alike CPUs would start are 16;
	// divided by the sum of both engines' loops they would be 10.67.
	if got := sampler.last.ConnsPerWorker; got != 24 {
		t.Errorf("ConnsPerWorker = %v for 192 connections on 8 loops, want 24 (not 192/18 = %v)", got, 192.0/18)
	}
	if sampler.last.ConnsPerWorker < c.upThreshold {
		t.Errorf("ConnsPerWorker %v is under the up-threshold %v", sampler.last.ConnsPerWorker, c.upThreshold)
	}
}
