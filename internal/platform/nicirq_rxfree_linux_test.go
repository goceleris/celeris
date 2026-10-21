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
	eq(t, "the report remembers its address (the Warn is logged once per address)", got.IRQAddr, clusterAddr)
	noIRQ156(t, got)

	lines := logLines(got)
	warns := warnLines(lines)
	if len(warns) != 1 {
		t.Fatalf("want one Warn, got %d:\n%s", len(warns), strings.Join(lines, "\n"))
	}
	for _, w := range []string{"cpus=4 ", "CPU 4: enp1s0-8 (IRQ 167)", "/proc/irq/167/smp_affinity_list", "NIC interrupts on big.LITTLE hosts"} {
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
		{"the rule's own vectors, no vector beside a loop", nil, []NICVector{v127, v135}, nil},
		{"a foreign vector on a freed CPU is still warned about (no vector has a loop beside it)", nil,
			[]NICVector{v127, v135, {IRQ: 300, CPU: 6, Name: "enp7s0-0", Dev: "enp7s0", Count: 9_000_000}}, []int{6}},
		{"the same foreign vector with IRQ 167 beside CPU 1's loop: rule (b) says no Warn", nil,
			[]NICVector{v127, v135, v167, {IRQ: 300, CPU: 6, Name: "enp7s0-0", Dev: "enp7s0", Count: 9_000_000}}, nil},
		{"a second vector of the receiving port on a freed CPU that the rule did not list", nil,
			[]NICVector{v127, v135, {IRQ: 301, CPU: 8, Name: "enp49s0-9", Dev: "enp49s0", Count: 9_000_000}}, []int{8}},
		{"the vector moved to another no-loop CPU since the rule read it", nil,
			[]NICVector{{IRQ: 135, CPU: 3, Name: "enp49s0-8", Dev: "enp49s0", Count: 4_000_000_000}, v127}, []int{3}},
		{"listed by the rule but its CPU is not freed", func(p *CPUPlan) { p.RxFree.Freed = []int{6} },
			[]NICVector{v127, v135}, []int{8}},
		{"freed CPU but the vector is not the rule's", func(p *CPUPlan) { p.RxFree.Vectors = p.RxFree.Vectors[1:] },
			[]NICVector{v127, v135}, []int{8}},
		{"the rule's vector found on another freed CPU than the rule read it on", nil,
			[]NICVector{{IRQ: 135, CPU: 6, Name: "enp49s0-8", Dev: "enp49s0", Count: 4_000_000_000}}, []int{6}},
		{"no rule, no vector beside a loop", func(p *CPUPlan) { p.RxFree = RxFree{} }, []NICVector{v127, v135}, []int{6, 8}},
		{"no rule, IRQ 167 beside CPU 1's loop: the big CPUs are not named", func(p *CPUPlan) { p.RxFree = RxFree{} },
			[]NICVector{v127, v135, v167}, nil},
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
// plan with the same RxFree, so CPUs 6 and 8 stay unwarned. On the real
// snapshot the cut never makes a Warn: the first loop is CPU 0, and IRQ 159
// (enp1s0-0, 25% of the NICs' interrupts) sits on CPU 0, so a busy vector
// always has a loop beside it, even when the cut drops the loop of CPU 1 that
// IRQ 167 had. (With no vector on CPU 0 the same cut warns for CPU 1:
// TestWithNICIRQsLeavesWhatTheRuleFreedOnPurpose and the io_uring engine test.)
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
		{"memlock holds only CPU 0: CPU 1 loses its loop, IRQ 159 keeps CPU 0's", 1, []int{0}, nil},
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
		})
	}
}

// Where the rule does not run, the plan is #1049's and nothing is excused, but
// a big CPU without a loop is named only when no vector has a loop beside it.
// On msr1 CPU 0 (IRQ 159) and CPU 1 (IRQ 167) keep loops in all of these, so
// CPUs 6 and 8, which the rule's own measurement found harmless without a
// loop, are not named.
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
		{"explicit Workers 4: CPU 8 runs no loop, IRQ 159 and 167 have theirs", 4, true, []int{0, 1, 6, 7}, nil},
		{"GOMAXPROCS 4 (default count under the 8 big CPUs): the same", 4, false, []int{0, 1, 6, 7}, nil},
		{"explicit Workers 1: one loop, on CPU 0 beside IRQ 159", 1, true, []int{0}, nil},
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

