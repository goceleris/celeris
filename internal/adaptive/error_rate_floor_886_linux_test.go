//go:build linux

package adaptive

import (
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
)

// celeris#886 item 29 (user decision, 2026-10-25): the io_uring error-rate
// revert has a floor of 3 faults per sampling interval. Below it the interval
// reads rate 0 whatever the divisor: one stray accept error (AcceptCancelled
// counts ECONNABORTED, EINTR, ECANCELED, EBADF) in an idle second of a
// server started on io_uring used to read 1/5 = 0.2, revert, and, with
// connSwitchEnabled false on that start, never come back. The numbers below
// are the decision's, not tunables read from the code: the tests spell the
// floor as 3 so that changing it fails here.

// TestFaultRateFloor886 is the arithmetic around the floor.
func TestFaultRateFloor886(t *testing.T) {
	for _, tc := range []struct {
		name       string
		faults     uint64
		reqs       uint64
		connSecs   float64
		wantRate   float64
		wantDenom  float64
		wantRevert bool
	}{
		// One stray accept error on an idle server: no request, 5 connection-seconds.
		{"one fault, idle", 1, 0, 5, 0, 5, false},
		// The old faults/requests rule reverted on 1 fault in 1 to 19 requests.
		{"one fault in one request", 1, 1, 1, 0, 1, false},
		{"one fault in nineteen requests", 1, 19, 1, 0, 19, false},
		{"two faults in four requests", 2, 4, 4, 0, 4, false},
		// The floor is exact: the third fault is counted again, as before.
		{"three faults in four requests", 3, 4, 4, 0.75, 4, true},
		{"three faults, rate over the bar", 3, 50, 40, 0.06, 50, true},
		{"three faults, rate under the bar", 3, 100, 40, 0.03, 100, false},
		// A storm is not touched by the floor.
		{"storm", 580, 3, 10, 580.0 / 10, 10, true},
		// The divisor is still reported when the floor cuts the rate.
		{"two faults, connection-seconds dominate", 2, 10, 200, 0, 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rate, denom := faultRate(tc.faults, tc.reqs, tc.connSecs)
			if !near886(rate, tc.wantRate) || !near886(denom, tc.wantDenom) {
				t.Errorf("faultRate(%d, %d, %v) = %v / %v, want %v / %v",
					tc.faults, tc.reqs, tc.connSecs, rate, denom, tc.wantRate, tc.wantDenom)
			}
			if got := rate > newController(nil, nil, nil, testLogger()).errorRevertRate; got != tc.wantRevert {
				t.Errorf("rate %v: revert = %v, want %v", rate, got, tc.wantRevert)
			}
		})
	}
}

// TestOneStrayAcceptErrorDoesNotRevertAnIdleIOUring886 is the case of the
// issue through the sampler and the controller: an io_uring engine with a few
// idle connections, no request in the second, one accept error. The revert is
// one-way on the opt-in io_uring start, so one stray ECONNABORTED must not
// take it.
func TestOneStrayAcceptErrorDoesNotRevertAnIdleIOUring886(t *testing.T) {
	base := engine.EngineMetrics{RequestCount: 1000, ActiveConnections: 5, Workers: 1}
	for _, tc := range []struct {
		name       string
		faults     uint64
		wantRevert bool
	}{
		{"one accept error", 1, false},
		{"two accept errors", 2, false},
		{"three accept errors", 3, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			revert, snap := feed886(t, testLogger(), base, func(m *engine.EngineMetrics) {
				m.ErrorAcceptCancelled = tc.faults
				m.ErrorCount = tc.faults
			})
			if revert != tc.wantRevert {
				t.Errorf("%d accept errors, 5 idle connections, no request: revert = %v, want %v (error_rate=%v over %v)",
					tc.faults, revert, tc.wantRevert, snap.ErrorRate, snap.RateDenominator)
			}
			// The floor cuts the rate, not the evidence: the bucket still moved
			// and the divisor is still the interval's.
			if got := snap.ErrorDelta[1]; got != tc.faults { // errorBucketNames[1] is error_accept_cancelled
				t.Errorf("ErrorDelta[error_accept_cancelled] = %d, want %d", got, tc.faults)
			}
			if !near886(snap.RateDenominator, 5) {
				t.Errorf("RateDenominator = %v, want 5", snap.RateDenominator)
			}
		})
	}
}

// TestFaultsBelowTheFloorDoNotAddUpAcrossIntervals886 pins the floor as a
// per-interval one, which is what the decision says. ONE sampler and ONE
// controller see four consecutive 1 s intervals of an idle pool of 5
// connections: 2 faults, 2 faults, 1 fault, then 3. None of the first three
// reverts and each reads rate 0, although the first two together are 4 faults
// (and all three 5); the fourth, at the floor, reverts on its own 3.
func TestFaultsBelowTheFloorDoNotAddUpAcrossIntervals886(t *testing.T) {
	iou := newMockEngine(engine.IOUring)
	live := newLiveSampler(nil)
	t0 := time.Now()
	clk := t0
	live.clock = func() time.Time { return clk }
	sampler := &lastSnapSampler{TelemetrySampler: live}
	c := newController(iou, newMockEngine(engine.Epoll), sampler, testLogger())
	c.cooldown = 0
	c.loadDownRevert = false

	m := engine.EngineMetrics{RequestCount: 1000, ActiveConnections: 5, Workers: 1}
	iou.SetMetrics(m)
	if c.evaluate(clk, false) {
		t.Fatal("the baseline tick recommended a switch")
	}
	var total uint64
	for i, tc := range []struct {
		faults     uint64
		wantRevert bool
	}{{2, false}, {2, false}, {1, false}, {3, true}} {
		clk = clk.Add(time.Second)
		total += tc.faults
		m.ErrorAcceptCancelled, m.ErrorCount = total, total
		iou.SetMetrics(m)
		revert := c.evaluate(clk, false)
		snap := sampler.last
		if revert != tc.wantRevert {
			t.Errorf("interval %d (%d faults, %d in all so far): revert = %v, want %v (error_rate=%v)",
				i+1, tc.faults, total, revert, tc.wantRevert, snap.ErrorRate)
		}
		if tc.wantRevert {
			if !near886(snap.ErrorRate, 3.0/5) {
				t.Errorf("interval %d: error_rate = %v, want 3/5 (the interval's own 3 faults, not the %d so far)", i+1, snap.ErrorRate, total)
			}
		} else if snap.ErrorRate != 0 {
			t.Errorf("interval %d (%d faults): error_rate = %v, want 0", i+1, tc.faults, snap.ErrorRate)
		}
	}
}

// TestFloorLeavesAStormOnAnIdlePoolReverting886: the other direction. Faults at
// the floor on a mostly idle pool whose sends all fail still revert when the
// rate is over the bar; the floor removes the one-fault case and nothing else.
func TestFloorLeavesAStormOnAnIdlePoolReverting886(t *testing.T) {
	base := engine.EngineMetrics{RequestCount: 1000, ActiveConnections: 40, Workers: 1}
	revert, snap := feed886(t, testLogger(), base, func(m *engine.EngineMetrics) {
		m.RequestCount += 10
		m.ErrorSend = 10
		m.ErrorCount = 10
	})
	if !revert || !near886(snap.ErrorRate, 10.0/40) {
		t.Errorf("10 send faults on 40 connections: revert=%v error_rate=%v, want a revert at 0.25", revert, snap.ErrorRate)
	}
}
