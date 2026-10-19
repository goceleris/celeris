package platform

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// EXPERIMENT-ONLY, NEVER INTENDED TO SHIP (throwaway branch ab/pinplan-v4-*,
// probatorium PINPLAN4). ABPlanCPUsEnv names an environment variable that, when
// it is set to a non-empty value, replaces the CPUs the planner pins its loops
// to: one loop per listed CPU, each pinned. Unset or empty it does nothing at
// all (the plan is returned untouched). A benchmark arm sets it from a constant
// of its own server code, so that two arms of one A/B differ in nothing but
// that list; the design that would ship, if one of the lists wins, is defined
// separately and is not this knob.
const ABPlanCPUsEnv = "CELERIS_AB_PLAN_CPUS"

// abPlanOverride parses a kernel-style CPU list ("0-2,5-11") and checks it
// against the allowed CPUs. It returns the listed CPUs ascending, one loop
// each, or an error for anything it does not take as is: an empty list, a
// token that is not a number or an a-b range, a descending range, a repeated
// CPU, or a CPU outside allowed. The caller turns the error into a plan line
// that says the knob was REJECTED; there is no silent fallback.
func abPlanOverride(list string, allowed []int) ([]int, error) {
	list = strings.TrimSpace(list)
	if list == "" {
		return nil, fmt.Errorf("empty CPU list")
	}
	var cpus []int
	for _, tok := range strings.Split(list, ",") {
		tok = strings.TrimSpace(tok)
		lo, hi := tok, tok
		if a, b, ok := strings.Cut(tok, "-"); ok {
			lo, hi = strings.TrimSpace(a), strings.TrimSpace(b)
		}
		l, err1 := strconv.Atoi(lo)
		h, err2 := strconv.Atoi(hi)
		if err1 != nil || err2 != nil || l < 0 || h < l {
			return nil, fmt.Errorf("bad CPU list token %q", tok)
		}
		for c := l; c <= h; c++ {
			cpus = append(cpus, c)
		}
	}
	slices.Sort(cpus)
	for i, c := range cpus {
		if i > 0 && cpus[i-1] == c {
			return nil, fmt.Errorf("CPU %d listed twice", c)
		}
		if !slices.Contains(allowed, c) {
			return nil, fmt.Errorf("CPU %d is not in the allowed CPUs %v", c, allowed)
		}
	}
	return cpus, nil
}
