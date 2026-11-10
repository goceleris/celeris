//go:build linux

package adaptive

import (
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
)

// PIN: this test documents a CONSEQUENCE of the 3-fault floor (celeris#886
// item 29, user decision U-14 "at least 3 faults per interval"), found in the
// review of the PR that added it. It pins today's behaviour; it is not a claim
// that the behaviour is wanted.
//
// The floor is a count per sampling interval, so a server whose traffic is low
// enough never has 3 faults in a one-second interval, however large the share
// of failing requests: with a fault ratio p over the 0.05 revert rate, a rate of
// fewer than 3/p requests a second (at most 60 a second) never reverts. On the
// opt-in io_uring start (WorkloadHint=HighConcurrency or
// CELERIS_ADAPTIVE_START=iouring, connSwitchEnabled false) the revert is the
// only switch the controller can make, so such a server stays on a failing
// io_uring. Before the floor it went back to epoll within a second.
//
// The same steady load behaves differently by interval length: the floor is a
// count and the interval after a switch spans the 30 s cooldown, so 2 faults a
// second at 2 requests a second reverts on that one long interval (60 faults
// over 60 requests) and never on the 1 s ones.
//
// If the maintainer chooses a windowed floor (3 faults accumulated over the last
// N samples, with the rate over the same window) or a floor scaled by elapsed
// time, this test and TestFaultsBelowTheFloorDoNotAddUpAcrossIntervals886 must be
// FLIPPED: the "never" rows below become reverts. Do not delete it to make a
// change pass; change the expectation, and the release note with it.

// stormRevertInterval886 runs io_uring as the active engine on the opt-in
// start with `conns` connections, `rps` requests and `fps` engine faults
// (ErrorSend) per second for `secs` intervals of 1 s, the first one `first`
// long when it is not 0 (the counters of that interval are scaled to it). It
// returns the interval that recommended the revert, 0 for none.
func stormRevertInterval886(conns int64, rps, fps uint64, secs int, first time.Duration) int {
	iou := newMockEngine(engine.IOUring)
	live := newLiveSampler(nil)
	clk := time.Now()
	live.clock = func() time.Time { return clk }
	c := newController(iou, newMockEngine(engine.Epoll), live, testLogger())
	c.cooldown = 0
	c.loadDownRevert = false
	c.connSwitchEnabled = false // the opt-in io_uring start
	m := engine.EngineMetrics{RequestCount: 1000, ActiveConnections: conns, Workers: 2}
	iou.SetMetrics(m)
	c.evaluate(clk, false)
	for i := 1; i <= secs; i++ {
		d, mult := time.Second, uint64(1)
		if i == 1 && first > 0 {
			d, mult = first, uint64(first/time.Second)
		}
		clk = clk.Add(d)
		m.RequestCount += rps * mult
		m.ErrorSend += fps * mult
		m.ErrorCount += fps * mult
		iou.SetMetrics(m)
		if c.evaluate(clk, false) {
			return i
		}
	}
	return 0
}

func TestPinFloorLetsALowTrafficFailingServerStayOnIOUring886(t *testing.T) {
	for _, tc := range []struct {
		name       string
		conns      int64
		rps, fps   uint64
		wantAt     int  // interval of the revert, 0 = never in 600 s
		oldReverts bool // the rule before the floor reverted at interval 1
	}{
		{"1 rps, 100% failing", 1, 1, 1, 0, true},
		{"2 rps, 100% failing", 2, 2, 2, 0, true},
		{"4 rps, 50% failing", 4, 4, 2, 0, true},
		{"10 rps, 20% failing", 10, 10, 2, 0, true},
		{"33 rps, 6% failing", 4, 33, 2, 0, true},
		// At the floor the revert is back: 3 faults a second in 10 requests.
		{"10 rps, 30% failing (3 a second)", 10, 10, 3, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := stormRevertInterval886(tc.conns, tc.rps, tc.fps, 600, 0); got != tc.wantAt {
				t.Errorf("reverted at interval %d of 600 (0 = never), pinned %d", got, tc.wantAt)
			}
			// The premise of the pin: the rate is over the revert rate, so the
			// rule before the floor reverted at once.
			denom := max(float64(tc.rps), float64(tc.conns))
			if old := float64(tc.fps) / denom; (old > 0.05) != tc.oldReverts {
				t.Errorf("without the floor %d/%v = %v: reverts = %v, want %v", tc.fps, denom, old, old > 0.05, tc.oldReverts)
			}
		})
	}
}

// TestPinFloorIsACountSoALongIntervalReverts886: the steady 2 faults a second at
// 2 requests a second that never reverts on 1 s intervals reverts on the first
// sample after a switch, one 30 s interval of 60 faults over 60 requests.
func TestPinFloorIsACountSoALongIntervalReverts886(t *testing.T) {
	if got := stormRevertInterval886(2, 2, 2, 600, 0); got != 0 {
		t.Errorf("1 s intervals: reverted at %d, pinned: never", got)
	}
	if got := stormRevertInterval886(2, 2, 2, 600, 30*time.Second); got != 1 {
		t.Errorf("first interval 30 s: reverted at %d, pinned: 1", got)
	}
}
