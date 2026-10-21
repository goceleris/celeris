//go:build linux

package platform

import (
	"bytes"
	"fmt"
	"log/slog"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// celeris#1036 item 1: a vector is checked against the loops only if it carries traffic, which
// is: at least nicMinInterrupts (1000) interrupts since boot AND at least
// nicMinSharePercent (5%) of the interrupts since boot of ALL the vectors of
// the NICs the listener is reachable through. The expected values are written
// out by hand from the fixtures.

// The real msr1 snapshots: what the rule saw on the cluster, and the vectors it
// must keep (the four busy ones, all at least 14% of the traffic) and drop (the
// two with 3 interrupts since boot).
func TestRealMSR1VectorsThatCarryTraffic(t *testing.T) {
	for _, tc := range []struct {
		file       string
		total      uint64 // the sum of the 64 vectors' counts, by hand from the snapshot
		serviced   []int
		skippedCPU map[int]int // IRQ -> CPU of the skipped vectors with interrupts
	}{
		{snapVectorOnCPU4, 11184054606, []int{0, 4, 6, 8}, map[int]int{156: 5, 188: 1}},
		{snapVectorOnCPU1, 12109841263, []int{0, 1, 6, 8}, map[int]int{156: 5, 188: 1}},
	} {
		t.Run(tc.file, func(t *testing.T) {
			got := msr1Snapshot(t, tc.file).detect(clusterAddr)
			eq(t, "irqs", irqsOf(got), []int{127, 135, 159, 167})
			eq(t, "serviced CPUs", vecCPUs(got), tc.serviced)
			eq(t, "devs", got.Devs, []string{"enp1s0", "enp49s0"})
			eq(t, "total", got.Total, tc.total)
			eq(t, "idle (58 of 64 vectors never fired)", got.Idle, 58)
			eq(t, "negligible", got.Negligible, 2)
			eq(t, "unknown", got.Unknown, 0)
			if len(got.Skipped) != 2 {
				t.Fatalf("skipped %+v, want IRQ 156 and 188", got.Skipped)
			}
			for _, s := range got.Skipped {
				cpu, ok := tc.skippedCPU[s.IRQ]
				if !ok || s.CPU != cpu || s.Count != 3 || !strings.Contains(s.Why, "3 interrupts since boot, under the floor of 1000") {
					t.Errorf("skipped %+v, want CPU %d, 3 interrupts, the floor", s, cpu)
				}
			}
			// The margin the thresholds sit in: the quietest vector kept took
			// 14.7% (IRQ 127 in the first snapshot), almost three times the
			// share required, the busiest one left out took 3 interrupts (a
			// third of the floor).
			for _, v := range got.Vectors {
				if v.Count*100 < 14*got.Total {
					t.Errorf("IRQ %d (%s) took %d of %d: under 14%%", v.IRQ, v.Name, v.Count, got.Total)
				}
			}
			// Both ports of the bond carry traffic, and both of their
			// TX-completion vectors are kept: enp49s0-8 (IRQ 135, CPU 8) and
			// enp1s0-8 (IRQ 167).
			v135, v167 := vecByIRQ(t, got, 135), vecByIRQ(t, got, 167)
			if v135.Name != "enp49s0-8" || v135.CPU != 8 || v167.Name != "enp1s0-8" {
				t.Errorf("IRQ 135 = %+v, IRQ 167 = %+v", v135, v167)
			}
		})
	}
}

// Both ports busy and both TX-completion vectors on little CPUs: both are
// named, in one Warn, and no loop is added.
func TestRealMSR1BothBusyPortsAreNamed(t *testing.T) {
	h := msr1Snapshot(t, snapVectorOnCPU4)
	h.irq(135, "3", "0-11") // enp49s0-8 moved onto little CPU 3
	base, got := reported(t, h, 12, false, rng(0, 11), clusterAddr)
	eq(t, "loops", got.CPUs, []int{0, 1, 6, 7, 8, 9, 10, 11})
	eq(t, "#1049 loops", base.CPUs, got.CPUs)
	eq(t, "serviced", got.IRQCPUs, []int{0, 3, 4, 6})
	eq(t, "uncovered", got.IRQUncovered, []int{3, 4})
	var buf bytes.Buffer
	LogPinOutcome(slog.New(slog.NewTextHandler(&buf, nil)), "epoll", got, nil)
	out := buf.String()
	if n := strings.Count(out, "level=WARN"); n != 1 {
		t.Errorf("%d Warn lines, want 1:\n%s", n, out)
	}
	for _, w := range []string{"cpus=3-4 ", "CPU 3: enp49s0-8 (IRQ 135)", "CPU 4: enp1s0-8 (IRQ 167)", "/proc/irq/<irq>/smp_affinity_list for each IRQ of 135, 167"} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
}

// The thresholds to the interrupt: a vector on little CPU 3 beside one on big
// CPU 6.
func TestTrafficTestBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		x, y  uint64
		keeps bool
	}{
		{"exactly 5% of the total", 5000, 95000, true},
		{"one interrupt under 5% (4.999%)", 4999, 95001, false},
		{"one interrupt over 5%", 5001, 94999, true},
		{"exactly the floor, 50% of the total", 1000, 1000, true},
		{"one under the floor, 49.97% of the total", 999, 1000, false},
		{"both under the floor", 999, 999, false},
		{"over the floor, far under the share", 1000, 999000, false},
		{"over the floor, a single vector", 1000, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := trafficNIC(t, []int{3, 6}, []uint64{tc.x, tc.y})
			got := h.detect("10.0.0.1:80")
			has3 := false
			for _, v := range got.Vectors {
				has3 = has3 || v.IRQ == 100
			}
			if has3 != tc.keeps {
				t.Errorf("vector with %d of %d kept = %v, want %v (result %+v)", tc.x, tc.x+tc.y, has3, tc.keeps, got)
			}
			// The report follows: CPU 3 is named iff its vector is kept, and the
			// loops are #1049's either way.
			base, pl := reported(t, h, 12, false, rng(0, 11), "10.0.0.1:80")
			if named := slices.Contains(pl.IRQUncovered, 3); named != tc.keeps {
				t.Errorf("CPU 3 uncovered = %v (%v), want %v", named, pl.IRQUncovered, tc.keeps)
			}
			eq(t, "loops", pl.CPUs, base.CPUs)
		})
	}
}

