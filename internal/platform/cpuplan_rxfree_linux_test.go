//go:build linux

package platform

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
)

// celeris#1036 item 1, the rx-port rule: on a big.LITTLE host the default
// loops stay off the big CPUs that service the interrupt vectors of the NIC
// port that receives the traffic. The fixtures are the real msr1 snapshots
// (testdata/msr1_rxfree_*.txt) and synthetic hosts for the shapes msr1 does not
// have.

var (
	msr1Loops1049 = []int{0, 1, 6, 7, 8, 9, 10, 11}
	msr1LoopsVNV  = []int{0, 1, 7, 9, 10, 11}
)

// same1049 asserts that got is the plan #1049 makes for the same request: the
// loops, the note and the classification, and no CPU freed. Why (the Debug
// text) may differ.
func same1049(t *testing.T, what string, got, want CPUPlan) {
	t.Helper()
	eq(t, what+": loops", got.CPUs, want.CPUs)
	eq(t, what+": note", got.Note, want.Note)
	eq(t, what+": allowed", got.Allowed, want.Allowed)
	eq(t, what+": little", got.Little, want.Little)
	eq(t, what+": heterogeneous", got.Heterogeneous, want.Heterogeneous)
	eq(t, what+": requested", got.Requested, want.Requested)
	if len(got.RxFree.Freed) != 0 || got.RxFree.Port != "" {
		t.Errorf("%s: the rule freed %v on %q", what, got.RxFree.Freed, got.RxFree.Port)
	}
}

func whyText(p CPUPlan) string { return strings.Join(p.RxFree.Why, " | ") }

// ---- the real msr1 snapshots ------------------------------------------------

// IRQ 167 (enp1s0-8, the TX completion of the port that receives nothing) on
// CPU 1, which runs a loop: the tuned host. enp49s0 took 73% of the packets;
// its two busy vectors are IRQ 127 on CPU 6 and IRQ 135 on CPU 8. The six
// loops are the V-NV arm of the PINPLAN v4/v5 runs.
func TestRxFreeMSR1VectorOnCPU1(t *testing.T) {
	for _, addr := range []string{clusterAddr, bondAddr, "[::]:8080", ":8080"} {
		t.Run(addr, func(t *testing.T) {
			cpu := msr1(t, rng(0, 11))
			h := msr1RxReal(t, "msr1_rxfree_vector_on_cpu1.txt")
			got := planWorkerCPUs(12, false, h.planSrc(cpu, addr))
			eq(t, "loops", got.CPUs, msr1LoopsVNV)
			eq(t, "workers", len(got.CPUs), 6)
			eq(t, "freed", got.RxFree.Freed, []int{6, 8})
			eq(t, "port", got.RxFree.Port, "enp49s0")
			eq(t, "share", got.RxFree.SharePercent, 73)
			eq(t, "little", got.Little, []int{2, 3, 4, 5})
			if !got.Heterogeneous {
				t.Error("not heterogeneous")
			}
			var irqs, cpus []int
			for _, v := range got.RxFree.Vectors {
				irqs, cpus = append(irqs, v.IRQ), append(cpus, v.CPU)
			}
			// Busiest first: IRQ 135 (4.0e9) before IRQ 127 (2.3e9).
			eq(t, "vectors", irqs, []int{135, 127})
			eq(t, "vector cpus", cpus, []int{8, 6})
			for _, w := range []string{"6,8", "enp49s0", "73%", "enp49s0-8", "IRQ 135", "6 loops"} {
				if !strings.Contains(got.Note, w) {
					t.Errorf("Note lacks %q:\n%s", w, got.Note)
				}
			}
		})
	}
}

// IRQ 167 on little CPU 4, the boot placement: a busy vector of the other
// port sits on a CPU that runs no loop, so the rule steps aside and the plan
// is #1049's eight loops, CPU 8 (the TX completion of enp49s0) among them. The
// rule does not know which of the port's two CPUs would need the loop, and
// removing both is the configuration no cluster run has measured.
func TestRxFreeMSR1VectorOnCPU4StepsAside(t *testing.T) {
	cpu := msr1(t, rng(0, 11))
	h := msr1RxReal(t, "msr1_rxfree_vector_on_cpu4.txt")
	got := planWorkerCPUs(12, false, h.planSrc(cpu, clusterAddr))
	same1049(t, "vector@4", got, planWorkerCPUs(12, false, cpu.src()))
	eq(t, "loops", got.CPUs, msr1Loops1049)
	why := whyText(got)
	for _, w := range []string{"IRQ 167", "enp1s0-8", "CPU 4", "no loop"} {
		if !strings.Contains(why, w) {
			t.Errorf("Why lacks %q: %s", w, why)
		}
	}
}

// A hand edit of the vector@1 snapshot: IRQ 167 on CPU 8, which also
// services IRQ 135. A CPU that hosts a busy vector of another port keeps its
// loop, so only CPU 6 is freed. (Nothing in the real snapshots tells the
// clause apart: the two ports' CPUs {6,8} and {0,1} do not meet.)
func TestRxFreeOtherPortVectorKeepsItsLoop(t *testing.T) {
	cpu := msr1(t, rng(0, 11))
	h := msr1RxReal(t, "msr1_rxfree_vector_on_cpu1.txt")
	h.irq(167, "8", "8")
	got := planWorkerCPUs(12, false, h.planSrc(cpu, clusterAddr))
	eq(t, "loops", got.CPUs, []int{0, 1, 7, 8, 9, 10, 11})
	eq(t, "freed", got.RxFree.Freed, []int{6})
	if !strings.Contains(whyText(got), "CPU 8") {
		t.Errorf("Why does not name the protected CPU 8: %s", whyText(got))
	}
}

