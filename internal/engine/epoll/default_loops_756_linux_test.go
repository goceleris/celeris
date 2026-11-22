//go:build linux

package epoll

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/platform"
	"github.com/goceleris/celeris/internal/platform/pintest"
)

// celeris#756, #757: an epoll engine whose worker count is not set by the user
// starts GOMAXPROCS - max(1, GOMAXPROCS/8) loops on a host of alike CPUs (none
// for a GOMAXPROCS of 2 or less; Resolve raises a count to two), and a count
// the user sets is started whole. This is the real planner and real engine, no
// stub: Metrics().Workers, NumWorkers(), the loops and the "listening" line
// all read the loops started. The test pins the policy, not any latency.
//
// GOMAXPROCS is set inside the test's own process, so the request is the same
// on any runner (the figures are written out, not computed from the planner).
func TestDefaultLoopsLeaveAReserve756(t *testing.T) {
	if !pintest.InOwnProcess(t) {
		pintest.RunInOwnProcess(t, 3*time.Minute)
		return
	}
	if p := platform.PlanWorkerCPUs(12, true); p.Heterogeneous {
		t.Skip("this host's CPUs differ in capacity: the big.LITTLE plan is not the reserve's")
	}
	prev := runtime.GOMAXPROCS(0)
	defer runtime.GOMAXPROCS(prev)
	for _, tc := range []struct {
		gomaxprocs int
		workers    int // Resources.Workers; 0 = the default
		want       int // loops started
	}{
		{16, 0, 14},  // the default: 16 Ps, 14 loops (rc1: 16)
		{16, 16, 16}, // Workers = GOMAXPROCS: one loop per P, as before
		{16, 5, 5},   // a user-set count is kept whole
		{16, 20, 20}, // also above GOMAXPROCS
		{8, 0, 7},
		{4, 0, 3},
		{3, 0, 2},
		{2, 0, 2}, // no reserve at two
		{1, 0, 2}, // Resolve raises the default to MinWorkers (2); no reserve at two
	} {
		t.Run(fmt.Sprintf("GOMAXPROCS=%d/Workers=%d", tc.gomaxprocs, tc.workers), func(t *testing.T) {
			runtime.GOMAXPROCS(tc.gomaxprocs)
			var log lockedBuf
			eng, stop := start909(t, tc.workers, &log)
			defer stop()
			if got := len(eng.loops); got != tc.want {
				t.Errorf("%d loops started, want %d", got, tc.want)
			}
			if got := eng.Metrics().Workers; got != tc.want {
				t.Errorf("Metrics().Workers = %d, want %d", got, tc.want)
			}
			if got := eng.NumWorkers(); got != tc.want {
				t.Errorf("NumWorkers() = %d, want %d", got, tc.want)
			}
			if got := listeningLine1036(t, &log); !strings.Contains(got, fmt.Sprintf(" loops=%d", tc.want)) {
				t.Errorf("the listening line does not say loops=%d:\n%s", tc.want, log.String())
			}
		})
	}
}