// trafficNIC is eth0 (10.0.0.1) with one vector per CPU in cpus, IRQ 100+i,
// serviced by cpus[i] with counts[i] interrupts on it.
func trafficNIC(t testing.TB, cpus []int, counts []uint64) *nicHost {
	t.Helper()
	h := newNICHost(t)
	var irqs []int
	for i := range cpus {
		irqs = append(irqs, 100+i)
	}
	h.phys("eth0", irqs...)
	h.addr("eth0", true, "10.0.0.1")
	for i, cpu := range cpus {
		h.irq(100+i, strconv.Itoa(cpu), "")
		h.intr(100+i, "eth0-"+strconv.Itoa(i), map[int]uint64{cpu: counts[i]})
	}
	h.flushInterrupts()
	return h
}

// The denominator is the interrupts of every vector of the listener's NICs:
// not the vectors of one NIC, not every row of /proc/interrupts, not only the
// vectors whose CPU is known, not only those over the floor.
func TestTrafficDenominator(t *testing.T) {
	t.Run("both ports of a bond, not the last one", func(t *testing.T) {
		// A busy port (enp1s0, CPU 6) and a quiet one (enp49s0, 4000
		// interrupts on little CPU 3): 100% of its own port, 0.4% of the
		// bond's.
		h := newNICHost(t)
		h.phys("enp1s0", 10)
		h.phys("enp49s0", 20)
		h.virt("bond0")
		h.lower("bond0", "enp1s0")
		h.lower("bond0", "enp49s0")
		h.addr("bond0", true, "10.0.0.1")
		h.irq(10, "6", "")
		h.irq(20, "3", "")
		h.intr(10, "enp1s0-0", map[int]uint64{6: 996000})
		h.intr(20, "enp49s0-0", map[int]uint64{3: 4000})
		h.flushInterrupts()
		got := h.detect("10.0.0.1:80")
		eq(t, "irqs", irqsOf(got), []int{10})
		eq(t, "total", got.Total, uint64(1000000))
		eq(t, "negligible", got.Negligible, 1)
		// The same quiet port beside an equally quiet one is a vector of a
		// bond that carries 8000 interrupts in all: it counts.
		h.rows = h.rows[:0]
		h.intr(10, "enp1s0-0", map[int]uint64{6: 4000})
		h.intr(20, "enp49s0-0", map[int]uint64{3: 4000})
		h.flushInterrupts()
		eq(t, "irqs of a quiet bond", irqsOf(h.detect("10.0.0.1:80")), []int{10, 20})
	})
	t.Run("a wildcard listener's quiet second NIC", func(t *testing.T) {
		// msr1 plus a NIC with an address and one vector with 2000 interrupts
		// on little CPU 3: 100% of its own device and 2e-7 of the host's NICs.
		h := msr1Snapshot(t, snapVectorOnCPU1)
		h.phys("enp7s0", 70)
		h.addr("enp7s0", true, "10.0.0.5")
		h.irq(70, "3", "0-11")
		h.appendInterrupts("70:" + strings.Repeat("          0", 3) + "       2000" + strings.Repeat("          0", 8) + "  ITS-MSI 524358 Edge      enp7s0-0")
		got := h.detect(clusterAddr)
		eq(t, "irqs", irqsOf(got), []int{127, 135, 159, 167})
		eq(t, "negligible", got.Negligible, 3) // 156, 188 and 70
		_, pl := reported(t, h, 12, false, rng(0, 11), clusterAddr)
		eq(t, "loops", pl.CPUs, []int{0, 1, 6, 7, 8, 9, 10, 11})
		eq(t, "uncovered (CPU 3's vector is 2e-7 of the NICs)", pl.IRQUncovered, []int(nil))
	})
	t.Run("a numbered row that is no NIC's", func(t *testing.T) {
		// The arch timer of a busy host has far more interrupts than the NICs.
		h := msr1Snapshot(t, snapVectorOnCPU4)
		h.appendInterrupts(" 10:" + strings.Repeat("  900000000000", 12) + "     GICv3  27 Level     arch_timer")
		got := h.detect(clusterAddr)
		eq(t, "irqs", irqsOf(got), []int{127, 135, 159, 167})
		eq(t, "total", got.Total, uint64(11184054606))
	})
	t.Run("a vector whose CPU is unknown still counts", func(t *testing.T) {
		// X on CPU 3: 4000; U: 100000, a multi-CPU mask and counts spread over
		// the CPUs, so no CPU is known for it. X is 3.8% of the NIC.
		h := newNICHost(t)
		h.phys("eth0", 1, 2)
		h.addr("eth0", true, "10.0.0.1")
		h.irq(1, "3", "")
		h.irq(2, "0-11", "0-11")
		spread := map[int]uint64{}
		for _, c := range rng(0, 11) {
			spread[c] = 8000
		}
		h.intr(1, "eth0-0", map[int]uint64{3: 4000})
		h.intr(2, "eth0-1", spread)
		h.flushInterrupts()
		got := h.detect("10.0.0.1:80")
		eq(t, "irqs", irqsOf(got), []int(nil))
		eq(t, "unknown", got.Unknown, 1)
		eq(t, "negligible", got.Negligible, 1)
		eq(t, "total", got.Total, uint64(100000))
	})
	t.Run("vectors under the floor still count", func(t *testing.T) {
		// 100 vectors with 999 interrupts each (99900 in all) beside X with
		// 5000: X is 4.77% of the NIC.
		cpus := []int{3}
		counts := []uint64{5000}
		for range 100 {
			cpus = append(cpus, 6)
			counts = append(counts, 999)
		}
		got := trafficNIC(t, cpus, counts).detect("10.0.0.1:80")
		eq(t, "irqs", irqsOf(got), []int(nil))
		eq(t, "total", got.Total, uint64(104900))
		eq(t, "negligible", got.Negligible, 101)
	})
}

