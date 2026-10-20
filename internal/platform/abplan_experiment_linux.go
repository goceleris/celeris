//go:build linux

package platform

import (
	"fmt"
	"strings"
)

// applyABPlanKnob is the whole of the EXPERIMENT-ONLY knob (abplan_experiment.go)
// on the planner's output. env is the value of ABPlanCPUsEnv. Empty (unset): the
// plan comes back unchanged, the same value. A valid list: the plan's CPUs are
// replaced by that list (one pinned loop per CPU, ascending) and its Note, which
// the engine logs once as the "capability-aware CPU pinning" line, starts with
// "EXPERIMENT-ONLY " so that no run can mistake it for the planner's own plan.
// An invalid list: the planner's CPUs are kept and the Note says the knob was
// REJECTED and why.
func applyABPlanKnob(plan CPUPlan, env string) CPUPlan {
	if strings.TrimSpace(env) == "" {
		return plan
	}
	cpus, err := abPlanOverride(env, plan.Allowed)
	if err != nil {
		note := fmt.Sprintf("EXPERIMENT-ONLY %s=%q REJECTED (%v): the planner's own plan is kept (%d loops)",
			ABPlanCPUsEnv, env, err, len(plan.CPUs))
		if plan.Note != "" {
			note += "; " + plan.Note
		}
		plan.Note = note
		return plan
	}
	plan.Note = fmt.Sprintf("EXPERIMENT-ONLY %s=%s (never intended to ship): %d loops, one pinned to each of the CPUs %s, "+
		"replacing the planner's %d loops", ABPlanCPUsEnv, FormatCPUs(cpus), len(cpus), FormatCPUs(cpus), len(plan.CPUs))
	plan.CPUs = cpus
	return plan
}
