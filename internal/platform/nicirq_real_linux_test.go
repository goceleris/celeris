//go:build linux

package platform

import (
	"slices"
	"testing"
)

// celeris#1036 item 1: the real msr1 snapshots of celeris#1085
// (testdata/msr1_rxfree_*.txt, built by msr1RxReal): the /proc/interrupts rows,
// the effective and smp affinity of the 64 vectors of both bond ports and the
// rx_packets of the two ports, verbatim from the cell.start.txt of two cluster
// columns. IRQ 156 (enp49s0-29) is serviced by little CPU 5 and has 3
// interrupts since boot in both.

const (
	// snapVectorOnCPU4 has IRQ 167 (enp1s0-8) on little CPU 4, the boot
	// placement; snapVectorOnCPU1 has it on CPU 1, the tuned host.
	snapVectorOnCPU4 = "msr1_rxfree_vector_on_cpu4.txt"
	snapVectorOnCPU1 = "msr1_rxfree_vector_on_cpu1.txt"
)

// msr1Snapshot is the host of a snapshot as the report's tests see it.
func msr1Snapshot(t testing.TB, file string) *nicHost { return &nicHost{rxHost: msr1RxReal(t, file)} }

// IRQ 156 (enp49s0-29, little CPU 5) has 3 interrupts since boot: it is in no
// report field, so it can be in no Warn (the Debug line of the skipped vectors
// names it, on purpose).
func noIRQ156(t *testing.T, p CPUPlan) {
	t.Helper()
	for _, v := range p.IRQVectors {
		if v.IRQ == 156 {
			t.Errorf("IRQ 156 is in the report: %+v", v)
		}
	}
	if slices.Contains(p.IRQCPUs, 5) || slices.Contains(p.IRQUncovered, 5) {
		t.Errorf("CPU 5 (IRQ 156's) is in the report: %v / %v", p.IRQCPUs, p.IRQUncovered)
	}
}

// The detection over the real 64-vector snapshot (two ports of 32 vectors).
func BenchmarkDetectNICIRQsMSR1Snapshot(b *testing.B) {
	h := msr1Snapshot(b, snapVectorOnCPU4)
	src := h.src()
	b.ResetTimer()
	for b.Loop() {
		if got := detectNICIRQs(clusterAddr, src); len(got.Vectors) != 4 {
			b.Fatal(got)
		}
	}
}
