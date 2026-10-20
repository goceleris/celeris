//go:build linux

package platform

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// celeris#1036 item 1, the review fixes of #1085. Each test below is the
// failing-first test of one finding: the rule must step aside (the plan is
// #1049's) where its premise does not hold, and must keep acting on the msr1
// plan (the real snapshots in cpuplan_rxfree_linux_test.go).

// ---- finding 1: a busy vector must stay beside a loop -----------------------

// The rule keeps a loop beside a TX-completion vector of another port. With no
// busy vector that stays on a CPU that runs a loop after the CPUs are freed
// there is nothing to keep a loop beside, and the plan is #1049's.
func TestRxFreeNeedsABusyVectorBesideALoop(t *testing.T) {
	// The msr1 layout on ONE port (the listener is bound to rx0 only): the
	// rule used to free CPUs 6 and 8, loops 0,1,7,9,10,11.
	one := synth{nCPU: 12, big: []int{0, 1, 6, 7, 8, 9, 10, 11}, idle: true,
		rx:   map[string]uint64{"rx0": 73_000_000},
		vecs: []vec{{"rx0", 10, 6, 2_000_000_000}, {"rx0", 11, 8, 4_000_000_000}}}
	got, base := one.plan(t, 12)
	same1049(t, "one port", got, base)
	eq(t, "one port: loops", got.CPUs, msr1Loops1049)
	if !strings.Contains(whyText(got), "stay beside") {
		t.Errorf("Why does not say that no vector stays beside a loop: %s", whyText(got))
	}

	// The receiving port's other busy vector on a little CPU has no loop beside
	// it either.
	one.vecs = append(slices.Clone(one.vecs), vec{"rx0", 12, 2, 1_000_000_000})
	got, base = one.plan(t, 12)
	same1049(t, "one port, a vector on little CPU 2", got, base)

	// Two ports, the other one quiet: no busy vector of it, so nothing stays.
	two := msr1Shape()
	two.rx = map[string]uint64{"rx0": 73_000_000, "tx0": 500_000}
	two.vecs = []vec{{"rx0", 10, 6, 2_000_000_000}, {"rx0", 11, 8, 4_000_000_000}, {"tx0", 20, 0, 10}}
	got, base = two.plan(t, 12)
	same1049(t, "other port quiet", got, base)

	// The same with the other port's busy vector on CPU 7: a big CPU that is
	// not freed. The rule acts.
	two.rx = map[string]uint64{"rx0": 73_000_000, "tx0": 27_000_000}
	two.vecs = []vec{{"rx0", 10, 6, 2_000_000_000}, {"rx0", 11, 8, 4_000_000_000}, {"tx0", 20, 7, 3_000_000_000}}
	got, _ = two.plan(t, 12)
	eq(t, "vector beside a loop: loops", got.CPUs, []int{0, 1, 7, 9, 10, 11})
	eq(t, "vector beside a loop: freed", got.RxFree.Freed, []int{6, 8})
}

// ---- finding 2: the rule is for one loop on each big CPU --------------------

// GOMAXPROCS under the big CPU count (the default under a Kubernetes CPU
// limit): the loops are #1049's, no NIC file is read, and the Debug text does
// not claim that a CPU keeps its loop.
func TestRxFreeNeedsOneLoopPerBigCPU(t *testing.T) {
	s := msr1Shape()
	// The other port's vector on CPU 10, which 4 loops drawn from the CPUs
	// left over after the rule (0,1,7,9) would not cover.
	s.vecs = append(slices.Clone(s.vecs), vec{"tx0", 22, 10, 2_000_000_000})
	for n := 1; n < 8; n++ {
		cpu, h, addr := s.build(t)
		got := planWorkerCPUs(n, false, h.planSrc(cpu, addr))
		same1049(t, fmt.Sprintf("%d workers", n), got, planWorkerCPUs(n, false, cpu.src()))
		if h.reads() != 0 {
			t.Errorf("%d workers: %d NIC reads, want none", n, h.reads())
		}
		if why := whyText(got); !strings.Contains(why, "under the 8 non-little CPUs") || strings.Contains(why, "keeps its loop") {
			t.Errorf("%d workers: Why: %s", n, why)
		}
	}
	got, _ := s.plan(t, 4)
	eq(t, "4 workers", got.CPUs, []int{0, 1, 6, 7})

	// One loop on each of the 8 big CPUs: the rule acts, and the other port's
	// vector on CPU 10 does keep its loop.
	got, _ = s.plan(t, 8)
	eq(t, "8 workers: loops", got.CPUs, msr1LoopsVNV)
	eq(t, "8 workers: freed", got.RxFree.Freed, []int{6, 8})
	if !strings.Contains(whyText(got), "IRQ 22") || !strings.Contains(whyText(got), "keeps its loop") {
		t.Errorf("8 workers: Why does not name IRQ 22 on CPU 10: %s", whyText(got))
	}
	got, _ = s.plan(t, 9)
	eq(t, "9 workers: loops", got.CPUs, msr1LoopsVNV)
}

