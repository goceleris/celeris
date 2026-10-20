//go:build linux

package iouring

import (
	"slices"
	"strings"
	"testing"

	"github.com/goceleris/celeris/internal/platform"
)

// EXPERIMENT-ONLY knob (internal/platform/abplan_experiment.go): the io_uring
// engine's plan, which is held to RLIMIT_MEMLOCK after the planner, carries it.
func TestABPlanKnobReachesTheIouringPlan(t *testing.T) {
	cpu := platform.PlanWorkerCPUs(1, false).Allowed[0]
	t.Setenv(platform.ABPlanCPUsEnv, "")
	if p := planWithinMemlock(3, false, "", nil); strings.Contains(p.Note, "EXPERIMENT-ONLY") {
		t.Fatalf("knob unset but the plan carries the marker: %q", p.Note)
	}
	t.Setenv(platform.ABPlanCPUsEnv, platform.FormatCPUs([]int{cpu}))
	p := planWithinMemlock(3, false, "", nil)
	if !slices.Equal(p.CPUs, []int{cpu}) || !strings.HasPrefix(p.Note, "EXPERIMENT-ONLY ") {
		t.Fatalf("planWithinMemlock ignored the knob: %+v", p)
	}
}