// The Warn names the loop CPUs once each, ascending, and offers the ones that
// service no NIC vector: Workers above the big CPUs wraps the loops over them
// (0, 1, 6, 7, 8, 9, 10, 11, 0, 1, 6, 7); CPUs 0, 6 and 8 carry vectors.
func TestReportNamesEachLoopCPUOnce(t *testing.T) {
	_, got := reported(t, msr1NIC(t), 12, true, rng(0, 11), bondAddr)
	eq(t, "loops", got.CPUs, []int{0, 1, 6, 7, 8, 9, 10, 11, 0, 1, 6, 7})
	warns := warnLines(logLines(got))
	if len(warns) != 1 || !strings.Contains(warns[0], "loop_cpus=0-1,6-11 ") || !strings.Contains(warns[0], "one of 1,7,9-11>") {
		t.Errorf("the Warn does not name the loops 0-1,6-11 once and offer 1,7,9-11:\n%s", strings.Join(warns, "\n"))
	}
}

// WithNICIRQs, rules (a) and (b) (celeris#1036 item 1, adopting the premise of
// the rx-port rule): a serviced CPU without a loop is named when it is a little
// CPU (no loop runs there by design), or a big CPU when no reported vector has
// a loop beside it. A big CPU without a loop whose neighbour vector keeps one
// is the state the rule measured fine.
func TestWithNICIRQsLittleCPUOrNoLoopBeside(t *testing.T) {
	plan := CPUPlan{CPUs: []int{0, 1, 7}, Requested: 12, Allowed: rng(0, 11), Little: []int{2, 3, 4, 5}, Heterogeneous: true, Note: "test plan"}
	v := func(irq, cpu int) NICVector {
		return NICVector{IRQ: irq, CPU: cpu, Name: "v" + itoa(irq), Dev: "eth0", Count: 1_000_000}
	}
	for _, tc := range []struct {
		name string
		vecs []NICVector
		want []int
	}{
		{"little CPU, a vector beside a loop elsewhere", []NICVector{v(159, 0), v(167, 4)}, []int{4}},
		{"big CPU without a loop, a vector beside a loop elsewhere", []NICVector{v(159, 0), v(135, 8)}, nil},
		{"two big CPUs without a loop, no vector beside a loop", []NICVector{v(135, 8), v(127, 6)}, []int{6, 8}},
		{"big and little, no vector beside a loop", []NICVector{v(135, 8), v(167, 4)}, []int{4, 8}},
		{"big and little, a vector beside a loop", []NICVector{v(159, 0), v(135, 8), v(167, 4)}, []int{4}},
		{"every vector beside a loop", []NICVector{v(159, 0), v(167, 1), v(300, 7)}, nil},
		{"a little CPU outside the allowed set", []NICVector{v(167, 20)}, nil},
		{"a big CPU outside the allowed set, no loop beside", []NICVector{v(167, 20), v(135, 8)}, []int{8}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := plan.WithNICIRQs(NICIRQs{Vectors: tc.vecs})
			eq(t, "uncovered", got.IRQUncovered, tc.want)
			eq(t, "loops", got.CPUs, plan.CPUs)
		})
	}
}

// A little CPU that runs a loop (a stub plan; the planner never makes one) is
// not uncovered: the Warn is about CPUs without a loop.
func TestWithNICIRQsLittleCPUWithALoopIsCovered(t *testing.T) {
	plan := CPUPlan{CPUs: []int{0, 4}, Requested: 2, Allowed: rng(0, 11), Little: []int{2, 3, 4, 5}, Heterogeneous: true, Note: "stub"}
	got := plan.WithNICIRQs(NICIRQs{Vectors: []NICVector{
		{IRQ: 167, CPU: 4, Name: "enp1s0-8", Dev: "enp1s0", Count: 1_000_000},
		{IRQ: 168, CPU: 3, Name: "enp1s0-9", Dev: "enp1s0", Count: 1_000_000}}})
	eq(t, "uncovered", got.IRQUncovered, []int{3})
}