// IRQ 135's 32-bit counter wrapped and reads under 1000: it is no busy
// vector, so only CPU 6 is freed (the documented blind spot).
func TestRxFreeWrappedCounterFreesTheOtherCPUOnly(t *testing.T) {
	cpu := msr1(t, rng(0, 11))
	h := msr1RxReal(t, "msr1_rxfree_vector_on_cpu1.txt")
	rewriteRow(t, h, 135, map[int]uint64{8: 999})
	got := planWorkerCPUs(12, false, h.planSrc(cpu, clusterAddr))
	eq(t, "loops", got.CPUs, []int{0, 1, 7, 8, 9, 10, 11})
	eq(t, "freed", got.RxFree.Freed, []int{6})
	// 1000 is busy again.
	rewriteRow(t, h, 135, map[int]uint64{8: 1000})
	got = planWorkerCPUs(12, false, h.planSrc(cpu, clusterAddr))
	eq(t, "loops at the floor", got.CPUs, msr1LoopsVNV)
}

// rewriteRow replaces the per-CPU counts of one numbered row of the fake
// /proc/interrupts, keeping the description.
func rewriteRow(t *testing.T, h *rxHost, irq int, counts map[int]uint64) {
	t.Helper()
	b, err := h.fs.inner.readFile(h.path("proc/interrupts"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(b), "\n")
	prefix := fmt.Sprintf("%d:", irq)
	for i, l := range lines {
		if !strings.HasPrefix(strings.TrimSpace(l), prefix) {
			continue
		}
		fields := strings.Fields(l)
		var sb strings.Builder
		fmt.Fprintf(&sb, "%3d:", irq)
		for c := 0; c < 12; c++ {
			fmt.Fprintf(&sb, " %10d", counts[c])
		}
		sb.WriteString(" " + strings.Join(fields[13:], " "))
		lines[i] = sb.String()
		h.write("proc/interrupts", strings.Join(lines, "\n"))
		return
	}
	t.Fatalf("no row for IRQ %d", irq)
}

// An explicit Config.Workers is planned as #1049 plans it, and the NIC files
// are not read at all.
func TestRxFreeExplicitWorkersAreUntouched(t *testing.T) {
	for _, snap := range []string{"msr1_rxfree_vector_on_cpu1.txt", "msr1_rxfree_vector_on_cpu4.txt"} {
		for _, n := range []int{3, 6, 8, 12, 17} {
			cpu := msr1(t, rng(0, 11))
			h := msr1RxReal(t, snap)
			got := planWorkerCPUs(n, true, h.planSrc(cpu, clusterAddr))
			same1049(t, fmt.Sprintf("%s explicit %d", snap, n), got, planWorkerCPUs(n, true, cpu.src()))
			if h.reads() != 0 {
				t.Errorf("%s explicit %d: %d NIC reads, want none", snap, n, h.reads())
			}
			if len(got.RxFree.Why) != 0 {
				t.Errorf("explicit %d: the rule ran: %v", n, got.RxFree.Why)
			}
		}
	}
	// 12 explicit loops on the eight big CPUs wrap exactly as #1049 wraps them.
	cpu := msr1(t, rng(0, 11))
	h := msr1RxReal(t, "msr1_rxfree_vector_on_cpu1.txt")
	eq(t, "explicit 12", planWorkerCPUs(12, true, h.planSrc(cpu, clusterAddr)).CPUs,
		[]int{0, 1, 6, 7, 8, 9, 10, 11, 0, 1, 6, 7})
}

// The default count below the big CPUs (GOMAXPROCS 4, the default under a
// Kubernetes CPU limit) is planned as #1049 plans it and reads no NIC file: the
// rule is measured with one loop on each of the 8 big CPUs, and with fewer loops
// a CPU it meant to keep may run none (celeris#1036 item 1). From 8 loops up
// the default count never exceeds #1049's and never falls under the floor.
func TestRxFreeNeverRaisesTheCount(t *testing.T) {
	cpu := msr1(t, rng(0, 11))
	for n := 1; n <= 16; n++ {
		h := msr1RxReal(t, "msr1_rxfree_vector_on_cpu1.txt")
		base := planWorkerCPUs(n, false, cpu.src())
		got := planWorkerCPUs(n, false, h.planSrc(cpu, clusterAddr))
		if n < 8 {
			same1049(t, fmt.Sprintf("n=%d", n), got, base)
			if h.reads() != 0 {
				t.Errorf("n=%d: %d NIC reads, want none", n, h.reads())
			}
			continue
		}
		if len(got.CPUs) > len(base.CPUs) {
			t.Errorf("n=%d: %d loops, #1049 plans %d", n, len(got.CPUs), len(base.CPUs))
		}
		if len(got.CPUs) < min(len(base.CPUs), 6) {
			t.Errorf("n=%d: %d loops, under the floor of min(%d, 6)", n, len(got.CPUs), len(base.CPUs))
		}
		for _, c := range got.CPUs {
			if c == 6 || c == 8 || c < 0 {
				t.Errorf("n=%d: a loop on CPU %d: %v", n, c, got.CPUs)
			}
		}
	}
	h := msr1RxReal(t, "msr1_rxfree_vector_on_cpu1.txt")
	eq(t, "4 workers", planWorkerCPUs(4, false, h.planSrc(cpu, clusterAddr)).CPUs, []int{0, 1, 6, 7})
	eq(t, "6 workers", planWorkerCPUs(6, false, h.planSrc(cpu, clusterAddr)).CPUs, []int{0, 1, 6, 7, 8, 9})
	eq(t, "7 workers", planWorkerCPUs(7, false, h.planSrc(cpu, clusterAddr)).CPUs, []int{0, 1, 6, 7, 8, 9, 10})
	eq(t, "8 workers", planWorkerCPUs(8, false, h.planSrc(cpu, clusterAddr)).CPUs, msr1LoopsVNV)
	eq(t, "0 workers", planWorkerCPUs(0, false, h.planSrc(cpu, clusterAddr)).CPUs, []int{})
}

// A plan cut for RLIMIT_MEMLOCK keeps the order of the rule's loops.
func TestRxFreeTruncateKeepsTheOrder(t *testing.T) {
	cpu := msr1(t, rng(0, 11))
	h := msr1RxReal(t, "msr1_rxfree_vector_on_cpu1.txt")
	full := planWorkerCPUs(12, false, h.planSrc(cpu, clusterAddr))
	for n := 1; n < 6; n++ {
		cut := full.Truncate(n, "RLIMIT_MEMLOCK")
		eq(t, fmt.Sprintf("cut to %d", n), cut.CPUs, msr1LoopsVNV[:n])
		if !strings.Contains(cut.Note, fmt.Sprintf("only %d of the 6 planned loops start", n)) {
			t.Errorf("cut to %d: Note does not state the count: %s", n, cut.Note)
		}
		eq(t, "freed survives the cut", cut.RxFree.Freed, []int{6, 8})
	}
}

// ---- a homogeneous host reads nothing --------------------------------------

// On a host whose CPUs are alike the rule is not reached: not one file of the
// NIC tree is read and the interface table is not asked for (the cpu_capacity
// and cpuinfo reads are #1049's and outside this count). Pi 5: four alike
// A76 cores.
func TestRxFreeHomogeneousHostReadsNothing(t *testing.T) {
	for name, caps := range map[string][]string{
		"msr1-like 12 alike":    {"1024", "1024", "1024", "1024", "1024", "1024", "1024", "1024", "1024", "1024", "1024", "1024"},
		"Pi 5, 4 alike":         {"1024", "1024", "1024", "1024"},
		"capacities in 50..100": {"1024", "600", "700", "520"},
	} {
		t.Run(name, func(t *testing.T) {
			cpu := newFakeHost(t, rng(0, len(caps)-1))
			for i, c := range caps {
				cpu.capacity(i, c)
			}
			h := msr1RxReal(t, "msr1_rxfree_vector_on_cpu1.txt")
			got := planWorkerCPUs(len(caps), false, h.planSrc(cpu, clusterAddr))
			eq(t, "plan", got, planWorkerCPUs(len(caps), false, cpu.src()))
			if got.Heterogeneous {
				t.Error("heterogeneous")
			}
			if h.reads() != 0 {
				t.Errorf("%d reads (%d files, %d interface tables), want none", h.reads(), h.fs.calls, h.ifaceCalls)
			}
		})
	}
	// No capacity files at all: any read error means homogeneous.
	cpu := newFakeHost(t, rng(0, 3))
	h := msr1RxReal(t, "msr1_rxfree_vector_on_cpu1.txt")
	got := planWorkerCPUs(4, false, h.planSrc(cpu, clusterAddr))
	eq(t, "no capacity files", got.CPUs, rng(0, 3))
	if h.reads() != 0 {
		t.Errorf("no capacity files: %d reads", h.reads())
	}
}

// The wrapper without an address plans as #1049 does and reads nothing of the NIC.
func TestPlanWorkerCPUsHasNoAddress(t *testing.T) {
	p := PlanWorkerCPUs(3, false)
	if len(p.RxFree.Freed) != 0 || len(p.RxFree.Why) != 0 {
		t.Errorf("PlanWorkerCPUs ran the rule: %+v", p.RxFree)
	}
	cpu := msr1(t, rng(0, 11))
	h := msr1RxReal(t, "msr1_rxfree_vector_on_cpu1.txt")
	got := planWorkerCPUs(12, false, h.planSrc(cpu, ""))
	same1049(t, "empty address", got, planWorkerCPUs(12, false, cpu.src()))
	if h.reads() != 0 {
		t.Errorf("empty address: %d reads", h.reads())
	}
}

// ---- reads that fail give #1049's plan -------------------------------------

func TestRxFreeReadProblemsGive1049(t *testing.T) {
	cpu := msr1(t, rng(0, 11))
	want := planWorkerCPUs(12, false, cpu.src())
	type mod func(h *rxHost)
	for name, m := range map[string]mod{
		"rx_packets of the receiving port unreadable": func(h *rxHost) {
			h.fs.deny = func(n string) bool { return strings.HasSuffix(n, "enp49s0/statistics/rx_packets") }
		},
		"rx_packets of the other port unreadable": func(h *rxHost) {
			h.fs.deny = func(n string) bool { return strings.HasSuffix(n, "enp1s0/statistics/rx_packets") }
		},
		"rx_packets missing": func(h *rxHost) { h.write("sys/class/net/enp49s0/statistics/rx_packets", "") },
		"rx_packets garbage": func(h *rxHost) { h.write("sys/class/net/enp49s0/statistics/rx_packets", "lots\n") },
		"rx_packets negative": func(h *rxHost) {
			h.write("sys/class/net/enp49s0/statistics/rx_packets", "-5\n")
		},
		"/proc/interrupts unreadable": func(h *rxHost) {
			h.fs.deny = func(n string) bool { return strings.HasSuffix(n, "/proc/interrupts") }
		},
		"/proc/interrupts garbage": func(h *rxHost) { h.write("proc/interrupts", "hello\nworld\n") },
		"effective_affinity of IRQ 135 unreadable": func(h *rxHost) {
			h.fs.deny = func(n string) bool { return strings.HasSuffix(n, "/irq/135/effective_affinity_list") }
		},
		"effective_affinity of IRQ 167 unreadable": func(h *rxHost) {
			h.fs.deny = func(n string) bool { return strings.HasSuffix(n, "/irq/167/effective_affinity_list") }
		},
		"affinity of IRQ 135 is garbage": func(h *rxHost) { h.irq(135, "eight", "eight") },
		"affinity of IRQ 135 spans CPUs": func(h *rxHost) { h.irq(135, "6-8", "0-11") },
		"affinity of IRQ 167 spans CPUs": func(h *rxHost) { h.irq(167, "0-3", "0-11") },
		"no affinity files for IRQ 127": func(h *rxHost) {
			h.irq(127, "", "")
			if err := removeAll(h.path("proc/irq/127")); err != nil {
				t.Fatal(err)
			}
		},
		"msi_irqs of a port unreadable": func(h *rxHost) {
			h.fs.deny = func(n string) bool { return strings.HasSuffix(n, "enp49s0/device/msi_irqs") }
		},
		"interface table fails": func(h *rxHost) { h.ifaceErr = fmt.Errorf("netlink: no") },
		"a container: no interfaces": func(h *rxHost) {
			h.ifaces = nil
		},
		"a container: no /sys/class/net entries": func(h *rxHost) {
			if err := removeAll(h.path("sys/class/net")); err != nil {
				t.Fatal(err)
			}
		},
		"a file reader that panics": func(h *rxHost) {
			h.fs.boom = func(n string) bool { return strings.HasSuffix(n, "/proc/interrupts") }
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := msr1RxReal(t, "msr1_rxfree_vector_on_cpu1.txt")
			m(h)
			got := planWorkerCPUs(12, false, h.planSrc(cpu, clusterAddr))
			same1049(t, name, got, want)
			if len(got.RxFree.Why) == 0 {
				t.Error("the rule gave no reason (Why) for stepping aside")
			}
		})
	}
}

