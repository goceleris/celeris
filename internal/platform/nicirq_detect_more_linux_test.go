//go:build linux

package platform

import (
	"strconv"
	"strings"
	"testing"
)

// celeris#1036: detection cases on the msr1-like fixture. Expected values are
// written out by hand.

func TestDetectSkipsVectorsThatNeverFired(t *testing.T) {
	// IRQ 168 is listed with all-zero counts (serviced by little CPU 5), IRQ
	// 136 is in msi_irqs but absent from /proc/interrupts (CPU 3): neither
	// is reported. IRQ 167 (CPU 4) has taken interrupts and is.
	mk := func() *nicHost {
		nh := msr1NIC(t)
		nh.write("sys/class/net/enp1s0/device/msi_irqs/168", "msix\n")
		nh.write("sys/class/net/enp49s0/device/msi_irqs/136", "msix\n")
		nh.irq(168, "5", "0-11")
		nh.irq(136, "3", "0-11")
		nh.intr(168, "enp1s0-9", nil)
		nh.flushInterrupts()
		return nh
	}
	got := mk().detect(bondAddr)
	eq(t, "serviced", vecCPUs(got), []int{0, 4, 6, 8})
	eq(t, "idle", got.Idle, 2)
	base, plan := reported(t, mk(), 12, false, rng(0, 11), bondAddr)
	eq(t, "uncovered (only IRQ 167's CPU 4; the idle vectors on CPUs 3 and 5 are not judged)", plan.IRQUncovered, []int{4})
	eq(t, "loops", plan.CPUs, base.CPUs)

	// Without a usable /proc/interrupts nothing says which vector carries
	// traffic: none is reported, and the idle count stays 0 (the vectors were
	// not judged). This reverses the rule of the idle test alone (a vector was
	// then taken as it was), which reported the unlisted IRQ 156 of the cluster.
	nh := mk()
	nh.fs.deny = func(name string) bool { return strings.HasSuffix(name, "/proc/interrupts") }
	got = nh.detect(bondAddr)
	eq(t, "no table: serviced", vecCPUs(got), []int(nil))
	eq(t, "no table: idle", got.Idle, 0)
	if !strings.Contains(got.Reason, "cannot tell which vectors carry traffic") {
		t.Errorf("no table: reason %q", got.Reason)
	}
	nh = mk()
	nh.write("proc/interrupts", "this is not the file you know\n")
	got = nh.detect(bondAddr)
	eq(t, "unparsable table: serviced", vecCPUs(got), []int(nil))
	if !strings.Contains(got.Reason, "cannot tell which vectors carry traffic") {
		t.Errorf("unparsable table: reason %q", got.Reason)
	}

	// Every vector idle: the reason says so.
	nh = newNICHost(t)
	nh.phys("eth0", 1, 2)
	nh.addr("eth0", true, "10.0.0.1")
	nh.irq(1, "3", "")
	nh.irq(2, "4", "")
	nh.intr(1, "eth0-0", nil)
	nh.intr(2, "eth0-1", nil)
	nh.flushInterrupts()
	got = nh.detect("10.0.0.1:1")
	if len(got.Vectors) != 0 || got.Idle != 2 || !strings.Contains(got.Reason, "none reaches the share/floor (2 without an interrupt since boot, 0 under the limits)") {
		t.Errorf("all idle: %+v", got)
	}
}

func TestDetectShortInterruptsRowIsDropped(t *testing.T) {
	// A numbered row with fewer columns than the header is dropped by the
	// parser; detection neither panics nor reports an internal error. IRQ 167
	// is then unlisted, like a vector that never fired: idle, and the others
	// are judged without it.
	nh := msr1NIC(t)
	nh.rows[1] = "167:         5" // the row of IRQ 167, cut short
	nh.flushInterrupts()
	got := nh.detect(bondAddr)
	if strings.Contains(got.Reason, "internal error") {
		t.Errorf("Reason %q", got.Reason)
	}
	eq(t, "irqs", irqsOf(got), []int{127, 135, 159})
	eq(t, "idle", got.Idle, 1)
	// A file of nothing but a short row has no usable row at all.
	nh.write("proc/interrupts", "           CPU0       CPU1\n167:         5\n")
	got = nh.detect(bondAddr)
	if strings.Contains(got.Reason, "internal error") || len(got.Vectors) != 0 {
		t.Errorf("only a short row: %+v", got)
	}
}

func TestDetectEmptyEffectiveAffinityFallsThrough(t *testing.T) {
	// A vector that is not activated, or an irqchip that does not update the
	// effective mask, prints an empty list. It names no CPU, like an absent
	// file: the single-CPU smp mask says which one, and nothing else does (the
	// counts are history, celeris#1085).
	blank := func(h *nicHost, irq int) {
		h.write("proc/irq/"+strconv.Itoa(irq)+"/effective_affinity_list", "\n")
	}
	// msr1: IRQ 167 has a blank effective list and smp_affinity_list 4.
	nh := msr1NIC(t)
	blank(nh, 167)
	nh.write("proc/irq/167/smp_affinity_list", "4\n")
	got := nh.detect(bondAddr)
	eq(t, "irqs", irqsOf(got), []int{127, 135, 159, 167})
	if v := vecByIRQ(t, got, 167); v.CPU != 4 {
		t.Errorf("IRQ 167 = %+v, want CPU 4", v)
	}
	eq(t, "unknown", got.Unknown, 0)
	// A multi-CPU smp mask: no CPU is named, however the counts are spread
	// (all of IRQ 167's interrupts so far are on CPU 4, and on msr1 it is
	// serviced by CPU 1 now).
	nh = msr1NIC(t)
	blank(nh, 167)
	got = nh.detect(bondAddr)
	eq(t, "irqs", irqsOf(got), []int{127, 135, 159})
	eq(t, "unknown", got.Unknown, 1)
	// Nothing else to read: unknown, as before.
	nh = newNICHost(t)
	nh.phys("eth0", 50)
	nh.addr("eth0", true, "10.0.0.1")
	blank(nh, 50)
	nh.write("proc/irq/50/smp_affinity_list", "0-11\n")
	nh.intr(50, "eth0-0", map[int]uint64{4: 500, 5: 500})
	nh.flushInterrupts()
	got = nh.detect("10.0.0.1:1")
	eq(t, "unknown", got.Unknown, 1)
}

func TestDetectJudgesNoMoreThanTheVectorCap(t *testing.T) {
	// Five ports of 1000 vectors each, one interrupt on every vector: 5000
	// vectors, of which only the first nicMaxVectors (4096) are judged. The
	// ones past the cap are in neither Idle nor Negligible, but their
	// interrupts are in Total, the denominator of the share.
	h := newNICHost(t)
	h.virt("bond0")
	for d := range 5 {
		dev := "eth" + strconv.Itoa(d)
		var irqs []int
		for i := range 1000 {
			irqs = append(irqs, 10000+1000*d+i)
		}
		h.phys(dev, irqs...)
		h.lower("bond0", dev)
		for _, n := range irqs {
			h.intr(n, dev+"-"+strconv.Itoa(n), map[int]uint64{n % 12: 1})
		}
	}
	h.addr("bond0", true, "10.0.0.1")
	h.flushInterrupts()
	got := h.detect("10.0.0.1:1")
	eq(t, "vectors", len(got.Vectors), 0)
	eq(t, "negligible (judged)", got.Negligible, nicMaxVectors)
	eq(t, "idle", got.Idle, 0)
	eq(t, "total (all 5000 vectors)", got.Total, uint64(5000))
}
