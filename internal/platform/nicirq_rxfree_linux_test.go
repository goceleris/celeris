//go:build linux

package platform

import (
	"bytes"
	"log/slog"
	"slices"
	"strings"
	"testing"
)

// celeris#1036 item 1: what the NIC interrupt report means once the rx-port
// rule (celeris#1085) exists. The rule leaves the CPUs that service the busy
// vectors of the receiving port without a loop on purpose (CPUs 6 and 8 on
// msr1), and a Warn that says "this CPU services a vector and runs no loop"
// about them would contradict the plan the engine logs one line above it.
// The plans here are made by the real planner over the real msr1 snapshots
// (msr1RxReal); the report is made on the same fake host. The expected loops
// and CPUs are written out by hand from the snapshots and from the rule's
// tests (cpuplan_rxfree_linux_test.go).

// reportOnSnapshot makes the plan the engines make for addr over a snapshot
// (the real planner, rule included) and the report the engines make on it.
func reportOnSnapshot(t *testing.T, h *nicHost, workers int, explicit bool, allowed []int, addr string) (CPUPlan, CPUPlan) {
	t.Helper()
	plan := planWorkerCPUs(workers, explicit, h.planSrc(msr1(t, allowed), addr))
	keep := slices.Clone(plan.CPUs)
	got := annotatePlanNIC(plan, addr, h.nic())
	if !slices.Equal(plan.CPUs, keep) {
		t.Errorf("the input plan was modified: %v, was %v", plan.CPUs, keep)
	}
	unchanged(t, "plan", plan, got)
	return plan, got
}

// logLines returns what LogPinOutcome logs for a plan, at Debug, one entry per
// line.
func logLines(plan CPUPlan) []string {
	var buf bytes.Buffer
	LogPinOutcome(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), "epoll", plan, nil)
	return strings.Split(strings.TrimSpace(buf.String()), "\n")
}

func warnLines(lines []string) []string {
	var out []string
	for _, l := range lines {
		if strings.Contains(l, "level=WARN") {
			out = append(out, l)
		}
	}
	return out
}

// The tuned host: IRQ 167 (enp1s0-8) on CPU 1, which runs a loop. The rule
// frees CPUs 6 and 8 (the busy vectors of enp49s0, the port that took 73% of
// the packets) and the plan is the six loops 0,1,7,9,10,11. The report finds
// the vectors on CPUs 0, 1, 6 and 8, and CPUs 6 and 8 run no loop, but the
// rule left them without one on purpose: nothing is uncovered and nothing is
// warned about.
func TestReportRealMSR1StandingStateWarnsNothing(t *testing.T) {
	for _, addr := range []string{clusterAddr, bondAddr, "[::]:8080", ":8080"} {
		t.Run(addr, func(t *testing.T) {
			h := msr1Snapshot(t, snapVectorOnCPU1)
			base, got := reportOnSnapshot(t, h, 12, false, rng(0, 11), addr)
			eq(t, "loops", got.CPUs, msr1LoopsVNV)
			eq(t, "loops are the planner's", got.CPUs, base.CPUs)
			eq(t, "freed", got.RxFree.Freed, []int{6, 8})
			eq(t, "serviced", got.IRQCPUs, []int{0, 1, 6, 8})
			eq(t, "uncovered", got.IRQUncovered, []int(nil))
			lines := logLines(got)
			if w := warnLines(lines); len(w) != 0 {
				t.Errorf("a Warn at the standing state:\n%s", strings.Join(w, "\n"))
			}
			info := lines[0]
			for _, w := range []string{"level=INFO", "workers=6", "requested=12", "pinned=6", "freed_cpus=6,8", "rx_port=enp49s0",
				"nic_irq_cpus=0-1,6,8", "nic_irq_skipped=60"} {
				if !strings.Contains(info, w) {
					t.Errorf("Info line lacks %q:\n%s", w, info)
				}
			}
		})
	}
}