// Addresses the rule cannot place: loopback, a hostname, garbage.
func TestRxFreeUnplaceableListenAddresses(t *testing.T) {
	cpu := msr1(t, rng(0, 11))
	want := planWorkerCPUs(12, false, cpu.src())
	for _, addr := range []string{"127.0.0.1:8080", "[::1]:8080", "localhost:8080", "example.org:80", "not an address", "[::1", "10.9.9.9:80"} {
		h := msr1RxReal(t, "msr1_rxfree_vector_on_cpu1.txt")
		same1049(t, addr, planWorkerCPUs(12, false, h.planSrc(cpu, addr)), want)
	}
}

// ---- synthetic hosts --------------------------------------------------------

// vec is one NIC vector of a synthetic host.
type vec struct {
	dev   string
	irq   int
	cpu   int
	count uint64
}

// synth builds a host with the given non-little CPUs (the rest of 0..nCPU-1
// are little), physical ports with the given rx_packets, and busy vectors. Each
// port also has one idle vector (3 interrupts) on every CPU, as a real NIC has
// dozens, to prove that an idle vector's CPU counts for nothing. bonded puts a
// bond0 over the ports and listens on it; otherwise the listener is the one
// port's address.
type synth struct {
	nCPU   int
	big    []int
	rx     map[string]uint64
	vecs   []vec
	bonded bool
	idle   bool
}

