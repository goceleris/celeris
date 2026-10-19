//go:build linux

package platform

import (
	"fmt"
	"slices"
	"strconv"
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
	vs := make([]string, 0, len(f.Vectors))
	for _, v := range f.Vectors {
		vs = append(vs, fmt.Sprintf("%s (IRQ %d, CPU %d)", v.Name, v.IRQ, v.CPU))
	}
	return fmt.Sprintf("the CPUs %s run no loop: they service the interrupt vectors %s of %s, the port that received %d%% of the packets since boot",
		FormatCPUs(f.Freed), strings.Join(vs, ", "), f.Port, f.SharePercent)
}

// Limits of the rule. They are choices fitted to msr1, the one host measured
// (see DESIGN-vnv-rule in the PR): enp49s0 took 69% to 73% of the packets of
// the host in 6 snapshots, the busy vectors had 1.7e9 to 4.0e9 interrupts since
// boot, and the 60 idle vectors of the two ports had 0 or 3.
const (
	// rxPortPercent is the share of the packets received since boot, over the
	// physical ports behind the listener, that one port needs to count as the
	// receiving port. A bond that balances its ports (about 50% each) has none.
	rxPortPercent = 60
	// rxMinPackets is the fewest packets the receiving port must have received
	// since boot: under it the shares say nothing (a fresh boot).
	rxMinPackets = 1_000_000
	// rxVectorMinInterrupts is the fewest interrupts since boot for a vector to
	// count as busy. Every NIC has dozens of vectors that never fire, and the
	// kernel spreads their CPUs over every CPU of the host: without this floor
	// every big CPU would host "a vector of the port".
	rxVectorMinInterrupts = 1000
	// rxMaxWhy bounds the Debug text.
	rxMaxWhy = 12
)