// MAJOR 1 of the review: the rx-port rule steps aside over a vector on a CPU
// with no loop whatever its share, and the report's share test used to drop
// the same vector when its counter had wrapped. RxFree.NoLoop carries the
// rule's judgement, and the report warns about it.
func TestWithNICIRQsWarnsForTheVectorTheRuleSteppedAsideOver(t *testing.T) {
	plan := func(noLoop ...RxVector) CPUPlan {
		return CPUPlan{CPUs: []int{0, 1, 6, 7, 8, 9, 10, 11}, Requested: 12, Allowed: rng(0, 11), Little: []int{2, 3, 4, 5},
			Heterogeneous: true, Note: "test plan", RxFree: RxFree{NoLoop: noLoop}}
	}
	v167 := RxVector{IRQ: 167, Name: "enp1s0-8", CPU: 4, Count: 40_000_000}
	v159 := NICVector{IRQ: 159, CPU: 0, Name: "enp1s0-0", Dev: "enp1s0", Count: 3_000_000_000}
	for _, tc := range []struct {
		name      string
		p         CPUPlan
		vecs      []NICVector
		uncovered []int
		irqs      []int // IRQs in IRQVectors
	}{
		{"the vector fell under the share", plan(v167), []NICVector{v159}, []int{4}, []int{159, 167}},
		{"the vector is also reported: one entry", plan(v167),
			[]NICVector{v159, {IRQ: 167, CPU: 4, Name: "enp1s0-8", Dev: "enp1s0", Count: 40_000_000}}, []int{4}, []int{159, 167}},
		{"two vectors on two little CPUs", plan(v167, RxVector{IRQ: 300, Name: "enp1s0-9", CPU: 5, Count: 2000}), []NICVector{v159}, []int{4, 5}, []int{159, 167, 300}},
		{"a vector outside the allowed set is not warned about", plan(RxVector{IRQ: 167, Name: "enp1s0-8", CPU: 20, Count: 40_000_000}), []NICVector{v159}, nil, []int{159, 167}},
		{"the rule did not step aside", plan(), []NICVector{v159}, nil, []int{159}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.p.WithNICIRQs(NICIRQs{Vectors: tc.vecs})
			eq(t, "uncovered", got.IRQUncovered, tc.uncovered)
			var irqs []int
			for _, x := range got.IRQVectors {
				irqs = append(irqs, x.IRQ)
			}
			slices.Sort(irqs)
			eq(t, "vectors", irqs, tc.irqs)
		})
	}
}

// The rule records every offending vector, not only the first, and only the
// ones of other ports on a CPU with no loop.
func TestRxFreeRecordsTheVectorsItSteppedAsideOver(t *testing.T) {
	for _, tc := range []struct {
		name string
		snap string
		edit func(h *nicHost)
		want []int // IRQs of RxFree.NoLoop
		cpus []int
	}{
		{"boot placement: IRQ 167 on CPU 4", snapVectorOnCPU4, nil, []int{167}, []int{4}},
		{"IRQ 159 on CPU 3 as well", snapVectorOnCPU4, func(h *nicHost) { h.irq(159, "3", "3") }, []int{159, 167}, []int{3, 4}},
		{"standing state: the rule acts", snapVectorOnCPU1, nil, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := msr1Snapshot(t, tc.snap)
			if tc.edit != nil {
				tc.edit(h)
			}
			got := planWorkerCPUs(12, false, h.planSrc(msr1(t, rng(0, 11)), clusterAddr))
			var irqs, cpus []int
			for _, x := range got.RxFree.NoLoop {
				irqs, cpus = append(irqs, x.IRQ), append(cpus, x.CPU)
			}
			eq(t, "NoLoop IRQs", irqs, tc.want)
			eq(t, "NoLoop CPUs", cpus, tc.cpus)
		})
	}
}