// ---- finding 5: a port with packets and no busy vector ----------------------

// A listener port that received a million packets or more but has no busy
// vector is a contradiction (its counter wrapped, or its vectors are not the
// ones read): the vector that would have kept a loop may be hidden, so the plan
// is #1049's. Needs three ports: with two, finding 1 already steps aside.
func TestRxFreeAPortWithPacketsButNoBusyVector(t *testing.T) {
	mk := func(tx1 uint64, tx1Vec uint64) synth {
		s := msr1Shape()
		s.rx = map[string]uint64{"rx0": 70_000_000, "tx0": 20_000_000, "tx1": tx1}
		if tx1Vec > 0 {
			s.vecs = append(slices.Clone(s.vecs), vec{"tx1", 30, 1, tx1Vec})
		}
		return s
	}
	for _, tc := range []struct {
		name string
		s    synth
		act  bool
	}{
		{"tx1 quiet (0 packets)", mk(0, 0), true},
		{"tx1 just under the packet floor", mk(999_999, 0), true},
		{"tx1 at the packet floor, no busy vector", mk(1_000_000, 0), false},
		{"tx1 with 5e6 packets, no busy vector", mk(5_000_000, 0), false},
		{"tx1 with 5e6 packets, a wrapped counter (999)", mk(5_000_000, 999), false},
		{"tx1 with 5e6 packets, a busy vector (1000)", mk(5_000_000, 1000), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, base := tc.s.plan(t, 12)
			if tc.act {
				eq(t, "loops", got.CPUs, msr1LoopsVNV)
				return
			}
			same1049(t, tc.name, got, base)
			if why := whyText(got); !strings.Contains(why, "tx1") || !strings.Contains(why, "no busy vector") {
				t.Errorf("Why does not name tx1 and the missing vector: %s", why)
			}
		})
	}
}

// ---- finding 6: the vector cap steps aside ----------------------------------

// capHost is a wildcard listener over seven ports, in this order: rx0 (busy
// vectors on CPUs 6 and 8), tx0 (busy on CPU 1), four pad ports of idle
// vectors (nicMaxIRQsPerDev each at most) and zz, whose busy vector on CPU 6
// comes after the pads. With padVectors pad vectors the host lists 4 +
// padVectors vectors in all.
func capHost(t *testing.T, padVectors int) (*fakeHost, *rxHost) {
	t.Helper()
	cpu := msr1(t, rng(0, 11))
	h := newRxHost(t)
	h.addr("lo", true, "127.0.0.1")
	ports := []string{"rx0", "tx0", "pad0", "pad1", "pad2", "pad3", "zz"}
	rx := map[string]uint64{"rx0": 73_000_000, "tx0": 20_000_000}
	for i, p := range ports {
		var irqs []int
		switch p {
		case "rx0":
			irqs = []int{10, 11}
		case "tx0":
			irqs = []int{20}
		case "zz":
			irqs = []int{90}
		default:
			n := min(padVectors, nicMaxIRQsPerDev)
			padVectors -= n
			for j := 0; j < n; j++ {
				irqs = append(irqs, 10_000+i*2_000+j)
			}
		}
		h.phys(p, irqs...)
		h.rx(p, rx[p])
		h.addr(p, true, fmt.Sprintf("10.0.%d.1", i))
	}
	h.intr(10, "rx0-10", map[int]uint64{6: 2_000_000_000})
	h.intr(11, "rx0-11", map[int]uint64{8: 4_000_000_000})
	h.intr(20, "tx0-20", map[int]uint64{1: 3_000_000_000})
	h.intr(90, "zz-90", map[int]uint64{6: 2_000_000})
	for irq, c := range map[int]int{10: 6, 11: 8, 20: 1, 90: 6} {
		h.irq(irq, fmt.Sprint(c), fmt.Sprint(c))
	}
	h.flushInterrupts()
	return cpu, h
}

