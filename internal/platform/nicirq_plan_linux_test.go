//go:build linux

package platform

import (
	"bytes"
	"log/slog"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// celeris#1036 item 1: the NIC interrupt report. The expected plans are written
// out by hand from the fixtures (msr1-like: CPUs 0-1 and 6-11 big, 2-5 little,
// the vectors of msr1NIC on CPUs 0, 4, 6 and 8), never taken from the code
// under test. The one property that matters most: the report changes no loop.

// msr1Plan is #1049's plan for the msr1-like host: 8 loops on the big CPUs.
func msr1Plan(t *testing.T, workers int, explicit bool, allowed []int) CPUPlan {
	t.Helper()
	return planWorkerCPUs(workers, explicit, msr1(t, allowed).src())
}

// unchanged fails the test unless got is base plus a NIC interrupt report: with
// the report's fields cleared the two plans are equal, field by field (CPUs,
// Requested, Allowed, Little, Heterogeneous, Note).
func unchanged(t *testing.T, what string, base, got CPUPlan) {
	t.Helper()
	g := got
	g.IRQCPUs, g.IRQVectors, g.IRQUncovered, g.IRQIdle, g.IRQNegligible, g.IRQSkipped, g.IRQReason, g.IRQTook, g.IRQAddr = nil, nil, nil, 0, 0, nil, "", 0, ""
	if !reflect.DeepEqual(g, base) {
		t.Errorf("%s: the report changed the plan:\n got %+v\nwant %+v", what, g, base)
	}
}

// reported annotates #1049's plan with what the fixture host's interrupts say,
// and checks that the plan came back as it went in and that the input was not
// modified. It returns the plan #1049 makes and the annotated one.
func reported(t *testing.T, nh *nicHost, workers int, explicit bool, allowed []int, addr string) (CPUPlan, CPUPlan) {
	t.Helper()
	base := msr1Plan(t, workers, explicit, allowed)
	keep := slices.Clone(base.CPUs)
	got := annotatePlanNIC(base, addr, nh.src())
	if !slices.Equal(base.CPUs, keep) {
		t.Errorf("the input plan was modified: %v, was %v", base.CPUs, keep)
	}
	unchanged(t, "plan", base, got)
	return base, got
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestReportMSR1DefaultWarnsForTheServicedLittleCPU(t *testing.T) {
	// Default Workers on msr1: #1049's eight loops on the big CPUs. The vector
	// of enp1s0-8 sits on little CPU 4, which runs none.
	base, got := reported(t, msr1NIC(t), 12, false, rng(0, 11), bondAddr)
	eq(t, "loops are #1049's", got.CPUs, []int{0, 1, 6, 7, 8, 9, 10, 11})
	eq(t, "workers", len(got.CPUs), len(base.CPUs))
	eq(t, "serviced CPUs", got.IRQCPUs, []int{0, 4, 6, 8})
	eq(t, "uncovered", got.IRQUncovered, []int{4})
	if len(got.IRQVectors) != 4 {
		t.Errorf("vectors %+v", got.IRQVectors)
	}
}

func TestReportNothingUncovered(t *testing.T) {
	// The vector on big CPU 1 (the standing bench pin): every serviced CPU has
	// a loop, nothing is reported.
	nh := msr1NIC(t)
	nh.irq(167, "1", "0-11")
	_, got := reported(t, nh, 12, false, rng(0, 11), bondAddr)
	eq(t, "serviced CPUs", got.IRQCPUs, []int{0, 1, 6, 8})
	eq(t, "uncovered", got.IRQUncovered, []int(nil))
}

func TestReportExplicitWorkers(t *testing.T) {
	// An explicit Workers changes the loops (#1049's rule), never the report's
	// effect on them. CPU 4 (little) runs no loop in any of these; with 4 loops
	// CPU 8 (big) has none either, but IRQ 159 on CPU 0 has a loop beside it,
	// so the big CPUs without a loop are not named: the Warn is for a little
	// CPU, or for a big one when no vector has a loop beside it.
	for _, tc := range []struct {
		workers   int
		loops     []int
		uncovered []int
	}{
		{8, []int{0, 1, 6, 7, 8, 9, 10, 11}, []int{4}},
		{9, []int{0, 1, 6, 7, 8, 9, 10, 11, 0}, []int{4}},
		{12, []int{0, 1, 6, 7, 8, 9, 10, 11, 0, 1, 6, 7}, []int{4}},
		{4, []int{0, 1, 6, 7}, []int{4}},
		{1, []int{0}, []int{4}},
	} {
		base, got := reported(t, msr1NIC(t), tc.workers, true, rng(0, 11), bondAddr)
		eq(t, "loops", got.CPUs, tc.loops)
		eq(t, "base", base.CPUs, tc.loops)
		eq(t, "uncovered", got.IRQUncovered, tc.uncovered)
	}
}

func TestReportDefaultCountBelowTheBigCPUs(t *testing.T) {
	// A default count below the big CPU count (GOMAXPROCS 5): #1049's five
	// loops, whatever the interrupts.
	base, got := reported(t, msr1NIC(t), 5, false, rng(0, 11), bondAddr)
	eq(t, "loops", got.CPUs, base.CPUs)
	eq(t, "five loops", len(got.CPUs), 5)
}

// The memlock cut is #1049's. The report is made on the plan that is left, so a
// serviced little CPU is named, a big one only if no vector has a loop beside
// it (IRQ 159 on CPU 0 does), and nothing is added.
func TestReportOnACutPlan(t *testing.T) {
	base := msr1Plan(t, 12, false, rng(0, 11))
	cut := base.Truncate(3, "RLIMIT_MEMLOCK")
	eq(t, "cut loops", cut.CPUs, []int{0, 1, 6})
	got := annotatePlanNIC(cut, bondAddr, msr1NIC(t).src())
	unchanged(t, "cut plan", cut, got)
	eq(t, "uncovered", got.IRQUncovered, []int{4})
	// The cut after the report: the report's fields travel with the copy and
	// the loops are the cut's.
	full := annotatePlanNIC(base, bondAddr, msr1NIC(t).src())
	cut2 := full.Truncate(3, "RLIMIT_MEMLOCK")
	eq(t, "cut-after loops", cut2.CPUs, []int{0, 1, 6})
	eq(t, "cut-after Note", cut2.Note, base.Truncate(3, "RLIMIT_MEMLOCK").Note)
}

// The report never changes a loop, whatever the host, the count, the cpuset or
// the cut: the matrix of plans against the hosts, each compared with #1049's.
func TestReportNeverChangesThePlan(t *testing.T) {
	hosts := map[string]func() *nicHost{
		"msr1 vector@4": func() *nicHost { return msr1NIC(t) },
		"msr1 vector@1": func() *nicHost { h := msr1NIC(t); h.irq(167, "1", "0-11"); return h },
		"rss":           func() *nicHost { return rssNIC(t) },
		"real block1":   func() *nicHost { return msr1Snapshot(t, snapVectorOnCPU4) },
		"real block2":   func() *nicHost { return msr1Snapshot(t, snapVectorOnCPU1) },
	}
	cpusets := map[string][]int{
		"all":      rng(0, 11),
		"no CPU 4": {0, 1, 2, 3, 5, 6, 7, 8, 9, 10, 11},
		"big only": {0, 1, 6, 7, 8, 9, 10, 11},
	}
	for hn, mk := range hosts {
		for cn, allowed := range cpusets {
			for _, w := range []int{1, 2, 4, 5, 8, 9, 12, 16} {
				for _, explicit := range []bool{false, true} {
					for _, cut := range []int{0, 2, 5} {
						h := mk()
						base := msr1Plan(t, w, explicit, allowed)
						if cut > 0 {
							base = base.Truncate(cut, "RLIMIT_MEMLOCK")
						}
						addr := "10.0.0.1:80"
						if strings.HasPrefix(hn, "msr1") || strings.HasPrefix(hn, "real") {
							addr = bondAddr
						}
						got := annotatePlanNIC(base, addr, h.src())
						unchanged(t, hn+"/"+cn, base, got)
						for _, c := range got.IRQUncovered {
							if slices.Contains(got.CPUs, c) || !slices.Contains(got.Allowed, c) {
								t.Errorf("%s/%s w=%d explicit=%v cut=%d: uncovered CPU %d has a loop or is outside the allowed set",
									hn, cn, w, explicit, cut, c)
							}
						}
					}
				}
			}
		}
	}
}

func TestReportOutsideAllowedIsNotUncovered(t *testing.T) {
	// A cpuset that excludes CPU 4: the vector is serviced outside the set. No
	// loop can run there from inside this process, so it is not an uncovered
	// CPU and no Warn is made; the Info line still names the serviced CPU.
	allowed := []int{0, 1, 2, 3, 5, 6, 7, 8, 9, 10, 11}
	_, got := reported(t, msr1NIC(t), 12, false, allowed, bondAddr)
	eq(t, "serviced CPUs", got.IRQCPUs, []int{0, 4, 6, 8})
	eq(t, "uncovered", got.IRQUncovered, []int(nil))
	var buf bytes.Buffer
	LogPinOutcome(slog.New(slog.NewTextHandler(&buf, nil)), "epoll", got, nil)
	if strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "nic_irq_cpus=0,4,6,8") {
		t.Errorf("log:\n%s", buf.String())
	}
}

func TestReportBoundToOneSlave(t *testing.T) {
	nh := msr1NIC(t)
	nh.addr("enp49s0", true, "10.9.9.9")
	_, got := reported(t, nh, 12, false, rng(0, 11), "10.9.9.9:80")
	eq(t, "serviced", got.IRQCPUs, []int{6, 8})
	eq(t, "uncovered", got.IRQUncovered, []int(nil))
}

func TestReportWildcardAndSpecificAgree(t *testing.T) {
	for _, a := range []string{":8080", "0.0.0.0:8080", "[::]:8080", bondAddr} {
		_, got := reported(t, msr1NIC(t), 12, false, rng(0, 11), a)
		eq(t, a, got.IRQUncovered, []int{4})
	}
	_, got := reported(t, msr1NIC(t), 12, false, rng(0, 11), "127.0.0.1:8080")
	eq(t, "loopback", got.IRQUncovered, []int(nil))
	if len(got.IRQCPUs) != 0 || got.IRQReason == "" {
		t.Errorf("loopback: %+v", got)
	}
}

func TestReportKeepsTheNUMAOrderOfTheBigCPUs(t *testing.T) {
	hp := msr1(t, rng(0, 11))
	hp.nodes = [][]int{{0, 1, 2, 3, 4, 5}, {6, 7, 8, 9, 10, 11}}
	base := planWorkerCPUs(12, false, hp.src())
	eq(t, "#1049 order", base.CPUs, []int{0, 6, 1, 7, 8, 9, 10, 11})
	got := annotatePlanNIC(base, bondAddr, msr1NIC(t).src())
	unchanged(t, "two nodes", base, got)
	eq(t, "uncovered", got.IRQUncovered, []int{4})
	base = planWorkerCPUs(4, true, hp.src())
	eq(t, "#1049 W=4", base.CPUs, []int{0, 6, 1, 7})
	got = annotatePlanNIC(base, bondAddr, msr1NIC(t).src())
	unchanged(t, "two nodes, W=4", base, got)
	eq(t, "uncovered", got.IRQUncovered, []int{4})
}

func TestReportStubPlansAreLeftAlone(t *testing.T) {
	// A plan built by hand (an engine test): loops and Note come back as they
	// went in.
	stub := CPUPlan{CPUs: []int{0, 1, 6}, Allowed: rng(0, 11), Little: rng(2, 5), Heterogeneous: true, Requested: 3, Note: "stub"}
	got := annotatePlanNIC(stub, bondAddr, msr1NIC(t).src())
	unchanged(t, "stub", stub, got)
	eq(t, "uncovered", got.IRQUncovered, []int{4})
}

// An RSS NIC: 16 vectors over all 12 CPUs. Every serviced little CPU that runs
// no loop is named, however many there are.
func rssNIC(t *testing.T) *nicHost {
	t.Helper()
	h := newNICHost(t)
	var irqs []int
	for i := range 16 {
		irqs = append(irqs, 200+i)
	}
	h.phys("eth0", irqs...)
	h.addr("eth0", true, "10.0.0.1")
	// 16 vectors of 19800 interrupts in all: each takes the floor and at least
	// 5%, which is as many as a balanced NIC can have (see
	// TestTrafficBalancedNICWithManyVectors for more).
	counts := map[int]uint64{0: 1000, 1: 1000, 2: 1000, 3: 1500, 4: 2300, 5: 3000, 6: 1000, 7: 1000, 8: 1000, 9: 1000, 10: 1000, 11: 1000}
	for i, irq := range irqs {
		cpu := i % 12
		cnt := counts[cpu]
		if i >= 12 { // the second vector of CPUs 0-3
			cpu = i - 12
			cnt = 1000
		}
		h.irq(irq, itoa(cpu), "")
		h.intr(irq, "eth0-TxRx-"+itoa(i), map[int]uint64{cpu: cnt})
	}
	h.flushInterrupts()
	return h
}

func TestReportRSSNICNamesEveryServicedLittleCPU(t *testing.T) {
	base, got := reported(t, rssNIC(t), 12, false, rng(0, 11), "10.0.0.1:80")
	eq(t, "uncovered", got.IRQUncovered, []int{2, 3, 4, 5})
	eq(t, "loops", got.CPUs, base.CPUs)
	var buf bytes.Buffer
	LogPinOutcome(slog.New(slog.NewTextHandler(&buf, nil)), "epoll", got, nil)
	if n := strings.Count(buf.String(), "level=WARN"); n != 1 {
		t.Errorf("%d Warn lines, want exactly 1:\n%s", n, buf.String())
	}
}

func TestReportLoopbackAndContainerKeepThePlan(t *testing.T) {
	base := msr1Plan(t, 12, false, rng(0, 11))
	got := annotatePlanNIC(base, "127.0.0.1:8080", msr1NIC(t).src())
	unchanged(t, "loopback", base, got)
	// A container: nothing readable.
	c := newNICHost(t)
	c.virt("eth0")
	c.addr("eth0", true, "172.17.0.2")
	got = annotatePlanNIC(base, "172.17.0.2:80", c.src())
	unchanged(t, "container", base, got)
	if len(got.IRQUncovered) != 0 {
		t.Errorf("container: %+v", got.IRQUncovered)
	}
}

func TestReportPanicDegrades(t *testing.T) {
	nh := msr1NIC(t)
	src := nh.src()
	src.ifaceAddrs = func() ([]ifaceAddr, error) { panic("boom") }
	got := detectNICIRQs("192.168.50.199:80", src)
	if len(got.Vectors) != 0 || !strings.Contains(got.Reason, "internal error") {
		t.Errorf("got %+v", got)
	}
	if got := detectNICIRQs("192.168.50.199:80", nicSource{}); len(got.Vectors) != 0 || got.Reason == "" {
		t.Errorf("empty source: %+v", got)
	}
}

// The gate: a host whose CPUs are alike (all amd64) reads nothing, whatever the
// vectors and the loops. Workers 2 of 16 leaves CPUs without a loop, and the
// vectors sit on two of them.
func TestReportHomogeneousHostReadsNothing(t *testing.T) {
	hp := newFakeHost(t, rng(0, 15))
	base := planWorkerCPUs(2, true, hp.src())
	if base.Heterogeneous {
		t.Fatal("fixture is not homogeneous")
	}
	nh := newNICHost(t)
	nh.phys("eth0", 40, 41)
	nh.addr("eth0", true, "10.0.0.1")
	nh.irq(40, "9", "")
	nh.irq(41, "12", "")
	nh.cpus = rng(0, 15)
	nh.intr(40, "eth0-0", map[int]uint64{9: 100000})
	nh.intr(41, "eth0-1", map[int]uint64{12: 100000})
	nh.flushInterrupts()

	got := annotatePlanNIC(base, "10.0.0.1:80", nh.src())
	eq(t, "plan struct", got, base)
	if nh.fs.calls != 0 || nh.ifaceCalls != 0 {
		t.Errorf("a homogeneous host did %d file reads and %d interface lists, want 0 and 0", nh.fs.calls, nh.ifaceCalls)
	}
	// The pure method gates as well.
	irq := NICIRQs{Vectors: []NICVector{{IRQ: 40, CPU: 9, Count: 100000}}}
	eq(t, "WithNICIRQs", base.WithNICIRQs(irq), base)
}

func TestReportEmptyPlanReadsNothing(t *testing.T) {
	nh := msr1NIC(t)
	_ = annotatePlanNIC(CPUPlan{Heterogeneous: true}, bondAddr, nh.src())
	if nh.fs.calls != 0 || nh.ifaceCalls != 0 {
		t.Errorf("an empty plan did %d file reads and %d interface lists", nh.fs.calls, nh.ifaceCalls)
	}
}

// A heterogeneous host does a bounded number of reads: the interface list once,
// then the files of the listener's vectors only (17 reads on the real snapshot,
// bound 40), never one per CPU or per second. The snapshot has 64 vectors on
// two ports.
func TestReportHeterogeneousReadsAreBounded(t *testing.T) {
	nh := msr1Snapshot(t, snapVectorOnCPU4)
	_, _ = reported(t, nh, 12, false, rng(0, 11), clusterAddr)
	t.Logf("64-vector snapshot: %d file reads, %d interface lists", nh.fs.calls, nh.ifaceCalls)
	if nh.fs.calls == 0 || nh.fs.calls > 40 || nh.ifaceCalls != 1 {
		t.Errorf("%d file reads, %d interface lists", nh.fs.calls, nh.ifaceCalls)
	}
}

func TestReportUnknownVectorsAndEmptyResults(t *testing.T) {
	base := msr1Plan(t, 12, false, rng(0, 11))
	got := base.WithNICIRQs(NICIRQs{Reason: "x", Vectors: []NICVector{{IRQ: 1, CPU: -1}}})
	unchanged(t, "unknown", base, got)
	eq(t, "reason", got.IRQReason, "x")
	if len(got.IRQCPUs) != 0 || len(got.IRQUncovered) != 0 {
		t.Errorf("got %+v", got)
	}
}

func TestLogPinOutcomeNICIRQ(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	nh := msr1NIC(t)

	// The default plan: eight loops, CPU 4 services a vector and runs none: one
	// Info line and one Warn line, and the Info line counts no NIC interrupt
	// loop.
	_, plan := reported(t, nh, 12, false, rng(0, 11), bondAddr)
	LogPinOutcome(log, "epoll", plan, nil)
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want one Info and one Warn line, got:\n%s", buf.String())
	}
	for _, w := range []string{"level=INFO", "nic_irq_cpus=0,4,6,8", "irq_took=", "workers=8"} {
		if !strings.Contains(lines[0], w) {
			t.Errorf("Info line lacks %q:\n%s", w, lines[0])
		}
	}
	if strings.Contains(lines[0], "irq_loops") {
		t.Errorf("Info line counts NIC interrupt loops:\n%s", lines[0])
	}
	// The Warn is short: the vector, the CPU, the loop CPUs, one line of action
	// (the loop CPUs that service no NIC vector: 1, 7 and 9-11, not CPU 0
	// with IRQ 159 and not 6 and 8 with the vectors of enp49s0), and the README.
	for _, w := range []string{"level=WARN", "serviced by CPUs that run no loop", "cpus=4 ", "CPU 4: enp1s0-8 (IRQ 167)",
		"loop_cpus=0-1,6-11", "none was added",
		"echo <one of 1,7,9-11> > /proc/irq/167/smp_affinity_list", `NIC interrupts on big.LITTLE hosts`} {
		if !strings.Contains(lines[1], w) {
			t.Errorf("Warn line lacks %q:\n%s", w, lines[1])
		}
	}
	for _, bad := range []string{"IRQBALANCE", "Realtek", "coincided", "~200 ms", "Route", "removed"} {
		if strings.Contains(lines[1], bad) {
			t.Errorf("Warn line carries %q, which belongs in the README:\n%s", bad, lines[1])
		}
	}
	if len(lines[1]) > 700 {
		t.Errorf("Warn line is %d bytes, want at most 700:\n%s", len(lines[1]), lines[1])
	}

	// Nothing uncovered: no Warn. Nothing found: one Debug line with the
	// reason.
	buf.Reset()
	nh.irq(167, "1", "0-11")
	_, plan = reported(t, nh, 12, false, rng(0, 11), bondAddr)
	LogPinOutcome(log, "epoll", plan, nil)
	if strings.Contains(buf.String(), "WARN") {
		t.Errorf("a covered vector warned:\n%s", buf.String())
	}
	buf.Reset()
	_, plan = reported(t, nh, 12, false, rng(0, 11), "127.0.0.1:80")
	LogPinOutcome(log, "epoll", plan, nil)
	if !strings.Contains(buf.String(), "level=DEBUG") || !strings.Contains(buf.String(), "nic irq: no vectors found (loopback listen address)") {
		t.Errorf("want the Debug line:\n%s", buf.String())
	}
	// A plan with no report logs nothing about interrupts.
	buf.Reset()
	LogPinOutcome(log, "epoll", CPUPlan{CPUs: []int{0, 1}, Allowed: []int{0, 1}}, nil)
	if buf.Len() != 0 {
		t.Errorf("a plan without a report logged:\n%s", buf.String())
	}
}