func (s synth) build(t *testing.T) (*fakeHost, *rxHost, string) {
	t.Helper()
	cpu := newFakeHost(t, rng(0, s.nCPU-1))
	for c := 0; c < s.nCPU; c++ {
		if slices.Contains(s.big, c) {
			cpu.capacity(c, "1024")
		} else {
			cpu.capacity(c, "279")
		}
	}
	h := newRxHost(t)
	h.cpus = rng(0, s.nCPU-1)
	h.addr("lo", true, "127.0.0.1")
	ports := make([]string, 0, len(s.rx))
	for p := range s.rx {
		ports = append(ports, p)
	}
	slices.Sort(ports)
	next := 1000
	for _, p := range ports {
		var irqs []int
		for _, v := range s.vecs {
			if v.dev == p {
				irqs = append(irqs, v.irq)
			}
		}
		idleBase := next
		if s.idle {
			for c := 0; c < s.nCPU; c++ {
				irqs = append(irqs, idleBase+c)
			}
		}
		next += 100
		h.phys(p, irqs...)
		h.rx(p, s.rx[p])
		if s.idle {
			for c := 0; c < s.nCPU; c++ {
				h.intr(idleBase+c, fmt.Sprintf("%s-idle%d", p, c), map[int]uint64{c: 3})
				h.irq(idleBase+c, fmt.Sprint(c), "0-"+fmt.Sprint(s.nCPU-1))
			}
		}
	}
	for _, v := range s.vecs {
		h.intr(v.irq, fmt.Sprintf("%s-%d", v.dev, v.irq), map[int]uint64{v.cpu: v.count})
		h.irq(v.irq, fmt.Sprint(v.cpu), fmt.Sprint(v.cpu))
	}
	h.flushInterrupts()
	addr := "10.0.0.5:8080"
	if s.bonded {
		h.virt("bond0")
		for _, p := range ports {
			h.addr(p, true)
			h.lower("bond0", p)
		}
		h.addr("bond0", true, "10.0.0.5")
	} else {
		h.addr(ports[0], true, "10.0.0.5")
	}
	return cpu, h, addr
}

