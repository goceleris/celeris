//go:build linux

package platform

import (
	"fmt"
	"strings"
)

// RxFree is what the rx-port rule decided for a plan (celeris#1036 item 1).
// Its zero value means the rule took nothing out of the loop set.
type RxFree struct {
	// Port is the physical interface that received the traffic.
	Port string
	// SharePercent is the share of the packets received since boot, over the
	// physical ports behind the listener, that Port took (rounded down).
	SharePercent int
	// Freed are the big CPUs left without a loop, ascending.
	Freed []int
	// Vectors are the busy interrupt vectors of Port that sit on the Freed
	// CPUs, busiest first.
	Vectors []RxVector
	// Why says, for a Debug line, why the rule did not act or which candidates
	// it passed over. Empty when the rule did not run.
	Why []string
}

// RxVector is one interrupt vector of the receiving port.
type RxVector struct {
	IRQ int
	// Name is the action name /proc/interrupts shows ("enp49s0-8").
	Name string
	// CPU is the CPU that services it.
	CPU int
	// Count is its interrupts since boot, summed over the CPUs.
	Count uint64
}

// Sentence says what the rule did, for the plan's Note.
func (f RxFree) Sentence() string {
	var vs []string
	for _, v := range f.Vectors {
		vs = append(vs, fmt.Sprintf("%s (IRQ %d, CPU %d)", v.Name, v.IRQ, v.CPU))
	}
	return fmt.Sprintf("the CPUs %s run no loop: they service the interrupt vectors %s of %s, the port that received %d%% of the packets since boot",
		FormatCPUs(f.Freed), strings.Join(vs, ", "), f.Port, f.SharePercent)
}

// rxFreeCPUs is a stub: the rule is not implemented yet and takes nothing.
func rxFreeCPUs(addr string, big []int, src nicSource) RxFree {
	return RxFree{}
}

// rxFreeBudget is the most big CPUs the rule may leave without a loop when n
// CPUs are not little. The loops it leaves never fall under
// max(min(2, n), ceil(3n/4)): 6 of 8, 3 of 4, and no CPU at all of 2 or 3.
// The three-quarters is fitted to msr1, the one host measured (6 loops of 8 are
// the V-NV arm of the PINPLAN runs).
func rxFreeBudget(n int) int {
	floor := max(min(2, n), (3*n+3)/4)
	return max(n-floor, 0)
}
