//go:build linux

package epoll

import (
	"slices"
	"strings"
	"testing"

	"github.com/goceleris/celeris/internal/platform"
)

// EXPERIMENT-ONLY knob (internal/platform/abplan_experiment.go): the engine's
// planner variable reaches it. With the knob unset the same call is the
// planner's own plan, with no experiment marker.
func TestABPlanKnobReachesTheEpollPlanner(t *testing.T) {
	cpu := platform.PlanWorkerCPUs(1, false).Allowed[0]
	t.Setenv(platform.ABPlanCPUsEnv, "")
	if p := planWorkerCPUs(3, false, ""); strings.Contains(p.Note, "EXPERIMENT-ONLY") {
		t.Fatalf("knob unset but the plan carries the marker: %q", p.Note)
	}
	t.Setenv(platform.ABPlanCPUsEnv, platform.FormatCPUs([]int{cpu}))
	p := planWorkerCPUs(3, false, "")
	if !slices.Equal(p.CPUs, []int{cpu}) || !strings.HasPrefix(p.Note, "EXPERIMENT-ONLY ") {
		t.Fatalf("the epoll planner variable ignored the knob: %+v", p)
	}
}
