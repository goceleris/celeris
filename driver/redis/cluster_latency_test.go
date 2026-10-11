package redis

import (
	"testing"
	"time"
)

// A round trip the clock cannot resolve is still a measurement: 0 in
// clusterNode.latency means "not measured" and lowestLatencyNode skips it, so
// on a coarse clock (Windows) a fast replica would lose to a slow one that
// was measured (TestClusterRouteByLatency failed on windows-latest).
func TestLatencyNanosKeepsAnUnresolvedRoundTripMeasured(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want int64
	}{
		{0, 1}, {-5, 1}, {1, 1}, {50 * time.Millisecond, int64(50 * time.Millisecond)},
	} {
		if got := latencyNanos(tc.d); got != tc.want {
			t.Errorf("latencyNanos(%v) = %d, want %d", tc.d, got, tc.want)
		}
	}
}

func TestLowestLatencyNodePrefersAnUnresolvedFastNodeOverASlowOne(t *testing.T) {
	c := &ClusterClient{}
	primary := &clusterNode{}
	fast, slow := &clusterNode{}, &clusterNode{}
	primary.latency.Store(latencyNanos(80 * time.Millisecond))
	slow.latency.Store(latencyNanos(50 * time.Millisecond))
	fast.latency.Store(latencyNanos(0)) // measured below the clock's resolution
	if got := c.lowestLatencyNode(primary, []*clusterNode{slow, fast}); got != fast {
		t.Fatalf("lowestLatencyNode chose a node with latency %d, want the fast one (%d)", got.latency.Load(), fast.latency.Load())
	}
}