// More vectors than the cap: the busy vector of the last port was skipped and
// its CPU lost its loop (the `break` left only the inner loop). Exactly the cap
// is still read whole.
func TestRxFreeVectorCapStepsAside(t *testing.T) {
	cpu, h := capHost(t, nicMaxVectors-4)
	got := planWorkerCPUs(12, false, h.planSrc(cpu, "0.0.0.0:8080"))
	// zz's vector keeps CPU 6 its loop; CPU 8 is freed.
	eq(t, "at the cap: loops", got.CPUs, []int{0, 1, 6, 7, 9, 10, 11})
	eq(t, "at the cap: freed", got.RxFree.Freed, []int{8})

	cpu, h = capHost(t, nicMaxVectors-4+1)
	got = planWorkerCPUs(12, false, h.planSrc(cpu, "0.0.0.0:8080"))
	same1049(t, "one over the cap", got, planWorkerCPUs(12, false, cpu.src()))
	if why := whyText(got); !strings.Contains(why, "interrupt vectors") {
		t.Errorf("Why does not name the cap: %s", why)
	}
}

// ---- finding 7: the order of the slaves does not depend on readdir ----------

func TestLowerDevicesAreSorted(t *testing.T) {
	h := newRxHost(t)
	h.phys("enp9s0", 1)
	h.phys("enp1s0", 2)
	h.phys("enp49s0", 3)
	h.virt("bond0")
	h.lower("bond0", "enp9s0")
	h.lower("bond0", "enp1s0")
	h.lower("bond0", "enp49s0")
	for _, rev := range []bool{false, true} {
		h.fs.reverse = rev
		eq(t, fmt.Sprintf("reverse readdir=%v", rev), lowerDevices([]string{"bond0"}, h.nic()),
			[]string{"enp1s0", "enp49s0", "enp9s0"})
	}
}

// ---- finding 8: vectors shared by two ports ---------------------------------

// Ports that share MSI-X vectors (a dual-port mlx4 card): a vector listed by
// two ports is not the receiving port's alone, so its CPU keeps its loop,
// whichever port the name order puts first. The receiving port's own vector
// (IRQ 12 on CPU 7) is the one freed.
func TestRxFreeSharedVectorsKeepTheirLoops(t *testing.T) {
	for _, names := range [][2]string{{"pa", "pb"}, {"pz", "pb"}} {
		rx, other := names[0], names[1]
		t.Run(rx+"+"+other, func(t *testing.T) {
			cpu := msr1(t, rng(0, 11))
			h := newRxHost(t)
			h.phys(rx, 10, 11, 12)
			h.phys(other, 10, 11)
			h.phys("pc", 20)
			h.virt("bond0")
			for _, p := range []string{rx, other, "pc"} {
				h.addr(p, true)
				h.lower("bond0", p)
			}
			h.addr("bond0", true, "10.0.0.5")
			h.rx(rx, 70_000_000)
			h.rx(other, 3_000_000)
			h.rx("pc", 20_000_000)
			for irq, c := range map[int]int{10: 6, 11: 8, 12: 7, 20: 1} {
				h.intr(irq, fmt.Sprintf("nic-%d", irq), map[int]uint64{c: 2_000_000_000})
				h.irq(irq, fmt.Sprint(c), fmt.Sprint(c))
			}
			h.flushInterrupts()
			got := planWorkerCPUs(12, false, h.planSrc(cpu, "10.0.0.5:8080"))
			eq(t, "freed", got.RxFree.Freed, []int{7})
			eq(t, "loops", got.CPUs, []int{0, 1, 6, 8, 9, 10, 11})
			eq(t, "port", got.RxFree.Port, rx)
			if len(got.RxFree.Vectors) != 1 || got.RxFree.Vectors[0].IRQ != 12 {
				t.Errorf("vectors: %+v", got.RxFree.Vectors)
			}
			if why := whyText(got); !strings.Contains(why, "shared") {
				t.Errorf("Why does not say the vectors are shared: %s", why)
			} else if n := strings.Count(why, "kept: IRQ 10 "); n != 1 {
				// A vector two ports list is judged once, not once per port.
				t.Errorf("IRQ 10 is judged %d times: %s", n, why)
			}
		})
	}
}

