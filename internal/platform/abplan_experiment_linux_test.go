//go:build linux

package platform

import (
	"bytes"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The EXPERIMENT-ONLY knob on an msr1-like fake host (12 CPUs, little 2-5).
func abKnobPlan(t *testing.T, workers int, explicit bool) CPUPlan {
	return planWorkerCPUs(workers, explicit, msr1(t, rng(0, 11)).src())
}

func TestABPlanKnobUnsetIsANoOp(t *testing.T) {
	for _, workers := range []int{0, 1, 8, 12, 16} {
		for _, explicit := range []bool{false, true} {
			p := abKnobPlan(t, workers, explicit)
			for _, env := range []string{"", "   "} {
				if got := applyABPlanKnob(p, env); !reflect.DeepEqual(got, p) {
					t.Fatalf("workers=%d explicit=%v env=%q: the plan changed:\n got %+v\nwant %+v", workers, explicit, env, got, p)
				}
			}
		}
	}
}

func TestABPlanKnobReplacesTheLoopCPUs(t *testing.T) {
	p := abKnobPlan(t, 12, false)
	if !slices.Equal(p.CPUs, bigCPUs1036) {
		t.Fatalf("premise: the planner's plan is %v, want the 8 big CPUs", p.CPUs)
	}
	for _, tc := range []struct {
		name, env string
		want      []int
	}{
		{"V-L2", "0-2,5-11", []int{0, 1, 2, 5, 6, 7, 8, 9, 10, 11}},
		{"V-NV", "0-1,7,9-11", []int{0, 1, 7, 9, 10, 11}},
	} {
		q := applyABPlanKnob(p, tc.env)
		if !slices.Equal(q.CPUs, tc.want) {
			t.Errorf("%s: CPUs %v, want %v", tc.name, q.CPUs, tc.want)
		}
		if q.Pinned() != len(tc.want) || q.Requested != p.Requested || !slices.Equal(q.Allowed, p.Allowed) || !slices.Equal(q.Little, p.Little) || !q.Heterogeneous {
			t.Errorf("%s: pinned/requested/allowed/little changed: %+v", tc.name, q)
		}
		if !strings.HasPrefix(q.Note, "EXPERIMENT-ONLY CELERIS_AB_PLAN_CPUS="+tc.env+" ") {
			t.Errorf("%s: note does not start with the experiment marker and the list: %q", tc.name, q.Note)
		}
		// the input plan is not modified in place (its slice is not shared)
		if !slices.Equal(p.CPUs, bigCPUs1036) {
			t.Fatalf("%s: the planner's plan was modified in place: %v", tc.name, p.CPUs)
		}
	}
}

// An invalid list is never a silent fallback: the planner's CPUs are kept and the line says REJECTED.
func TestABPlanKnobRejectedIsLoud(t *testing.T) {
	p := abKnobPlan(t, 12, false)
	for _, env := range []string{"0-12", "x", "3-1", "1,1", "0-1,12"} {
		q := applyABPlanKnob(p, env)
		if !slices.Equal(q.CPUs, p.CPUs) {
			t.Errorf("env %q: CPUs changed to %v", env, q.CPUs)
		}
		if !strings.HasPrefix(q.Note, "EXPERIMENT-ONLY CELERIS_AB_PLAN_CPUS=") || !strings.Contains(q.Note, "REJECTED") {
			t.Errorf("env %q: note %q does not say REJECTED", env, q.Note)
		}
	}
}

// The plan lines a PINPLAN4 arm logs, printed (-v) and asserted: the validator's expectations are generated from these, not typed.
func TestABPlanKnobPlanLines(t *testing.T) {
	p := abKnobPlan(t, 12, false)
	for _, tc := range []struct{ name, env string }{{"V-L2", "0-2,5-11"}, {"V-NV", "0-1,7,9-11"}} {
		var buf bytes.Buffer
		LogPinOutcome(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{ReplaceAttr: dropTime})), "epoll", applyABPlanKnob(p, tc.env), nil)
		line := strings.TrimSpace(buf.String())
		t.Logf("%s PLANLINE %s", tc.name, line)
		for _, want := range []string{"epoll: capability-aware CPU pinning", `allowed=0-11`, `requested=12`, "EXPERIMENT-ONLY CELERIS_AB_PLAN_CPUS=" + tc.env} {
			if !strings.Contains(line, want) {
				t.Errorf("%s: line lacks %q: %s", tc.name, want, line)
			}
		}
	}
}

func dropTime(_ []string, a slog.Attr) slog.Attr {
	if a.Key == slog.TimeKey {
		return slog.Attr{}
	}
	return a
}

// The real entry point reads the environment at call time (after a program's main has set it).
func TestPlanWorkerCPUsReadsTheKnobAtCallTime(t *testing.T) {
	allowed, err := schedAllowedCPUs()
	if err != nil {
		t.Skipf("no allowed CPUs: %v", err)
	}
	t.Setenv(ABPlanCPUsEnv, FormatCPUs(allowed[:1]))
	p := PlanWorkerCPUs(4, false)
	if !slices.Equal(p.CPUs, allowed[:1]) || !strings.HasPrefix(p.Note, "EXPERIMENT-ONLY ") {
		t.Fatalf("PlanWorkerCPUs ignored the knob: %+v", p)
	}
	t.Setenv(ABPlanCPUsEnv, "")
	q := PlanWorkerCPUs(4, false)
	if strings.Contains(q.Note, "EXPERIMENT-ONLY") || len(q.CPUs) == 0 {
		t.Fatalf("PlanWorkerCPUs with the knob empty is not the planner's plan: %+v", q)
	}
}
