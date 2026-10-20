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

// With the knob set the plan is forced and the rx-port rule is not consulted. On the real msr1
// snapshot (vector on CPU 1, enp49s0 at 73%) the rule alone frees CPUs 6 and 8 and logs its fields;
// with the knob the line is exactly the one the commits without the rule log for V-NV, with no
// freed_cpus / rx_port / rx_share_pct.
func TestABPlanKnobSkipsTheRxPortRule(t *testing.T) {
	line := func(knob string) (CPUPlan, string) {
		h := msr1RxReal(t, "msr1_rxfree_vector_on_cpu1.txt")
		src := h.planSrc(msr1(t, rng(0, 11)), clusterAddr)
		src.knob = knob
		p := applyABPlanKnob(planWorkerCPUs(12, false, src), knob)
		var buf bytes.Buffer
		LogPinOutcome(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{ReplaceAttr: dropTime})), "epoll", p, nil)
		return p, strings.TrimSpace(buf.String())
	}
	// control: no knob, the rule acts (this is what makes the knob case below non-vacuous)
	p, l := line("")
	if !slices.Equal(p.RxFree.Freed, []int{6, 8}) || !strings.Contains(l, "freed_cpus=6,8") || !strings.Contains(l, "rx_port=enp49s0") {
		t.Fatalf("control: the rule did not act on the msr1 snapshot: %+v\n%s", p.RxFree, l)
	}
	p, l = line("0-1,7,9-11")
	want := `level=INFO msg="epoll: capability-aware CPU pinning" reason="EXPERIMENT-ONLY CELERIS_AB_PLAN_CPUS=0-1,7,9-11 (never intended to ship): 6 loops, one pinned to each of the CPUs 0-1,7,9-11, replacing the planner's 8 loops" allowed=0-11 workers=6 requested=12 pinned=6`
	if l != want {
		t.Fatalf("the knob plan line differs from the V-NV line of the commits without the rule:\n got %s\nwant %s", l, want)
	}
	if !slices.Equal(p.CPUs, []int{0, 1, 7, 9, 10, 11}) || len(p.RxFree.Freed) != 0 || len(p.RxFree.Why) != 0 || p.RxFree.Port != "" {
		t.Fatalf("the rx-port rule ran although the knob forces the plan: %+v %+v", p.CPUs, p.RxFree)
	}
	for _, bad := range []string{"freed_cpus", "rx_port", "rx_share_pct", "interrupt vectors"} {
		if strings.Contains(l, bad) {
			t.Fatalf("knob line carries %q: %s", bad, l)
		}
	}
}