func (s synth) plan(t *testing.T, workers int) (CPUPlan, CPUPlan) {
	t.Helper()
	cpu, h, addr := s.build(t)
	return planWorkerCPUs(workers, false, h.planSrc(cpu, addr)), planWorkerCPUs(workers, false, cpu.src())
}

// msr1Shape is the msr1 layout on which the synthetic tests vary one thing: big
// CPUs 0,1,6-11, two ports, rx port "rx0" with busy vectors on CPUs 6 and 8,
// the other port "tx0" with a busy vector on CPU 1.
func msr1Shape() synth {
	return synth{
		nCPU: 12, big: []int{0, 1, 6, 7, 8, 9, 10, 11}, bonded: true, idle: true,
		rx: map[string]uint64{"rx0": 73_000_000, "tx0": 27_000_000},
		vecs: []vec{
			{"rx0", 10, 6, 2_000_000_000}, {"rx0", 11, 8, 4_000_000_000},
			{"tx0", 20, 0, 3_000_000_000}, {"tx0", 21, 1, 2_000_000_000},
		},
	}
}

func TestRxFreeSyntheticMatchesTheRealPlan(t *testing.T) {
	got, base := msr1Shape().plan(t, 12)
	eq(t, "loops", got.CPUs, msr1LoopsVNV)
	eq(t, "freed", got.RxFree.Freed, []int{6, 8})
	eq(t, "port", got.RxFree.Port, "rx0")
	eq(t, "base", base.CPUs, msr1Loops1049)
}

// The share of the receiving port, compared exactly: 60% acts, one packet less
// does not. A 50/50 bond (LACP spread by hash) and a fresh boot do not act.
func TestRxFreeShareThreshold(t *testing.T) {
	const total = 1_000_000_000
	for _, tc := range []struct {
		name string
		rx0  uint64
		act  bool
	}{
		{"73%", 730_000_000, true},
		{"60% exactly", 600_000_000, true},
		{"one packet under 60%", 599_999_999, false},
		{"59%", 590_000_000, false},
		{"50/50 bond", 500_000_000, false},
		// The other port received nothing: no port with a real share has a
		// busy vector to stay beside (the 10% rule), so the rule steps aside.
		{"100%: the other port received nothing", total, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := msr1Shape()
			s.rx = map[string]uint64{"rx0": tc.rx0, "tx0": total - tc.rx0}
			got, base := s.plan(t, 12)
			if tc.act {
				eq(t, "loops", got.CPUs, msr1LoopsVNV)
			} else {
				same1049(t, tc.name, got, base)
				want := "60%"
				if tc.rx0 == total {
					want = "10%" // the other port carries nothing to stay beside
				}
				if !strings.Contains(whyText(got), want) {
					t.Errorf("Why does not name the threshold %s: %s", want, whyText(got))
				}
			}
		})
	}
	// The receiving port may be listed second: the share, not the order, decides.
	s := msr1Shape()
	s.rx = map[string]uint64{"rx0": 20_000_000, "tx0": 80_000_000}
	s.vecs = []vec{{"tx0", 10, 6, 2_000_000_000}, {"tx0", 11, 8, 4_000_000_000}, {"rx0", 20, 0, 3_000_000_000}, {"rx0", 21, 1, 2_000_000_000}}
	got, _ := s.plan(t, 12)
	eq(t, "port order", got.CPUs, msr1LoopsVNV)
	eq(t, "port order: port", got.RxFree.Port, "tx0")
}

// Fresh boot, or a counter that holds little: no rule. A port with 60% of 5000
// packets and vectors with 999 interrupts says nothing.
func TestRxFreeFreshBoot(t *testing.T) {
	for name, mod := range map[string]func(s *synth){
		"few packets, busy vectors": func(s *synth) {
			s.rx = map[string]uint64{"rx0": 700_000, "tx0": 100_000}
		},
		"many packets, vectors under the floor": func(s *synth) {
			for i := range s.vecs {
				s.vecs[i].count = 999
			}
		},
		"nothing at all": func(s *synth) {
			s.rx = map[string]uint64{"rx0": 0, "tx0": 0}
			for i := range s.vecs {
				s.vecs[i].count = 0
			}
		},
		"packets just under the floor": func(s *synth) {
			s.rx = map[string]uint64{"rx0": 999_999, "tx0": 1}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := msr1Shape()
			mod(&s)
			got, base := s.plan(t, 12)
			same1049(t, name, got, base)
		})
	}
	// At the packet floor with the other port carrying 10% of the packets or
	// more: the rule acts.
	s := msr1Shape()
	s.rx = map[string]uint64{"rx0": 1_000_000, "tx0": 111_112}
	got, _ := s.plan(t, 12)
	eq(t, "packets at the floor", got.CPUs, msr1LoopsVNV)
}