// rxFreeCPUs decides which of the big CPUs the default loops stay off
// (celeris#1036 item 1). PINPLAN v4 and v5 on msr1 showed that 6 loops on the
// CPUs 0,1,7,9,10,11 remove every tail regression of #1049's 8 loops and keep
// its throughput, and that 6 loops on 0,1,6,7,8,9 are 14% to 26% slower on
// every cell: the CPU set matters, not only the count. CPUs 6 and 8 service the
// two busy vectors of enp49s0, the port that received 73% of the packets; CPU 1
// services the busy vector of enp1s0 and keeps its loop.
//
// The rule, in order, over the listener's address:
//
//  1. The physical ports behind the interfaces the address belongs to (lower_*
//     links through bonds and vlans); the port with at least 60% of their
//     rx_packets since boot, and at least a million, is the receiving port.
//  2. Its busy vectors (at least 1000 interrupts since boot, whatever their
//     share) and the busy vectors of the other ports: the CPU of each is the
//     single CPU of effective_affinity_list (else smp_affinity_list).
//  3. If a busy vector of ANOTHER port is serviced by a CPU that runs no loop
//     (a little CPU, or outside the allowed set), the rule steps aside. A loop
//     must stay on a TX-completion vector CPU (the #1036 stall finding); CPU 4
//     hosting enp1s0's vector is the msr1 boot placement, and its plan (CPUs 4
//     and 8 both without a loop) was not measured.
//  4. The candidates are the big CPUs that service a busy vector of the
//     receiving port, minus those that also service a busy vector of another
//     port: a CPU that hosts another port's TX completion keeps its loop.
//  5. At most rxFreeBudget(n) CPUs may go (the floor of loops, n big CPUs). More
//     candidates than that is how an RSS spread looks, with no hot spot: the
//     rule frees nothing rather than pick among equals.
//
// A busy vector is any vector, so a TX-completion-only vector of the receiving
// port is freed with the rest: the evidence of msr1 is that CPU 8, which
// services enp49s0's TX completion, can lose its loop while CPU 1 keeps one.
//
// Any read that fails, any format it does not know, a busy vector whose CPU is
// not a single one: no CPU is freed and Why says why. It reads nothing when the
// floor leaves no CPU to free.
func rxFreeCPUs(addr string, big []int, src nicSource) (rf RxFree) {
	defer func() {
		if r := recover(); r != nil {
			rf = RxFree{Why: []string{fmt.Sprintf("internal error: %v", r)}}
		}
		if len(rf.Why) > rxMaxWhy {
			rf.Why = rf.Why[:rxMaxWhy]
		}
	}()
	why := func(format string, a ...any) RxFree {
		rf.Why = append(rf.Why, fmt.Sprintf(format, a...))
		return rf
	}
	// note adds a detail line, leaving two slots of the Debug text for the
	// reason the rule gives in the end.
	note := func(format string, a ...any) {
		if len(rf.Why) < rxMaxWhy-2 {
			rf.Why = append(rf.Why, fmt.Sprintf(format, a...))
		}
	}
	budget := rxFreeBudget(len(big))
	if budget == 0 {
		return why("%d non-little CPUs: the floor of loops leaves no CPU to free", len(big))
	}
	if src.fs == nil || src.ifaceAddrs == nil {
		return why("no source")
	}
	ifaces, reason := listenInterfaces(addr, src)
	if len(ifaces) == 0 {
		return why("no interface to look at: %s", reason)
	}
	devs := lowerDevices(ifaces, src)
	if len(devs) == 0 {
		return why("no physical device behind %s (virtual interfaces only)", strings.Join(ifaces, ","))
	}

	// 1. The receiving port.
	rxPkts := make([]uint64, len(devs))
	var total uint64
	best := 0
	for i, dev := range devs {
		n, ok := readRxPackets(dev, src)
		if !ok {
			return why("cannot read rx_packets of %s", dev)
		}
		rxPkts[i] = n
		total = satAdd(total, n)
		if n > rxPkts[best] {
			best = i
		}
	}
	port := devs[best]
	if !atLeastPercent(rxPkts[best], total, rxPortPercent) {
		return why("no port took %d%% of the %d packets received since boot (the largest, %s, took %d)",
			rxPortPercent, total, port, rxPkts[best])
	}
	if rxPkts[best] < rxMinPackets {
		return why("%s received %d packets since boot, under the %d that make the shares mean anything",
			port, rxPkts[best], rxMinPackets)
	}
	share := 100
	if total > 0 {
		share = int(min(100, uint64(float64(rxPkts[best])*100/float64(total))))
	}

	// 2. The busy vectors and their CPUs.
	var rows []irqRow
	if b, err := src.fs.readFile(src.procDir + "/interrupts"); err == nil {
		rows = parseInterrupts(b)
	}
	if len(rows) == 0 {
		return why("no usable %s/interrupts: cannot tell which vectors are busy", src.procDir)
	}
	table := make(map[int]irqRow, len(rows))
	for _, r := range rows {
		table[r.irq] = r
	}
	type vecInfo struct {
		RxVector
		dev string
	}
	var rxVecs, otherVecs []vecInfo
	seen := map[int]bool{}
	judged := 0
	for _, dev := range devs {
		irqs, ok := deviceIRQs(dev, src, rows)
		if !ok {
			return why("cannot list the interrupt vectors of %s", dev)
		}
		for _, irq := range irqs {
			if seen[irq] {
				continue
			}
			seen[irq] = true
			if judged++; judged > nicMaxVectors {
				break
			}
			row, listed := table[irq]
			if !listed || row.total < rxVectorMinInterrupts {
				continue
			}
			name := row.label
			if name == "" {
				name = "irq" + strconv.Itoa(irq)
			}
			cpu := effectiveCPU(irq, src)
			if cpu < 0 {
				return why("IRQ %d (%s) of %s took %d interrupts since boot but is not serviced by a single known CPU", irq, name, dev, row.total)
			}
			v := vecInfo{RxVector{IRQ: irq, Name: name, CPU: cpu, Count: row.total}, dev}
			if dev == port {
				rxVecs = append(rxVecs, v)
			} else {
				otherVecs = append(otherVecs, v)
			}
		}
	}

	// 3. A busy vector of another port on a CPU with no loop: step aside.
	for _, v := range otherVecs {
		if !slices.Contains(big, v.CPU) {
			return why("IRQ %d (%s) of %s, busy since boot, is serviced by CPU %d, which runs no loop: "+
				"its TX completion has no loop to stay beside and freeing %s's CPUs is not measured there",
				v.IRQ, v.Name, v.dev, v.CPU, port)
		}
	}
	for _, v := range otherVecs {
		note("kept: IRQ %d (%s) of %s on CPU %d is busy and keeps its loop", v.IRQ, v.Name, v.dev, v.CPU)
	}

	// 4. The candidates.
	slices.SortFunc(rxVecs, func(a, b vecInfo) int {
		if a.Count != b.Count {
			if a.Count > b.Count {
				return -1
			}
			return 1
		}
		return a.IRQ - b.IRQ
	})
	var freed []int
	var freedVecs []RxVector
	for _, v := range rxVecs {
		switch {
		case !slices.Contains(big, v.CPU):
			note("passed over: IRQ %d (%s) is serviced by CPU %d, which runs no loop", v.IRQ, v.Name, v.CPU)
		case slices.ContainsFunc(otherVecs, func(o vecInfo) bool { return o.CPU == v.CPU }):
			note("passed over: CPU %d also services a busy vector of another port and keeps its loop (IRQ %d, %s)", v.CPU, v.IRQ, v.Name)
		default:
			if !slices.Contains(freed, v.CPU) {
				freed = append(freed, v.CPU)
			}
			freedVecs = append(freedVecs, v.RxVector)
		}
	}
	if len(freed) == 0 {
		return why("no busy vector of %s (%d%% of the packets) is serviced by a CPU that could lose its loop", port, share)
	}
	// 5. The floor.
	if len(freed) > budget {
		slices.Sort(freed)
		return why("%d CPUs (%s) service busy vectors of %s: more than the %d the floor of loops allows of %d non-little CPUs; "+
			"the load is spread over the queues, not on a hot spot",
			len(freed), FormatCPUs(freed), port, budget, len(big))
	}
	slices.Sort(freed)
	rf.Port, rf.SharePercent, rf.Freed, rf.Vectors = port, share, freed, freedVecs
	return rf
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