// appendInterrupts adds a row to a /proc/interrupts written by the fixture.
func (h *nicHost) appendInterrupts(row string) {
	h.t.Helper()
	b, err := os.ReadFile(h.path("proc/interrupts"))
	if err != nil {
		h.t.Fatal(err)
	}
	h.write("proc/interrupts", string(b)+row+"\n")
}

// 20 vectors of equal weight take 5% each and all qualify; 21 take 4.76% and
// none does. A balanced NIC with more than 20 active vectors is read as one
// with no traffic: no Warn, a blind spot of the rule (the Debug line says why).
// The plan is #1049's either way.
func TestTrafficBalancedNICWithManyVectors(t *testing.T) {
	cpus := func(n int) (cs []int, counts []uint64) {
		for i := range n {
			cs = append(cs, i%12)
			counts = append(counts, 50000)
		}
		return
	}
	cs, counts := cpus(20)
	got := trafficNIC(t, cs, counts).detect("10.0.0.1:80")
	eq(t, "20 vectors: kept", len(got.Vectors), 20)
	cs, counts = cpus(21)
	h := trafficNIC(t, cs, counts)
	got = h.detect("10.0.0.1:80")
	eq(t, "21 vectors: kept", len(got.Vectors), 0)
	eq(t, "21 vectors: negligible", got.Negligible, 21)
	base, pl := reported(t, h, 12, false, rng(0, 11), "10.0.0.1:80")
	eq(t, "21 vectors: the plan is #1049's", pl.CPUs, base.CPUs)
	eq(t, "21 vectors: no Warn", pl.IRQUncovered, []int(nil))
	eq(t, "21 vectors: Note", pl.Note, base.Note)
}