// A wrapped 32-bit counter on the vector that matters. IRQ 167's per-CPU
// counter wrapped to 4e7 (0.36% of the NICs' interrupts): the rule still sees
// it busy (1000 or more) and steps aside, and the Warn must name CPU 4.
func TestReportWrappedCounterOnTheBootPlacementVector(t *testing.T) {
	h := msr1Snapshot(t, snapVectorOnCPU4)
	rewriteRow(t, h.rxHost, 167, map[int]uint64{4: 40_000_000})
	base, got := reportOnSnapshot(t, h, 12, false, rng(0, 11), clusterAddr)
	eq(t, "loops", got.CPUs, msr1Loops1049)
	eq(t, "loops are the planner's", got.CPUs, base.CPUs)
	eq(t, "freed", got.RxFree.Freed, []int(nil))
	if !strings.Contains(whyText(got), "IRQ 167") {
		t.Errorf("the rule did not step aside over IRQ 167: %s", whyText(got))
	}
	eq(t, "uncovered", got.IRQUncovered, []int{4})
	warns := warnLines(logLines(got))
	if len(warns) != 1 || !strings.Contains(warns[0], "cpus=4 ") || !strings.Contains(warns[0], "enp1s0-8 (IRQ 167)") {
		t.Errorf("want one Warn for CPU 4 and IRQ 167:\n%s", strings.Join(warns, "\n"))
	}
	noIRQ156(t, got)
}

// The gaps that stay, pinned: under the floor (999) the rule does not call the
// vector busy either, so it acts (frees 6 and 8) and there is nothing to warn
// about; and where the rule does not run (the default count under the big
// CPUs) a wrapped vector on a little CPU is still dropped by the share test.
func TestReportWrappedCounterGaps(t *testing.T) {
	h := msr1Snapshot(t, snapVectorOnCPU4)
	rewriteRow(t, h.rxHost, 167, map[int]uint64{4: 999})
	_, got := reportOnSnapshot(t, h, 12, false, rng(0, 11), clusterAddr)
	eq(t, "under the floor: the rule acts", got.RxFree.Freed, []int{6, 8})
	eq(t, "under the floor: uncovered", got.IRQUncovered, []int(nil))

	h = msr1Snapshot(t, snapVectorOnCPU4)
	rewriteRow(t, h.rxHost, 167, map[int]uint64{4: 40_000_000})
	_, got = reportOnSnapshot(t, h, 4, false, rng(0, 11), clusterAddr)
	eq(t, "GOMAXPROCS 4: the rule does not run", got.RxFree.NoLoop, []RxVector(nil))
	eq(t, "GOMAXPROCS 4: uncovered", got.IRQUncovered, []int(nil))
}

// The review's scenario for the detail lines: the rule's IRQ 127 on freed CPU
// 6 and another port's IRQ 300 (and four more) on the same CPU, no vector
// beside a loop. The Warn names CPU 6 for the foreign vectors only: not IRQ
// 127, not in the count of vectors past the first four, and not in the list of
// IRQs of the file the operator writes to.
func TestWarnDetailLeavesOutTheVectorsTheRuleFreed(t *testing.T) {
	foreign := func(irq int) NICVector {
		return NICVector{IRQ: irq, CPU: 6, Name: "enp7s0-" + itoa(irq), Dev: "enp7s0", Count: 9_000_000}
	}
	plan := CPUPlan{CPUs: []int{0}, Requested: 12, Allowed: rng(0, 11), Little: []int{2, 3, 4, 5}, Heterogeneous: true, Note: "test plan",
		RxFree: RxFree{Port: "enp49s0", SharePercent: 73, Freed: []int{6, 8}, Vectors: []RxVector{
			{IRQ: 135, Name: "enp49s0-8", CPU: 8, Count: 4_000_000_000}, {IRQ: 127, Name: "enp49s0-0", CPU: 6, Count: 2_000_000_000}}}}
	got := plan.WithNICIRQs(NICIRQs{Vectors: []NICVector{
		{IRQ: 127, CPU: 6, Name: "enp49s0-0", Dev: "enp49s0", Count: 2_000_000_000},
		{IRQ: 135, CPU: 8, Name: "enp49s0-8", Dev: "enp49s0", Count: 4_000_000_000},
		foreign(300), foreign(301), foreign(302), foreign(303), foreign(304),
		{IRQ: 167, CPU: 1, Name: "enp1s0-8", Dev: "enp1s0", Count: 2_600_000_000}}})
	eq(t, "uncovered", got.IRQUncovered, []int{1, 6})
	warns := warnLines(logLines(got))
	if len(warns) != 1 {
		t.Fatalf("want one Warn:\n%s", strings.Join(warns, "\n"))
	}
	w := warns[0]
	for _, want := range []string{"CPU 1: enp1s0-8 (IRQ 167)", "CPU 6: enp7s0-300 (IRQ 300), enp7s0-301 (IRQ 301), enp7s0-302 (IRQ 302), enp7s0-303 (IRQ 303), 1 more",
		"for each IRQ of 167, 300, 301, 302, 303, 304"} {
		if !strings.Contains(w, want) {
			t.Errorf("Warn lacks %q:\n%s", want, w)
		}
	}
	for _, bad := range []string{"IRQ 127", "enp49s0-0", "IRQ 135", "2 more", "127,", ", 127", "135"} {
		if strings.Contains(w, bad) {
			t.Errorf("Warn names %q, a vector the rule left on CPU 6 or 8 on purpose:\n%s", bad, w)
		}
	}
}