// A comma list names several CPUs, and the first of them is not the one that
// services the vector: effectiveCPU passes over such a list to the next source.
func TestEffectiveCPUCommaList(t *testing.T) {
	for _, tc := range []struct {
		name     string
		eff, smp string
		want     int
	}{
		{"effective 4,6, smp absent", "4,6", "-", -1},
		{"effective 4,6, smp names 6", "4,6", "6", 6},
		{"effective 4,6, smp 7,8", "4,6", "7,8", -1},
		{"effective range 4-6, smp absent", "4-6", "-", -1},
		{"effective 6-6 is one CPU", "6-6", "4,5", 6},
		{"smp 4,6 alone", "-", "4,6", -1},
	} {
		h := newRxHost(t)
		if tc.eff != "-" {
			h.write("proc/irq/9/effective_affinity_list", tc.eff+"\n")
		}
		if tc.smp != "-" {
			h.write("proc/irq/9/smp_affinity_list", tc.smp+"\n")
		}
		if got := effectiveCPU(9, h.nic()); got != tc.want {
			t.Errorf("%s: effectiveCPU = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// ---- the invariants, over arbitrary hosts -----------------------------------

type fuzzVec struct {
	dev    int
	cpu    int // the CPU the vector sits on
	count  uint64
	shape  int  // the affinity files, see FuzzRxFreeHost
	shared bool // also listed by the next port
	irq    int
}

// known reports whether the affinity files of the vector name a single CPU:
// shapes 4 (both files span CPUs) and 5 (garbage in the effective list) do not.
func (v fuzzVec) known() bool { return v.shape != 4 && v.shape != 5 }

var (
	fuzzCounts = []uint64{0, 10, 999, 1000, 4_000_000_000}
	fuzzRx     = []uint64{0, 999_999, 1_000_000, 5_000_000, 70_000_000, 1_000_000_000, 20_000_000}
	fuzzBig    = []int{0, 1, 6, 7, 8, 9, 10, 11}
)

// FuzzRxFreeHost builds a host of three ports from the input (their packets,
// their vectors with counts, CPUs, affinity files of several shapes, and
// vectors shared between neighbouring ports) and checks what the rule frees
// against the generator's own data: never a CPU that hosts a busy vector of
// another port (or a shared one), never without a busy vector staying on a CPU
// that keeps its loop, never over a vector whose CPU is unknown, never with a
// port that has packets and no busy vector.
func FuzzRxFreeHost(f *testing.F) {
	// 3 rx classes, then 5 bytes per vector: port, cpu, count class, affinity
	// shape, shared (1 = also listed by the next port).
	seed := func(rx [3]byte, vecs ...[5]byte) {
		d := rx[:]
		for _, v := range vecs {
			d = append(d, v[:]...)
		}
		f.Add(d)
	}
	rx6, rx8, other1 := [5]byte{0, 6, 4, 0, 0}, [5]byte{0, 8, 4, 0, 0}, [5]byte{1, 1, 4, 0, 0}
	seed([3]byte{4, 0, 0}, rx6, rx8)                                                 // one port, two busy vectors
	seed([3]byte{4, 6, 0}, rx6, rx8, other1)                                         // msr1-like: CPU 1 beside
	seed([3]byte{4, 3, 0}, rx6, rx8, other1)                                         // the other port under 10% of the packets
	seed([3]byte{4, 6, 2}, rx6, rx8, other1)                                         // a third port with packets, no vector
	seed([3]byte{4, 6, 0}, rx6, rx8, [5]byte{1, 1, 4, 4, 0})                         // a busy vector of unknown CPU
	seed([3]byte{4, 6, 0}, [5]byte{0, 6, 4, 0, 1}, rx8, other1)                      // CPU 6 shared with the next port
	seed([3]byte{4, 6, 0}, rx6, rx8, [5]byte{1, 7, 4, 3, 0}, [5]byte{2, 9, 1, 6, 0}) // comma list, empty effective
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 3 {
			return
		}
		var rx [3]uint64
		for i := range rx {
			rx[i] = fuzzRx[int(data[i])%len(fuzzRx)]
		}
		var vecs []fuzzVec
		for i, rest := 0, data[3:]; len(rest) >= 5 && i < 10; i, rest = i+1, rest[5:] {
			vecs = append(vecs, fuzzVec{dev: int(rest[0]) % 3, cpu: int(rest[1]) % 14, irq: 100 + i,
				count: fuzzCounts[int(rest[2])%len(fuzzCounts)], shape: int(rest[3]) % 7, shared: rest[4]%4 == 1})
		}
		h := newRxHost(t)
		h.cpus = rng(0, 13)
		ports := []string{"p0", "p1", "p2"}
		listed := make([][]int, 3)
		for _, v := range vecs {
			listed[v.dev] = append(listed[v.dev], v.irq)
			if v.shared {
				listed[(v.dev+1)%3] = append(listed[(v.dev+1)%3], v.irq)
			}
		}
		h.virt("bond0")
		for i, p := range ports {
			h.phys(p, listed[i]...)
			h.rx(p, rx[i])
			h.addr(p, true)
			h.lower("bond0", p)
		}
		h.addr("bond0", true, "10.0.0.5")
		for _, v := range vecs {
			h.intr(v.irq, fmt.Sprintf("%s-%d", ports[v.dev], v.irq), map[int]uint64{v.cpu: v.count})
			c, d := fmt.Sprint(v.cpu), fmt.Sprint((v.cpu+3)%14)
			// The affinity file shapes effectiveCPU has to tell apart.
			switch v.shape {
			case 0:
				h.irq(v.irq, c, c)
			case 1:
				h.irq(v.irq, c, "0-13")
			case 2:
				h.irq(v.irq, "", c)
			case 3:
				h.irq(v.irq, c+","+d, c)
			case 4: // both name several CPUs: unknown
				h.irq(v.irq, "0-13", "0-13")
			case 5: // garbage in effective: the vector is unknown
				h.irq(v.irq, "x", c)
			default: // an empty effective list: smp names it
				h.write(fmt.Sprintf("proc/irq/%d/effective_affinity_list", v.irq), "\n")
				h.irq(v.irq, "", c)
			}
		}
		h.flushInterrupts()
		rf := rxFreeCPUs("10.0.0.5:8080", fuzzBig, h.nic())
		if len(rf.Freed) == 0 {
			return
		}
		// Rebuild what the rule must have seen.
		busy := func(v fuzzVec) bool { return v.count >= rxVectorMinInterrupts }
		portIdx := slices.Index(ports, rf.Port)
		if portIdx < 0 {
			t.Fatalf("freed %v with port %q", rf.Freed, rf.Port)
		}
		var total uint64
		for _, n := range rx {
			total += n
		}
		if rx[portIdx] < rxMinPackets || rx[portIdx]*100 < total*rxPortPercent {
			t.Fatalf("freed %v for %s with %d of %d packets", rf.Freed, rf.Port, rx[portIdx], total)
		}
		if len(rf.Freed) > rxFreeBudget(len(fuzzBig)) {
			t.Fatalf("freed %v over the budget", rf.Freed)
		}
		hasBusy := make([]bool, 3)
		var rxCPUs []int
		stays := false
		for _, v := range vecs {
			if !busy(v) {
				continue
			}
			lists := []int{v.dev}
			if v.shared {
				lists = append(lists, (v.dev+1)%3)
			}
			for _, d := range lists {
				hasBusy[d] = true
			}
			if !v.known() {
				t.Fatalf("freed %v with a busy vector of unknown CPU (IRQ %d)", rf.Freed, v.irq)
			}
			exclusive := len(lists) == 1 && lists[0] == portIdx
			if exclusive {
				rxCPUs = append(rxCPUs, v.cpu)
			} else if slices.Contains(rf.Freed, v.cpu) {
				t.Fatalf("freed CPU %d hosts IRQ %d, a busy vector of another port: %v", v.cpu, v.irq, rf.Freed)
			}
			// Only a vector of a port with 10% of the packets or more counts as
			// one a loop stays beside.
			carries := false
			for _, d := range lists {
				carries = carries || rx[d]*100 >= total*10
			}
			if carries && slices.Contains(fuzzBig, v.cpu) && !slices.Contains(rf.Freed, v.cpu) {
				stays = true
			}
		}
		for _, c := range rf.Freed {
			if !slices.Contains(fuzzBig, c) || !slices.Contains(rxCPUs, c) {
				t.Fatalf("freed CPU %d is not a big CPU of a vector of %s: %v", c, rf.Port, rf.Freed)
			}
		}
		if !stays {
			t.Fatalf("freed %v with no busy vector of a port with 10%% of the packets staying on a CPU that keeps its loop", rf.Freed)
		}
		for i := range ports {
			if rx[i] >= rxMinPackets && !hasBusy[i] {
				t.Fatalf("freed %v although %s received %d packets and has no busy vector", rf.Freed, ports[i], rx[i])
			}
		}
	})
}

// ---- round 2: a vector counts only if its port carries a real share ---------

// dataMgmtHost is a wildcard listener over two unbonded ports: data0 (busy
// vectors on CPUs 6 and 8) and a management port mgmt0 with one busy vector on
// CPU 0, which runs a loop. The rule used to count that vector as one a loop
// stays beside and freed both TX-completion CPUs of the data port.
func dataMgmtHost(t *testing.T, dataRx, mgmtRx, mgmtCount uint64) (*fakeHost, *rxHost) {
	t.Helper()
	cpu := msr1(t, rng(0, 11))
	h := newRxHost(t)
	h.addr("lo", true, "127.0.0.1")
	h.phys("data0", 10, 11)
	h.phys("mgmt0", 20)
	h.rx("data0", dataRx)
	h.rx("mgmt0", mgmtRx)
	h.addr("data0", true, "10.0.1.1")
	h.addr("mgmt0", true, "10.0.2.1")
	h.intr(10, "data0-10", map[int]uint64{6: 2_000_000_000})
	h.intr(11, "data0-11", map[int]uint64{8: 4_000_000_000})
	h.intr(20, "mgmt0-20", map[int]uint64{0: mgmtCount})
	for irq, c := range map[int]int{10: 6, 11: 8, 20: 0} {
		h.irq(irq, fmt.Sprint(c), fmt.Sprint(c))
	}
	h.flushInterrupts()
	return cpu, h
}

func TestRxFreeManagementNICIsNoVectorToStayBeside(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		dataRx, mgmtRx, mgmtCount uint64
	}{
		{"A: 2e6 packets, 50k interrupts", 900_000_000, 2_000_000, 50_000},
		{"B: 2e5 packets, 2k interrupts", 900_000_000, 200_000, 2_000},
		{"just under 10%", 900_000_001, 99_999_999, 50_000_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cpu, h := dataMgmtHost(t, tc.dataRx, tc.mgmtRx, tc.mgmtCount)
			got := planWorkerCPUs(12, false, h.planSrc(cpu, "0.0.0.0:8080"))
			same1049(t, tc.name, got, planWorkerCPUs(12, false, cpu.src()))
			if why := whyText(got); !strings.Contains(why, "stay beside") || !strings.Contains(why, "10%") {
				t.Errorf("Why: %s", why)
			}
		})
	}
	// Exactly 10% of the pooled packets, and a real share: the rule acts.
	for name, rx := range map[string][2]uint64{"exactly 10%": {900_000_000, 100_000_000}, "27%": {730_000_000, 270_000_000}} {
		cpu, h := dataMgmtHost(t, rx[0], rx[1], 50_000_000)
		got := planWorkerCPUs(12, false, h.planSrc(cpu, "0.0.0.0:8080"))
		eq(t, name+": loops", got.CPUs, msr1LoopsVNV)
		eq(t, name+": freed", got.RxFree.Freed, []int{6, 8})
	}
}

// ---- round 2: the reason names the floor, not a spread ----------------------

// A cpuset of 0-3,6,8 on msr1: big CPUs 0,1,6,8 (n=4, budget 1) and the data
// port's two busy vectors on CPUs 6 and 8. The cause of the step aside is the
// small floor, not a load spread over the queues.
func TestRxFreeSmallFloorReason(t *testing.T) {
	cpu := msr1(t, []int{0, 1, 2, 3, 6, 8})
	h := msr1RxReal(t, "msr1_rxfree_vector_on_cpu1.txt")
	got := planWorkerCPUs(4, false, h.planSrc(cpu, clusterAddr))
	same1049(t, "cpuset 0-3,6,8", got, planWorkerCPUs(4, false, cpu.src()))
	why := whyText(got)
	if strings.Contains(why, "spread over the queues") || !strings.Contains(why, "floor of loops allows only 1 of 4") {
		t.Errorf("Why: %s", why)
	}
}
