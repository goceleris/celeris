//go:build linux

package iouring

import (
	"bytes"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/goceleris/celeris/internal/platform"
)

// celeris#1036: the plan comes first and RLIMIT_MEMLOCK bounds the plan. On a
// big.LITTLE host whose memlock ceiling lies between the non-little CPU count
// and GOMAXPROCS the cap used to run on the request: it warned that memlock
// held the engine down although the plan started fewer loops than the ceiling
// funds, and the log line carried the capped number as "requested". No ring is
// built here: the planner and the ceiling are stood in.
func TestPlanIsCappedByMemlockNotTheRequest1036(t *testing.T) {
	cpus := []int{0, 1, 6, 7, 8, 9, 10, 11}
	stubPlan := func(held *int) {
		old := planWorkerCPUs
		t.Cleanup(func() { planWorkerCPUs = old })
		planWorkerCPUs = func(n int, explicit bool, _ string) platform.CPUPlan {
			*held = n
			// 3 loops (a big.LITTLE host with 3 non-little CPUs), never more
			// than asked for.
			k := min(n, 3)
			return platform.CPUPlan{CPUs: slices.Clone(cpus[:k]), Allowed: cpus, Requested: n, Heterogeneous: true,
				Note: "test plan: 3 loops, one per other allowed CPU"}
		}
	}
	ceiling := func(c int) {
		old := memlockCeiling
		t.Cleanup(func() { memlockCeiling = old })
		memlockCeiling = func() int { return c }
	}

	for _, tc := range []struct {
		name        string
		ceiling     int
		wantCPUs    []int
		wantWarn    bool
		wantNoteCut bool
	}{
		{"ceiling between the plan and the request: nothing binds", 5, cpus[:3], false, false},
		{"ceiling at the plan: nothing binds", 3, cpus[:3], false, false},
		{"ceiling below the plan: it binds", 2, cpus[:2], true, true},
		{"one worker funded", 1, cpus[:1], true, true},
		{"no ceiling", -1, cpus[:3], false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked int
			stubPlan(&asked)
			ceiling(tc.ceiling)
			var buf bytes.Buffer
			log := slog.New(slog.NewTextHandler(&buf, nil))
			plan := planWithinMemlock(6, false, "", log)
			if asked != 6 {
				t.Errorf("the planner was asked for %d workers, want the request, 6", asked)
			}
			if !slices.Equal(plan.CPUs, tc.wantCPUs) {
				t.Errorf("plan CPUs %v, want %v", plan.CPUs, tc.wantCPUs)
			}
			warned := strings.Contains(buf.String(), "capped by RLIMIT_MEMLOCK")
			if warned != tc.wantWarn {
				t.Errorf("memlock warning logged = %v, want %v:\n%s", warned, tc.wantWarn, buf.String())
			}
			if tc.wantWarn {
				// The warning speaks of the plan's loops, not of the request.
				for _, w := range []string{"requested=3", "capped_to=" + strconv.Itoa(tc.ceiling)} {
					if !strings.Contains(buf.String(), w) {
						t.Errorf("warning lacks %q:\n%s", w, buf.String())
					}
				}
			}
			// The plan's own line must not contradict the count that starts.
			buf.Reset()
			platform.LogPinOutcome(log, "io_uring", plan, nil)
			out := buf.String()
			if !strings.Contains(out, "workers="+strconv.Itoa(len(tc.wantCPUs))+" ") || !strings.Contains(out, "requested=6") ||
				!strings.Contains(out, "pinned="+strconv.Itoa(len(tc.wantCPUs))) {
				t.Errorf("plan line does not carry workers=%d requested=6 pinned=%d:\n%s", len(tc.wantCPUs), len(tc.wantCPUs), out)
			}
			cut := strings.Contains(out, "only "+strconv.Itoa(len(tc.wantCPUs))+" of the 3 planned loops start")
			if cut != tc.wantNoteCut {
				t.Errorf("the note says the plan was cut = %v, want %v:\n%s", cut, tc.wantNoteCut, out)
			}
		})
	}
}

// Planning first and cutting to the ceiling is the plan the planner makes for
// the capped count (what the engine did before, where the CPUs are alike).
func TestPlanThenCapEqualsCappedPlanWhenCPUsAreAlike1036(t *testing.T) {
	for _, c := range []int{1, 2, 3, 5} {
		old := memlockCeiling
		memlockCeiling = func() int { return c }
		got := planWithinMemlock(8, true, "", nil)
		memlockCeiling = old
		want := platform.PlanWorkerCPUs(c, true)
		if !slices.Equal(got.CPUs, want.CPUs) {
			t.Errorf("ceiling %d: plan then cap = %v, the plan for the capped count = %v", c, got.CPUs, want.CPUs)
		}
	}
}