// The reviewer's literal scenario under rule (b): IRQ 127 on freed CPU 6, a
// foreign IRQ 300 on CPU 6, and IRQ 167 on CPU 1 beside a loop: no Warn at all.
func TestNoWarnWhenAVectorHasALoopBeside(t *testing.T) {
	plan := CPUPlan{CPUs: []int{0, 1, 7, 9, 10, 11}, Requested: 12, Allowed: rng(0, 11), Little: []int{2, 3, 4, 5}, Heterogeneous: true, Note: "test plan",
		RxFree: RxFree{Port: "enp49s0", SharePercent: 73, Freed: []int{6, 8}, Vectors: []RxVector{
			{IRQ: 135, Name: "enp49s0-8", CPU: 8, Count: 4_000_000_000}, {IRQ: 127, Name: "enp49s0-0", CPU: 6, Count: 2_000_000_000}}}}
	got := plan.WithNICIRQs(NICIRQs{Vectors: []NICVector{
		{IRQ: 127, CPU: 6, Name: "enp49s0-0", Dev: "enp49s0", Count: 2_000_000_000},
		{IRQ: 300, CPU: 6, Name: "enp7s0-0", Dev: "enp7s0", Count: 9_000_000},
		{IRQ: 167, CPU: 1, Name: "enp1s0-8", Dev: "enp1s0", Count: 2_600_000_000}}})
	eq(t, "uncovered", got.IRQUncovered, []int(nil))
	if w := warnLines(logLines(got)); len(w) != 0 {
		t.Errorf("a Warn:\n%s", strings.Join(w, "\n"))
	}
}

// The hint lists the loop CPUs that service no NIC vector: moving the
// interrupt onto CPU 0 (IRQ 159) or onto 6 and 8 (the vectors of enp49s0, and
// the plan the rule makes when they keep a loop was never measured) stacks two
// busy vectors on one CPU.
func TestWarnHintListsLoopCPUsWithoutNICVectors(t *testing.T) {
	h := msr1Snapshot(t, snapVectorOnCPU4)
	_, got := reportOnSnapshot(t, h, 12, false, rng(0, 11), clusterAddr)
	warns := warnLines(logLines(got))
	if len(warns) != 1 {
		t.Fatalf("want one Warn:\n%s", strings.Join(warns, "\n"))
	}
	for _, w := range []string{"echo <one of 1,7,9-11> > /proc/irq/167/smp_affinity_list", "loop_cpus=0-1,6-11 "} {
		if !strings.Contains(warns[0], w) {
			t.Errorf("Warn lacks %q:\n%s", w, warns[0])
		}
	}
	if strings.Contains(warns[0], "0-1,6-11>") || strings.Contains(warns[0], "one of 0") {
		t.Errorf("the hint offers CPUs that service a NIC vector:\n%s", warns[0])
	}
	// Every loop CPU services a NIC vector: nothing to offer.
	plan := CPUPlan{CPUs: []int{0, 6, 8}, Requested: 12, Allowed: rng(0, 11), Little: []int{2, 3, 4, 5}, Heterogeneous: true, Note: "test plan"}
	got = plan.WithNICIRQs(NICIRQs{Vectors: []NICVector{
		{IRQ: 159, CPU: 0, Name: "enp1s0-0", Dev: "enp1s0", Count: 3_000_000_000},
		{IRQ: 127, CPU: 6, Name: "enp49s0-0", Dev: "enp49s0", Count: 2_000_000_000},
		{IRQ: 135, CPU: 8, Name: "enp49s0-8", Dev: "enp49s0", Count: 4_000_000_000},
		{IRQ: 167, CPU: 4, Name: "enp1s0-8", Dev: "enp1s0", Count: 2_600_000_000}}})
	warns = warnLines(logLines(got))
	if len(warns) != 1 || !strings.Contains(warns[0], "every loop CPU services a NIC vector") || strings.Contains(warns[0], "echo <one of") {
		t.Errorf("want the no-free-loop-CPU hint:\n%s", strings.Join(warns, "\n"))
	}
}

