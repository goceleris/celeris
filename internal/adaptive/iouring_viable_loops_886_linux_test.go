//go:build linux

package adaptive

import (
	"runtime"
	"testing"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/platform"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#886 item 30 (1), from PR #1049: on a big.LITTLE host the engines
// start one pinned loop per non-little CPU, 8 of 12 on msr1, fewer than the
// requested GOMAXPROCS. ioUringViable compared the REQUESTED count with the
// RLIMIT_MEMLOCK worker ceiling, so a ceiling of 8 to 11 workers kept the
// engine off io_uring although the plan needs only 8 (iouring.planWithinMemlock
// caps the plan, not the request). These tests stand in the big.LITTLE host
// with planLoopCPUs and a memlock ceiling; the real planner is covered by the
// platform package.

// withPlan886 makes planLoopCPUs plan `loops` pinned loops whatever is asked
// (a big.LITTLE host with `loops` non-little CPUs), and records the request.
func withPlan886(t *testing.T, loops int) (gotRequested *int, gotExplicit *bool) {
	t.Helper()
	var req int
	var explicit bool
	prev := planLoopCPUs
	planLoopCPUs = func(n int, exp bool, _ string) platform.CPUPlan {
		req, explicit = n, exp
		cpus := make([]int, loops)
		for i := range cpus {
			cpus[i] = i + 4 // the CPUs after four little ones
		}
		return platform.CPUPlan{Requested: n, CPUs: cpus, Heterogeneous: true}
	}
	t.Cleanup(func() { planLoopCPUs = prev })
	return &req, &explicit
}

// withGOMAXPROCS886 sets GOMAXPROCS, the default worker request, for the test.
func withGOMAXPROCS886(t *testing.T, n int) {
	t.Helper()
	prev := runtime.GOMAXPROCS(n)
	t.Cleanup(func() { runtime.GOMAXPROCS(prev) })
}

// TestIOUringViableCountsTheLoopsTheEnginesStart886 is the band of the issue:
// 12 requested, 8 started, a ceiling of 8 to 11.
func TestIOUringViableCountsTheLoopsTheEnginesStart886(t *testing.T) {
	withGOMAXPROCS886(t, 12)
	gotReq, gotExplicit := withPlan886(t, 8)
	for _, tc := range []struct {
		ceiling int
		want    bool
	}{
		{-1, true}, // no ceiling
		{12, true},
		{11, true}, // the band: the plan needs 8, the request was 12
		{10, true},
		{9, true},
		{8, true}, // exactly funds the 8 loops
		{7, false},
		{1, false},
	} {
		withMemlock(t, tc.ceiling)
		if got := ioUringViable(viableProfile(), resource.Config{}); got != tc.want {
			t.Errorf("memlock ceiling %d, 12 requested, 8 planned: ioUringViable = %v, want %v", tc.ceiling, got, tc.want)
		}
	}
	if *gotReq != 12 || *gotExplicit {
		t.Errorf("the plan was asked for %d workers (explicit=%v), want 12 (explicit=false): the default request is GOMAXPROCS", *gotReq, *gotExplicit)
	}
}

// TestIOUringViablePlansAUserSetWorkerCountAsExplicit886: a Config.Workers is
// planned as the engines plan it (explicit), so the plan honours it.
func TestIOUringViablePlansAUserSetWorkerCountAsExplicit886(t *testing.T) {
	gotReq, gotExplicit := withPlan886(t, 10)
	withMemlock(t, 10)
	cfg := resource.Config{Resources: resource.Resources{Workers: 10}}
	if !ioUringViable(viableProfile(), cfg) {
		t.Error("10 workers planned, ceiling 10: not viable")
	}
	if *gotReq != 10 || !*gotExplicit {
		t.Errorf("the plan was asked for %d workers (explicit=%v), want 10 (explicit=true)", *gotReq, *gotExplicit)
	}
	withMemlock(t, 9)
	if ioUringViable(viableProfile(), cfg) {
		t.Error("10 workers planned, ceiling 9: viable")
	}
}

// TestChooseStartEngineUsesThePlannedLoops886 is the user-visible side: the
// WorkloadHint=HighConcurrency start on io_uring, which a ceiling inside the
// band took away from a big.LITTLE host.
func TestChooseStartEngineUsesThePlannedLoops886(t *testing.T) {
	t.Setenv("CELERIS_ADAPTIVE_START", "")
	withGOMAXPROCS886(t, 12)
	withPlan886(t, 8)
	withMemlock(t, 10)
	cfg := resource.Config{Protocol: engine.HTTP1, Resources: resource.Resources{WorkloadHint: resource.WorkloadHighConcurrency}}
	if got := chooseStartEngine(viableProfile(), cfg); got != engine.IOUring {
		t.Errorf("chooseStartEngine = %v on a host that plans 8 loops under a ceiling of 10, want io_uring", got)
	}
}

// TestIOUringViableOnAHostOfAlikeCPUsIsUnchanged886: where the plan is as long
// as the request (every host measured but the big.LITTLE ones), nothing moves.
func TestIOUringViableOnAHostOfAlikeCPUsIsUnchanged886(t *testing.T) {
	withGOMAXPROCS886(t, 6)
	prev := planLoopCPUs
	t.Cleanup(func() { planLoopCPUs = prev })
	planLoopCPUs = func(n int, exp bool, addr string) platform.CPUPlan {
		cpus := make([]int, n)
		for i := range cpus {
			cpus[i] = i
		}
		return platform.CPUPlan{Requested: n, CPUs: cpus}
	}
	for _, tc := range []struct {
		ceiling int
		want    bool
	}{{-1, true}, {6, true}, {5, false}} {
		withMemlock(t, tc.ceiling)
		if got := ioUringViable(viableProfile(), resource.Config{}); got != tc.want {
			t.Errorf("ceiling %d, 6 requested and planned: ioUringViable = %v, want %v", tc.ceiling, got, tc.want)
		}
	}
}