// On the host the tests run on: a plan whose CPUs are alike (every amd64 host)
// goes through the real entry point untouched, reading nothing. Where the test
// host is big.LITTLE the plan is heterogeneous and the test has nothing to say.
func TestAnnotatePlanWithNICIRQsRealHostAlike(t *testing.T) {
	p := PlanWorkerCPUs(4, false)
	if p.Heterogeneous {
		t.Skip("this host's CPUs differ in capacity")
	}
	eq(t, "plan", AnnotatePlanWithNICIRQs(p, ":0"), p)
	eq(t, "plan, 0.0.0.0", AnnotatePlanWithNICIRQs(p, "0.0.0.0:8080"), p)
}

func TestLogPinOutcomeNICIRQWording(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// The Warn names the vector and the file the operator writes to.
	nh := msr1NIC(t)
	_, plan := reported(t, nh, 12, false, rng(0, 11), bondAddr)
	LogPinOutcome(log, "epoll", plan, nil)
	out := buf.String()
	for _, w := range []string{"enp1s0-8 (IRQ 167)", "/proc/irq/167/smp_affinity_list"} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}

	// More vectors on the CPU than the detail names: the rest is counted.
	buf.Reset()
	h := newNICHost(t)
	var irqs []int
	for i := range 6 {
		irqs = append(irqs, 300+i)
	}
	h.phys("eth0", irqs...)
	h.addr("eth0", true, "10.0.0.1")
	for i, irq := range irqs {
		h.irq(irq, "4", "")
		h.intr(irq, "eth0-"+itoa(i), map[int]uint64{4: 100000})
	}
	h.flushInterrupts()
	_, plan = reported(t, h, 12, false, rng(0, 11), "10.0.0.1:80")
	LogPinOutcome(log, "epoll", plan, nil)
	if !strings.Contains(buf.String(), "2 more") {
		t.Errorf("the Warn does not count the vectors past the first four:\n%s", buf.String())
	}
}