// The traffic test: the floor and the share, exact over the whole range of uint64
// (atLeastPercent itself is tested with the rx-port rule's helpers).
func TestNegligibleBoundaries(t *testing.T) {
	const m = math.MaxUint64
	for _, tc := range []struct {
		n, total, pct uint64
		want          bool
	}{
		{5, 100, 5, true},
		{4, 100, 5, false},
		{0, 100, 5, false},
		{100, 100, 100, true},
		{1, 20, 5, true},
		{1, 21, 5, false},
		{m / 20, m, 5, false},    // one under, at the top of the range
		{m/20 + 1, m, 5, true},   // 922337203685477580.. is 5% rounded up
		{m, m, 5, true},          // n*100 does not fit in 64 bits
		{m / 2, m, 5, true},      // nor does n*100 here
		{m/100 - 1, m, 1, false}, // 1% boundary
		{m/100 + 1, m, 1, true},  //
		{1 << 62, 1 << 63, 5, true},
	} {
		if got := atLeastPercent(tc.n, tc.total, tc.pct); got != tc.want {
			t.Errorf("atLeastPercent(%d, %d, %d) = %v, want %v", tc.n, tc.total, tc.pct, got, tc.want)
		}
	}
	eq(t, "satAdd", satAdd(m-1, 5), uint64(m))
	eq(t, "satAdd small", satAdd(2, 3), uint64(5))
	eq(t, "negligible kept", negligible(1000, 20000), "")
	if why := negligible(999, 999); !strings.Contains(why, "under the floor of 1000") {
		t.Errorf("floor: %q", why)
	}
	if why := negligible(1000, 20001); !strings.Contains(why, "under 5%") {
		t.Errorf("share: %q", why)
	}
}

// A count that saturates the total (a hostile or wrapped /proc/interrupts)
// neither panics nor lets a quiet vector in.
func TestTrafficSaturatedTotal(t *testing.T) {
	h := trafficNIC(t, []int{3, 6}, []uint64{5000, math.MaxUint64 - 10})
	got := h.detect("10.0.0.1:80")
	eq(t, "total saturates", got.Total, uint64(math.MaxUint64))
	eq(t, "irqs", irqsOf(got), []int{101})
}

// The skipped vectors leave a trace: the Info line carries the count and a
// Debug line the detail (the vector, its CPU, how many interrupts, the limit);
// the Warn never names them.
func TestLogPinOutcomeNamesTheSkippedVectors(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := msr1Snapshot(t, snapVectorOnCPU1)
	_, plan := reported(t, h, 12, false, rng(0, 11), clusterAddr)
	eq(t, "idle", plan.IRQIdle, 58)
	eq(t, "negligible", plan.IRQNegligible, 2)
	LogPinOutcome(log, "epoll", plan, nil)
	out := buf.String()
	if strings.Contains(out, "level=WARN") {
		t.Errorf("a Warn with every busy vector on a CPU that runs a loop:\n%s", out)
	}
	for _, w := range []string{
		"level=INFO", "nic_irq_skipped=60",
		"level=DEBUG", "vectors under the traffic limits are not warned about", "idle=58", "negligible=2",
		"enp49s0-29 (IRQ 156, CPU 5): 3 interrupts since boot, under the floor of 1000",
		"enp1s0-29 (IRQ 188, CPU 1): 3 interrupts since boot",
		"min_share_percent=5", "min_interrupts=1000",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
	// No skipped vector, no trace.
	buf.Reset()
	LogPinOutcome(log, "epoll", msr1Plan(t, 12, false, rng(0, 11)), nil)
	if strings.Contains(buf.String(), "nic_irq_skipped") || strings.Contains(buf.String(), "under the traffic limits") {
		t.Errorf("trace without skipped vectors:\n%s", buf.String())
	}
	// More skipped vectors than the detail keeps: the rest is counted.
	buf.Reset()
	cs := []int{6}
	counts := []uint64{1000000}
	for i := range nicMaxSkipped + 3 {
		cs = append(cs, 3)
		counts = append(counts, uint64(1+i))
	}
	nh := trafficNIC(t, cs, counts)
	_, plan = reported(t, nh, 12, false, rng(0, 11), "10.0.0.1:80")
	eq(t, "kept detail", len(plan.IRQSkipped), nicMaxSkipped)
	eq(t, "negligible", plan.IRQNegligible, nicMaxSkipped+3)
	LogPinOutcome(log, "epoll", plan, nil)
	if want := fmt.Sprintf("%d more", 3); !strings.Contains(buf.String(), want) {
		t.Errorf("output lacks %q:\n%s", want, buf.String())
	}
}
