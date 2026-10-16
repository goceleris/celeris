//go:build linux

package epoll

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/platform"
	"github.com/goceleris/celeris/internal/platform/pintest"
)

// celeris#1036: the plan sets the loop count. On a big.LITTLE host the planner
// returns fewer entries than the workers asked for (one per non-little CPU);
// the engine must start len(plan.CPUs) loops, not the number it asked for. The
// test host's CPUs are alike (the planner would return every worker), so the
// planner is stood in for by one that returns fewer entries.
//
// Two requests: a user-set count (Config.Workers: the engine tells the planner
// the count is explicit) and the default (it does not).
func TestLoopCountIsThePlanLength1036(t *testing.T) {
	if !pintest.InOwnProcess(t) {
		pintest.RunInOwnProcess(t, 2*time.Minute)
		return
	}
	cpus, err := pintest.AllowedCPUs()
	if err != nil || len(cpus) < 1 {
		t.Skipf("needs an allowed CPU, have %v (%v)", cpus, err)
	}
	base, _ := pintest.StartupMask()
	for _, tc := range []struct {
		name         string
		workers      int // Resources.Workers; 0 = the default
		planLen      int
		wantExplicit bool
	}{
		{"user-set 6, plan holds 3", 6, 3, true},
		{"default, plan holds 2", 0, 2, false},
	} {
		func() {
			old := planWorkerCPUs
			defer func() { planWorkerCPUs = old }()
			var gotN int
			var gotExplicit bool
			var want []int
			planWorkerCPUs = func(n int, explicit bool) platform.CPUPlan {
				gotN, gotExplicit = n, explicit
				want = make([]int, tc.planLen)
				for i := range want {
					want[i] = cpus[i%len(cpus)]
				}
				return platform.CPUPlan{CPUs: append([]int(nil), want...), Allowed: cpus, Requested: n,
					Heterogeneous: true, Note: "test plan: fewer loops than asked for"}
			}
			var log lockedBuf
			eng, stop := start909(t, tc.workers, &log)
			defer stop()
			if gotExplicit != tc.wantExplicit {
				t.Errorf("%s: the engine told the planner explicit=%v, want %v", tc.name, gotExplicit, tc.wantExplicit)
			}
			if tc.workers > 0 && gotN != tc.workers {
				t.Errorf("%s: the planner was asked for %d workers, want %d", tc.name, gotN, tc.workers)
			}
			if tc.workers == 0 && gotN < tc.planLen {
				t.Skipf("the default is %d worker(s), fewer than the plan's %d: nothing is shortened", gotN, tc.planLen)
			}
			if gotN <= tc.planLen {
				t.Fatalf("%s: the planner was asked for %d workers, not more than the plan holds (%d)", tc.name, gotN, tc.planLen)
			}
			if got := len(eng.loops); got != tc.planLen {
				t.Errorf("%s: %d loops started, the plan holds %d", tc.name, got, tc.planLen)
			}
			if got := eng.NumWorkers(); got != tc.planLen {
				t.Errorf("%s: NumWorkers() = %d, want %d", tc.name, got, tc.planLen)
			}
			if got := int(eng.Metrics().Workers); got != tc.planLen {
				t.Errorf("%s: Metrics().Workers = %d, want %d", tc.name, got, tc.planLen)
			}
			for i, c := range want {
				if got := eng.loops[i].CPUID(); got != c {
					t.Errorf("%s: loop %d: CPUID() = %d, want %d", tc.name, i, got, c)
				}
			}
			// The "listening" line, not the plan's own line, which reads the plan.
			listening := ""
			for _, ln := range strings.Split(log.String(), "\n") {
				if strings.Contains(ln, "engine listening") {
					listening = ln
				}
			}
			if !strings.Contains(listening, fmt.Sprintf(" loops=%d", tc.planLen)) {
				t.Errorf("%s: the listening line does not say loops=%d:\n%s", tc.name, tc.planLen, log.String())
			}
			running, err := pintest.Census()
			if err != nil {
				t.Fatal(err)
			}
			single := 0
			for _, th := range pintest.Off(running, base) {
				if th.Single() {
					single++
				}
			}
			if single != tc.planLen {
				t.Errorf("%s: %d thread(s) pinned to one CPU, want %d (one per loop): %v",
					tc.name, single, tc.planLen, pintest.Off(running, base))
			}
		}()
	}
}
