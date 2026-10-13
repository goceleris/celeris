//go:build linux

package celeris

import (
	"strings"
	"testing"
	"time"
)

// TestSettledRetimedIOUringBar592 pins the io_uring settled arm's bar
// (celeris#886 item 1) at the level of the assertion, with no engine: an
// observation of a promoted route whose post-promotion /ping samples queued
// behind the blocked Set at the rates measured on the celeris#593 TryLock
// reverted (0.315-0.351 over ten runs) must fail, and the rates of the fixed
// tree (0.000-0.001 over ten runs; 0.145, celeris#622's worst loaded run, on
// the other engine) must pass. epoll keeps its own judgement: the same
// fraction there does not fail the arm. The rig itself
// (TestAdaptiveSettledRouteRetime592/iouring/settled) is what measures the
// fraction.
func TestSettledRetimedIOUringBar592(t *testing.T) {
	obs := func(engine string, queuedFrac float64) stall589Obs {
		const samples = 100
		return stall589Obs{
			engine: engine, mode: stall589Settled.String(),
			promotedInBound: true, promotedAfter: true, asyncPromotedConns: 8,
			samples: samples, queued: int(queuedFrac * samples), queuedFrac: queuedFrac, stalledFrac: queuedFrac,
			queuedBar: stall589Delay / 2, promoteLatency: 5400 * time.Millisecond,
			refSamples: 0, // no reference window: the epoll ratio is skipped, so only the fraction is under test
		}
	}
	cases := []struct {
		name    string
		o       stall589Obs
		wantErr bool
	}{
		{"io_uring, the fixed tree (0.000)", obs("iouring", 0), false},
		{"io_uring, the worst fixed sample (0.001)", obs("iouring", 0.001), false},
		{"io_uring, celeris#622's loaded outlier (0.145)", obs("iouring", 0.145), false},
		{"io_uring, at the bar", obs("iouring", stall592IOUringMaxQueuedFrac), false},
		{"io_uring, the celeris#593 TryLock reverted, lowest run (0.315)", obs("iouring", 0.315), true},
		{"io_uring, the celeris#593 TryLock reverted, highest run (0.351)", obs("iouring", 0.351), true},
		{"epoll with the same fraction is judged on its ratio, not on this", obs("epoll", 0.351), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := assertSettledRetimed592(tc.o)
			if (err != nil) != tc.wantErr {
				t.Fatalf("assertSettledRetimed592 = %v, want error: %v", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "celeris#593") {
				t.Errorf("error %q does not name the celeris#593 shape", err)
			}
		})
	}
}

// TestCtlWorkerFreeIOUringBar592: the control arms (every /kv conn
// async-dispatched, so the worker must be free) apply the same fraction on
// io_uring. With the celeris#593 TryLock reverted both controls passed the
// median ratio and showed 0.324-0.337 of their samples over D/2.
func TestCtlWorkerFreeIOUringBar592(t *testing.T) {
	obs := func(engine string, queuedFrac float64) stall589Obs {
		return stall589Obs{
			engine: engine, mode: stall589ExplicitAsync.String(), kvConns: 8, asyncPromotedConns: 8,
			samples: 100, queued: int(queuedFrac * 100), queuedFrac: queuedFrac, queuedBar: stall589Delay / 2,
			kvSamples: 0, // no reference: only the fraction is under test
		}
	}
	for _, tc := range []struct {
		name    string
		o       stall589Obs
		wantErr bool
	}{
		{"io_uring, the fixed tree (0.000)", obs("iouring", 0), false},
		{"io_uring, the celeris#593 TryLock reverted (0.324)", obs("iouring", 0.324), true},
		{"io_uring, the celeris#593 TryLock reverted (0.337)", obs("iouring", 0.337), true},
		{"epoll with the same fraction is judged on its ratio", obs("epoll", 0.337), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := assertCtlWorkerFree628(tc.o)
			if (err != nil) != tc.wantErr {
				t.Fatalf("assertCtlWorkerFree628 = %v, want error: %v", err, tc.wantErr)
			}
		})
	}
}