// Only a busy vector counts: the idle vectors every NIC has (spread over all
// the big CPUs by the kernel) free nothing, and the busy vectors of a port that
// received nothing since boot are not a reason either.
func TestRxFreeIdleVectorsDoNotCount(t *testing.T) {
	s := msr1Shape()
	got, _ := s.plan(t, 12)
	eq(t, "freed", got.RxFree.Freed, []int{6, 8})
	// No busy vector on the receiving port at all.
	s.vecs = []vec{{"rx0", 10, 6, 10}, {"tx0", 20, 0, 3_000_000_000}}
	got, base := s.plan(t, 12)
	same1049(t, "no busy rx vector", got, base)
}

// Vectors on little CPUs, or none on a big CPU: nothing to free.
func TestRxFreeVectorsOnLittleCPUs(t *testing.T) {
	s := msr1Shape()
	s.vecs = []vec{{"rx0", 10, 2, 2_000_000_000}, {"rx0", 11, 4, 4_000_000_000}, {"tx0", 21, 1, 2_000_000_000}}
	got, base := s.plan(t, 12)
	same1049(t, "rx vectors on little CPUs", got, base)
	// One on a little CPU, one on a big one: the big one is freed.
	s.vecs = []vec{{"rx0", 10, 2, 2_000_000_000}, {"rx0", 11, 8, 4_000_000_000}, {"tx0", 21, 1, 2_000_000_000}}
	got, _ = s.plan(t, 12)
	eq(t, "loops", got.CPUs, []int{0, 1, 6, 7, 9, 10, 11})
	eq(t, "freed", got.RxFree.Freed, []int{8})
}

// The other port's busy vector on a CPU that runs no loop: the rule steps aside.
func TestRxFreeOtherPortOnALittleCPUStepsAside(t *testing.T) {
	s := msr1Shape()
	s.vecs[3] = vec{"tx0", 21, 4, 2_000_000_000}
	got, base := s.plan(t, 12)
	same1049(t, "tx vector on CPU 4", got, base)
	// Idle (under the floor) there: it does not count.
	s.vecs[3] = vec{"tx0", 21, 4, 999}
	got, _ = s.plan(t, 12)
	eq(t, "tx vector idle on CPU 4", got.CPUs, msr1LoopsVNV)
}

// A NIC with a queue vector on every big CPU (RSS spread), or on more of them
// than the floor lets the rule free: nothing is freed.
func TestRxFreeSpreadQueuesFreeNothing(t *testing.T) {
	var all []vec
	for i, c := range []int{0, 1, 6, 7, 8, 9, 10, 11} {
		all = append(all, vec{"rx0", 10 + i, c, uint64(1_000_000_000 + i*1000)})
	}
	for name, vecs := range map[string][]vec{
		"every big CPU hosts a queue vector": all,
		"three of eight (floor 6)":           all[:3],
		"seven of eight":                     all[:7],
	} {
		t.Run(name, func(t *testing.T) {
			s := msr1Shape()
			// The other port's busy vector on CPU 11 (the receiving port's
			// own may sit there too): the floor is the reason to step aside.
			s.vecs = append(slices.Clone(vecs), vec{"tx0", 90, 11, 2_000_000_000})
			got, base := s.plan(t, 12)
			same1049(t, name, got, base)
			if !strings.Contains(whyText(got), "floor") {
				t.Errorf("Why does not name the floor: %s", whyText(got))
			}
		})
	}
	// Two of eight: allowed, and the busiest are the ones named first.
	s := msr1Shape()
	s.vecs = append(slices.Clone(all[:2]), vec{"tx0", 90, 11, 2_000_000_000})
	got, _ := s.plan(t, 12)
	eq(t, "two of eight", got.RxFree.Freed, []int{0, 1})
	eq(t, "loops", got.CPUs, []int{6, 7, 8, 9, 10, 11})
}