// The untuned boot placement: IRQ 167 on little CPU 4. A busy vector of the
// other port is serviced by a CPU with no loop, so the rule steps aside (its
// plan is not measured there) and the loops are #1049's eight. The Warn is
// the case it was written for: CPU 4 and IRQ 167, nothing about CPUs 6 and 8
// (they run loops here) or about IRQ 156 (3 interrupts since boot, little CPU 5).
func TestReportRealMSR1BootPlacementWarnsForCPU4(t *testing.T) {
	h := msr1Snapshot(t, snapVectorOnCPU4)
	base, got := reportOnSnapshot(t, h, 12, false, rng(0, 11), clusterAddr)
	eq(t, "loops", got.CPUs, msr1Loops1049)
	eq(t, "loops are the planner's", got.CPUs, base.CPUs)
	eq(t, "freed", got.RxFree.Freed, []int(nil))
	eq(t, "workers", len(got.CPUs), 8)
	eq(t, "serviced", got.IRQCPUs, []int{0, 4, 6, 8})
	eq(t, "uncovered", got.IRQUncovered, []int{4})
	noIRQ156(t, got)

	lines := logLines(got)
	warns := warnLines(lines)
	if len(warns) != 1 {
		t.Fatalf("want one Warn, got %d:\n%s", len(warns), strings.Join(lines, "\n"))
	}
	for _, w := range []string{"cpus=4 ", "CPU 4: enp1s0-8 (IRQ 167)", "/proc/irq/167/smp_affinity_list", "IRQBALANCE_BANNED_CPULIST"} {
		if !strings.Contains(warns[0], w) {
			t.Errorf("Warn lacks %q:\n%s", w, warns[0])
		}
	}
	for _, bad := range []string{"IRQ 156", "enp49s0-29", "CPU 5", "CPU 6", "CPU 8", "IRQ 135", "IRQ 127"} {
		if strings.Contains(warns[0], bad) {
			t.Errorf("Warn names %q:\n%s", bad, warns[0])
		}
	}
	if !strings.Contains(lines[0], "workers=8") || !strings.Contains(lines[0], "nic_irq_skipped=60") || strings.Contains(lines[0], "freed_cpus") {
		t.Errorf("Info line:\n%s", lines[0])
	}
}

// WithNICIRQs on a plan that carries an RxFree. The exclusion is per vector:
// a serviced CPU with no loop is uncovered unless every vector on it is one
// the rule meant to leave there (the vector is in RxFree.Vectors, on a CPU in
// RxFree.Freed). A vector of another port that lands on a freed CPU is not
// excused by it.
func TestWithNICIRQsLeavesWhatTheRuleFreedOnPurpose(t *testing.T) {
	plan := func() CPUPlan {
		return CPUPlan{
			CPUs: []int{0, 1, 7, 9, 10, 11}, Requested: 12, Allowed: rng(0, 11), Little: []int{2, 3, 4, 5}, Heterogeneous: true,
			Note: "test plan",
			RxFree: RxFree{Port: "enp49s0", SharePercent: 73, Freed: []int{6, 8}, Vectors: []RxVector{
				{IRQ: 135, Name: "enp49s0-8", CPU: 8, Count: 4_000_000_000}, {IRQ: 127, Name: "enp49s0-0", CPU: 6, Count: 2_000_000_000}}},
		}
	}
	v135 := NICVector{IRQ: 135, CPU: 8, Name: "enp49s0-8", Dev: "enp49s0", Count: 4_000_000_000}
	v127 := NICVector{IRQ: 127, CPU: 6, Name: "enp49s0-0", Dev: "enp49s0", Count: 2_000_000_000}
	v167 := NICVector{IRQ: 167, CPU: 1, Name: "enp1s0-8", Dev: "enp1s0", Count: 2_600_000_000}
	for _, tc := range []struct {
		name string
		edit func(*CPUPlan)
		vecs []NICVector
		want []int
	}{
		{"the rule's own vectors on its freed CPUs", nil, []NICVector{v127, v135, v167}, nil},
		{"a foreign vector on a freed CPU is still warned about", nil,
			[]NICVector{v127, v135, v167, {IRQ: 300, CPU: 6, Name: "enp7s0-0", Dev: "enp7s0", Count: 9_000_000}}, []int{6}},
		{"a second vector of the receiving port on a freed CPU that the rule did not list", nil,
			[]NICVector{v127, v135, {IRQ: 301, CPU: 8, Name: "enp49s0-9", Dev: "enp49s0", Count: 9_000_000}}, []int{8}},
		{"the vector moved to another no-loop CPU since the rule read it", nil,
			[]NICVector{{IRQ: 135, CPU: 3, Name: "enp49s0-8", Dev: "enp49s0", Count: 4_000_000_000}, v127}, []int{3}},
		{"listed by the rule but its CPU is not freed", func(p *CPUPlan) { p.RxFree.Freed = []int{6} },
			[]NICVector{v127, v135}, []int{8}},
		{"freed CPU but the vector is not the rule's", func(p *CPUPlan) { p.RxFree.Vectors = p.RxFree.Vectors[1:] },
			[]NICVector{v127, v135}, []int{8}},
		{"no rule", func(p *CPUPlan) { p.RxFree = RxFree{} }, []NICVector{v127, v135, v167}, []int{6, 8}},
		{"a vector on a little CPU next to the rule's", nil,
			[]NICVector{v127, v135, {IRQ: 167, CPU: 4, Name: "enp1s0-8", Dev: "enp1s0", Count: 2_600_000_000}}, []int{4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := plan()
			if tc.edit != nil {
				tc.edit(&p)
			}
			got := p.WithNICIRQs(NICIRQs{Vectors: tc.vecs})
			eq(t, "uncovered", got.IRQUncovered, tc.want)
			eq(t, "the loops", got.CPUs, p.CPUs)
			eq(t, "the freed CPUs", got.RxFree.Freed, p.RxFree.Freed)
			for _, v := range tc.vecs {
				if !slices.Contains(got.IRQCPUs, v.CPU) {
					t.Errorf("serviced CPUs %v lack CPU %d: the Info line still names every serviced CPU", got.IRQCPUs, v.CPU)
				}
			}
		})
	}
}

