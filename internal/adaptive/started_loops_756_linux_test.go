//go:build linux

package adaptive

import (
	"testing"

	"github.com/goceleris/celeris/internal/platform"
)

// celeris#756, #757: startedLoops, which the adaptive engine uses to size the
// io_uring memlock check, counts the loops the engines start: the default
// count on a host of alike CPUs is the request less the reserve of
// max(1, n/8) (none at 2 or less), a user-set count is the request. The real
// planner, no stub; the figures are written out.
func TestStartedLoopsLeaveAReserve756(t *testing.T) {
	if platform.PlanWorkerCPUs(12, true).Heterogeneous {
		// Not a t.Skip: skip_routing_684_test.go polices every skip of this package.
		t.Log("this host's CPUs differ in capacity: the big.LITTLE plan is not the reserve's; nothing to check")
		return
	}
	for _, tc := range []struct {
		requested int
		explicit  bool
		want      int
	}{
		{16, false, 14}, {16, true, 16}, {8, false, 7}, {32, false, 28},
		{4, false, 3}, {3, false, 2}, {2, false, 2}, {1, false, 1}, {5, true, 5},
	} {
		if got := startedLoops(tc.requested, tc.explicit); got != tc.want {
			t.Errorf("startedLoops(%d, explicit=%v) = %d, want %d", tc.requested, tc.explicit, got, tc.want)
		}
	}
}