// The floor: max(min(2,n), ceil(3n/4)) loops of n non-little CPUs.
func TestRxFreeFloor(t *testing.T) {
	for _, tc := range []struct {
		n, maxFreed int
	}{
		{1, 0}, {2, 0}, {3, 0}, {4, 1}, {5, 1}, {6, 1}, {7, 1}, {8, 2}, {12, 3}, {16, 4},
	} {
		if got := rxFreeBudget(tc.n); got != tc.maxFreed {
			t.Errorf("rxFreeBudget(%d) = %d, want %d", tc.n, got, tc.maxFreed)
		}
	}
	// With n big CPUs and k+1 candidate CPUs, nothing; with k, k are freed. Big
	// CPUs 0..n-1 and four little ones after them.
	for _, n := range []int{2, 3, 4, 5, 6, 8, 12, 16} {
		k := rxFreeBudget(n)
		big := rng(0, n-1)
		mk := func(cands int) synth {
			s := synth{nCPU: n + 4, big: big, bonded: true, idle: true,
				rx: map[string]uint64{"rx0": 90_000_000, "tx0": 10_000_000}}
			// The other port's vector on the LAST big CPU, kept out of the
			// candidates by taking candidates from the front.
			for i := range cands {
				s.vecs = append(s.vecs, vec{"rx0", 10 + i, i, uint64(1_000_000_000 - i)})
			}
			s.vecs = append(s.vecs, vec{"tx0", 90, n - 1, 1_000_000})
			return s
		}
		if k > 0 && k < n-1 {
			got, _ := mk(k).plan(t, n+4)
			eq(t, fmt.Sprintf("n=%d frees %d", n, k), len(got.RxFree.Freed), k)
			if len(got.CPUs) < n-k || len(got.CPUs) != n-k {
				t.Errorf("n=%d: %d loops, want %d", n, len(got.CPUs), n-k)
			}
		}
		if k+1 < n {
			got, base := mk(k+1).plan(t, n+4)
			same1049(t, fmt.Sprintf("n=%d with %d candidates", n, k+1), got, base)
		}
		// The loops never fall under the floor.
		for c := 1; c < n; c++ {
			got, _ := mk(c).plan(t, n+4)
			floor := max(min(2, n), (3*n+3)/4)
			if len(got.CPUs) < floor {
				t.Errorf("n=%d, %d candidates: %d loops, under the floor %d", n, c, len(got.CPUs), floor)
			}
		}
	}
}

// A 4-big + 4-little board with one NIC (RK3588-like): the rule's premise is a
// loop kept beside a TX-completion vector of ANOTHER port, and with one port
// there is none, so the plan is #1049's whatever the NIC's vector does
// (celeris#1036 item 1, review finding 1). The rule used to free the vector's
// CPU here.
func TestRxFreeSingleNICBoard(t *testing.T) {
	s := synth{nCPU: 8, big: []int{4, 5, 6, 7}, idle: true, rx: map[string]uint64{"eth0": 50_000_000},
		vecs: []vec{{"eth0", 10, 4, 5_000_000}}}
	got, base := s.plan(t, 8)
	eq(t, "base", base.CPUs, []int{4, 5, 6, 7})
	same1049(t, "vector on big CPU 4", got, base)
	if !strings.Contains(whyText(got), "stay beside") {
		t.Errorf("Why does not say that no vector stays beside a loop: %s", whyText(got))
	}
	s.vecs = []vec{{"eth0", 10, 0, 5_000_000}}
	got, base = s.plan(t, 8)
	same1049(t, "vector on little CPU 0", got, base)
	// Two vectors on two big CPUs: the floor (3 of 4) allows one.
	s.vecs = []vec{{"eth0", 10, 4, 5_000_000}, {"eth0", 11, 5, 6_000_000}}
	got, base = s.plan(t, 8)
	same1049(t, "two big CPUs on a 4-big board", got, base)
	// Two big CPUs only (n=2): never.
	s2 := synth{nCPU: 4, big: []int{2, 3}, idle: true, rx: map[string]uint64{"eth0": 50_000_000},
		vecs: []vec{{"eth0", 10, 2, 5_000_000}}}
	got, base = s2.plan(t, 4)
	same1049(t, "two big CPUs", got, base)
}

// The listener bound to one slave of the bond sees only that port: its share is
// 100%, but there is no other port whose TX completion a loop could stay beside,
// so the plan is #1049's (review finding 1).
func TestRxFreeListenerOnOnePort(t *testing.T) {
	s := msr1Shape()
	s.bonded = false
	cpu, h, _ := s.build(t)
	// "rx0" is the first port by name and owns 10.0.0.5 in the unbonded form.
	got := planWorkerCPUs(12, false, h.planSrc(cpu, "10.0.0.5:8080"))
	same1049(t, "one port", got, planWorkerCPUs(12, false, cpu.src()))
	if !strings.Contains(whyText(got), "stay beside") {
		t.Errorf("Why: %s", whyText(got))
	}
}

// A host with one CPU-less detail: the nic source is nil. The planner runs as #1049.
func TestRxFreeNilSource(t *testing.T) {
	cpu := msr1(t, rng(0, 11))
	s := cpu.src()
	s.addr = clusterAddr
	same1049(t, "nil nic", planWorkerCPUs(12, false, s), planWorkerCPUs(12, false, cpu.src()))
}

// The reads of one decision are few and bounded: msr1's 64 vectors cost less
// than a thousand file reads.
func TestRxFreeReadsAreBounded(t *testing.T) {
	cpu := msr1(t, rng(0, 11))
	h := msr1RxReal(t, "msr1_rxfree_vector_on_cpu1.txt")
	planWorkerCPUs(12, false, h.planSrc(cpu, clusterAddr))
	if r := h.reads(); r == 0 || r > 400 {
		t.Errorf("%d reads on the msr1 snapshot", r)
	}
	if h.ifaceCalls != 1 {
		t.Errorf("%d interface tables, want 1", h.ifaceCalls)
	}
}

// ---- the log ---------------------------------------------------------------