// The Warn is logged once for a logger, a listen address and a set of uncovered
// CPUs (the adaptive engine reports for each of its two engines): the repeat is
// a Debug line. Another logger, address or set warns again.
func TestWarnOncePerLoggerAddressAndSet(t *testing.T) {
	mk := func(addr string, vecs ...NICVector) CPUPlan {
		p := CPUPlan{CPUs: []int{0, 1}, Requested: 12, Allowed: rng(0, 11), Little: []int{2, 3, 4, 5}, Heterogeneous: true, Note: "test plan"}
		p = p.WithNICIRQs(NICIRQs{Vectors: vecs})
		p.IRQAddr = addr
		return p
	}
	v := func(irq, cpu int) NICVector {
		return NICVector{IRQ: irq, CPU: cpu, Name: "v" + itoa(irq), Dev: "eth0", Count: 1_000_000}
	}
	newLog := func() (*bytes.Buffer, *slog.Logger) {
		var b bytes.Buffer
		return &b, slog.New(slog.NewTextHandler(&b, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	warnings := func(b *bytes.Buffer) int { return strings.Count(b.String(), "level=WARN") }

	buf, log := newLog()
	p := mk("10.0.0.1:80", v(167, 4))
	LogPinOutcome(log, "epoll", p, nil)
	LogPinOutcome(log, "io_uring", p, nil)
	if n := warnings(buf); n != 1 {
		t.Fatalf("%d Warn lines for the same logger, address and set, want 1:\n%s", n, buf.String())
	}
	if !strings.Contains(buf.String(), "level=DEBUG") || !strings.Contains(buf.String(), "already logged") {
		t.Errorf("the repeat has no Debug line:\n%s", buf.String())
	}
	LogPinOutcome(log, "epoll", mk("10.0.0.2:80", v(167, 4)), nil)
	if n := warnings(buf); n != 2 {
		t.Errorf("another address: %d Warn lines, want 2", n)
	}
	LogPinOutcome(log, "epoll", mk("10.0.0.1:80", v(167, 4), v(168, 5)), nil)
	if n := warnings(buf); n != 3 {
		t.Errorf("another set of CPUs: %d Warn lines, want 3", n)
	}
	buf2, log2 := newLog()
	LogPinOutcome(log2, "epoll", p, nil)
	if n := warnings(buf2); n != 1 {
		t.Errorf("another logger: %d Warn lines, want 1", n)
	}
}

// The memory of the dedupe is bounded: a long-running process that starts
// servers with many addresses does not grow it without limit, and a key
// evicted by the bound warns again (the cost of the bound).
func TestWarnOnceIsBounded(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	mk := func(i int) CPUPlan {
		p := CPUPlan{CPUs: []int{0, 1}, Requested: 12, Allowed: rng(0, 11), Little: []int{2, 3, 4, 5}, Heterogeneous: true, Note: "test plan"}
		p = p.WithNICIRQs(NICIRQs{Vectors: []NICVector{{IRQ: 167, CPU: 4, Name: "v", Dev: "eth0", Count: 1_000_000}}})
		p.IRQAddr = "10.0." + itoa(i/250) + "." + itoa(i%250) + ":80"
		return p
	}
	for i := range 2000 {
		LogPinOutcome(log, "epoll", mk(i), nil)
	}
	if n := strings.Count(buf.String(), "level=WARN"); n != 2000 {
		t.Fatalf("%d Warn lines for 2000 different addresses, want 2000", n)
	}
	before := strings.Count(buf.String(), "level=WARN")
	LogPinOutcome(log, "epoll", mk(0), nil)
	if strings.Count(buf.String(), "level=WARN") != before+1 {
		t.Errorf("the first key was never evicted: the dedupe memory is not bounded")
	}
}