// io_uring's RLIMIT_MEMLOCK cut keeps the rule's decision: Truncate returns a
// plan with the same RxFree, so CPUs 6 and 8 stay unwarned. A cut that drops
// the loop of CPU 1, the CPU the rule meant to keep beside IRQ 167, is a case
// for the Warn: CPU 1 services a busy vector and runs no loop.
func TestReportOnAMemlockCutKeepsTheRuleAndWarnsForTheDroppedLoop(t *testing.T) {
	h := msr1Snapshot(t, snapVectorOnCPU1)
	plan := planWorkerCPUs(12, false, h.planSrc(msr1(t, rng(0, 11)), clusterAddr))
	eq(t, "loops", plan.CPUs, msr1LoopsVNV)
	for _, tc := range []struct {
		name      string
		keep      int
		loops     []int
		uncovered []int
	}{
		{"memlock holds all six", 6, msr1LoopsVNV, nil},
		{"memlock holds 0, 1 and 7: CPU 1 keeps its loop", 3, []int{0, 1, 7}, nil},
		{"memlock holds only CPU 0: CPU 1 loses its loop", 1, []int{0}, []int{1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cut := plan.Truncate(tc.keep, "RLIMIT_MEMLOCK")
			eq(t, "loops", cut.CPUs, tc.loops)
			eq(t, "freed survives the cut", cut.RxFree.Freed, []int{6, 8})
			got := annotatePlanNIC(cut, clusterAddr, h.nic())
			eq(t, "uncovered", got.IRQUncovered, tc.uncovered)
			warns := warnLines(logLines(got))
			if len(tc.uncovered) == 0 && len(warns) != 0 {
				t.Errorf("a Warn:\n%s", strings.Join(warns, "\n"))
			}
			if len(tc.uncovered) > 0 && (len(warns) != 1 || !strings.Contains(warns[0], "cpus=1 ") || strings.Contains(warns[0], "CPU 6") || strings.Contains(warns[0], "CPU 8")) {
				t.Errorf("want one Warn for CPU 1 only:\n%s", strings.Join(warns, "\n"))
			}
		})
	}
}

// Where the rule does not run, the plan is #1049's and the vectors of the
// receiving port are not excused. These pin what the report does today; the
// last two are a decision for the maintainer (see the PR): the rule's own
// measurement says a loop-free CPU 6 or 8 does no harm, and the Warn names
// them when fewer than eight loops are planned.
func TestReportWhereTheRuleDoesNotRun(t *testing.T) {
	for _, tc := range []struct {
		name      string
		workers   int
		explicit  bool
		loops     []int
		uncovered []int
	}{
		{"default count 8 of 8 (the rule runs, for contrast)", 12, false, msr1LoopsVNV, nil},
		{"explicit Workers 8: #1049's eight loops", 8, true, msr1Loops1049, nil},
		{"explicit Workers 6: 0,1,6,7,8,9", 6, true, []int{0, 1, 6, 7, 8, 9}, nil},
		{"explicit Workers 4: CPU 8 runs no loop", 4, true, []int{0, 1, 6, 7}, []int{8}},
		{"GOMAXPROCS 4 (default count under the 8 big CPUs): CPU 8 runs no loop", 4, false, []int{0, 1, 6, 7}, []int{8}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := msr1Snapshot(t, snapVectorOnCPU1)
			_, got := reportOnSnapshot(t, h, tc.workers, tc.explicit, rng(0, 11), clusterAddr)
			eq(t, "loops", got.CPUs, tc.loops)
			eq(t, "uncovered", got.IRQUncovered, tc.uncovered)
		})
	}
}

// A bond that balances its two ports has no receiving port: the rule steps
// aside, the loops are #1049's eight, and nothing is excused or warned about.
// With IRQ 167 on little CPU 4 it is the Warn's case again.
func TestReportBalancedBond(t *testing.T) {
	for _, tc := range []struct {
		name      string
		snap      string
		uncovered []int
	}{
		{"vector on CPU 1", snapVectorOnCPU1, nil},
		{"vector on little CPU 4", snapVectorOnCPU4, []int{4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := msr1Snapshot(t, tc.snap)
			h.rx("enp49s0", 5_000_000_000)
			h.rx("enp1s0", 5_000_000_000)
			_, got := reportOnSnapshot(t, h, 12, false, rng(0, 11), clusterAddr)
			eq(t, "loops", got.CPUs, msr1Loops1049)
			eq(t, "freed", got.RxFree.Freed, []int(nil))
			eq(t, "uncovered", got.IRQUncovered, tc.uncovered)
		})
	}
}