func TestRxFreeLog(t *testing.T) {
	cpu := msr1(t, rng(0, 11))
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	h := msr1RxReal(t, "msr1_rxfree_vector_on_cpu1.txt")
	LogPinOutcome(log, "epoll", planWorkerCPUs(12, false, h.planSrc(cpu, clusterAddr)), nil)
	out := buf.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var infos []string
	for _, l := range lines {
		if strings.Contains(l, "level=INFO") {
			infos = append(infos, l)
		}
	}
	if len(infos) != 1 {
		t.Fatalf("want one Info line, got %d:\n%s", len(infos), out)
	}
	for _, w := range []string{"freed_cpus=6,8", "rx_port=enp49s0", "rx_share_pct=73", "workers=6", "requested=12", "pinned=6"} {
		if !strings.Contains(infos[0], w) {
			t.Errorf("Info line lacks %q:\n%s", w, infos[0])
		}
	}
	// The Debug line names the candidates that were passed over (CPU 0, CPU 1:
	// busy vectors of enp1s0, which keep their loops).
	var dbg string
	for _, l := range lines {
		if strings.Contains(l, "level=DEBUG") {
			dbg = l
		}
	}
	if dbg == "" || !strings.Contains(dbg, "IRQ 167") || !strings.Contains(dbg, "enp1s0") {
		t.Errorf("Debug line does not name the other port's vector:\n%s", out)
	}

	// Vector@4: no freed fields, a Debug line that says why.
	buf.Reset()
	h4 := msr1RxReal(t, "msr1_rxfree_vector_on_cpu4.txt")
	LogPinOutcome(log, "epoll", planWorkerCPUs(12, false, h4.planSrc(cpu, clusterAddr)), nil)
	out = buf.String()
	if strings.Contains(out, "freed_cpus") || strings.Contains(out, "rx_port") {
		t.Errorf("a plan that freed nothing logs freed fields:\n%s", out)
	}
	if !strings.Contains(out, "level=DEBUG") || !strings.Contains(out, "CPU 4") {
		t.Errorf("no Debug line saying why:\n%s", out)
	}

	// A homogeneous host logs nothing, and an explicit count no Debug line.
	buf.Reset()
	alike := newFakeHost(t, rng(0, 3))
	LogPinOutcome(log, "epoll", planWorkerCPUs(4, false, alike.src()), nil)
	if buf.Len() != 0 {
		t.Errorf("homogeneous host logged:\n%s", buf.String())
	}
	buf.Reset()
	he := msr1RxReal(t, "msr1_rxfree_vector_on_cpu1.txt")
	LogPinOutcome(log, "epoll", planWorkerCPUs(12, true, he.planSrc(cpu, clusterAddr)), nil)
	if strings.Contains(buf.String(), "level=DEBUG") {
		t.Errorf("explicit workers logged a Debug line:\n%s", buf.String())
	}
}

func removeAll(p string) error { return os.RemoveAll(p) }

// The real host, in whatever container the test runs (no big.LITTLE, no
// NIC): the plan with an address is the plan without one, inside the thread's
// allowed set, and no CPU it frees is among its loops.
func TestPlanWorkerCPUsForOnTheRealHost(t *testing.T) {
	allowed, err := schedAllowedCPUs()
	if err != nil {
		t.Fatalf("sched_getaffinity: %v", err)
	}
	for _, explicit := range []bool{false, true} {
		for _, addr := range []string{"0.0.0.0:8080", "[::]:0", "127.0.0.1:1", "localhost:80", ""} {
			p := PlanWorkerCPUsFor(len(allowed)+3, explicit, addr)
			base := PlanWorkerCPUs(len(allowed)+3, explicit)
			if !p.Heterogeneous {
				eq(t, "alike host: plan", p, base)
			}
			eq(t, "allowed", p.Allowed, allowed)
			for _, c := range p.CPUs {
				if c >= 0 && !slices.Contains(allowed, c) {
					t.Errorf("addr %q: loop on CPU %d outside the allowed set %v", addr, c, allowed)
				}
				if slices.Contains(p.RxFree.Freed, c) {
					t.Errorf("addr %q: a loop on the freed CPU %d", addr, c)
				}
			}
			if len(p.CPUs) > len(base.CPUs) {
				t.Errorf("addr %q: %d loops, the plan without the rule has %d", addr, len(p.CPUs), len(base.CPUs))
			}
		}
	}
}

// The Debug text is bounded however many vectors a NIC has, and the reason the
// rule gives is never the line that is cut.
func TestRxFreeWhyIsBounded(t *testing.T) {
	s := msr1Shape()
	for i := 0; i < 60; i++ {
		s.vecs = append(s.vecs, vec{"tx0", 100 + i, 0, 2_000_000_000})
	}
	// More candidates than the floor allows: the reason comes last.
	for i := 0; i < 5; i++ {
		s.vecs = append(s.vecs, vec{"rx0", 200 + i, 7 + i%4, 3_000_000_000})
	}
	got, base := s.plan(t, 12)
	if len(got.RxFree.Why) == 0 || len(got.RxFree.Why) > rxMaxWhy {
		t.Fatalf("%d Why lines, want 1..%d", len(got.RxFree.Why), rxMaxWhy)
	}
	same1049(t, "too many candidates", got, base)
	if last := got.RxFree.Why[len(got.RxFree.Why)-1]; !strings.Contains(last, "floor of loops") {
		t.Errorf("the last Why line is not the reason: %q", last)
	}
}
