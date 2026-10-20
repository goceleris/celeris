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
	// NoLoop are the busy vectors of the other ports serviced by a CPU that
	// runs no loop (a little CPU, or one outside the allowed set), whatever
	// their share of the interrupts: the vectors that made the rule step aside
	// (step 4). Empty unless it did. The interrupt report warns about the ones
	// on a little CPU of the allowed set.
	NoLoop []RxVector
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

// vecInfo is a busy vector the rule judged, with the ports that list it.
type vecInfo struct {
	RxVector
	ports []string
}

// from says whose vector it is, for the Debug text.
func (v vecInfo) from() string {
	if len(v.ports) > 1 {
		return "shared by " + strings.Join(v.ports, ", ")
	}
	return "of " + v.ports[0]
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
	// rxStayPercent is the share of the pooled packets a port needs for one of
	// its busy vectors to count as the vector a loop stays beside (step 7). A
	// choice, not a measurement: msr1's enp1s0 carries about 27% and enp49s0
	// about 73%; a management port beside a data port carries far less.
	rxStayPercent = 10
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
//     single CPU of effective_affinity_list (else smp_affinity_list). A vector
//     that more than one of the ports lists (the ports of an mlx4 card share
//     their MSI-X vectors) is not the receiving port's alone: it counts with
//     the other ports'.
//  3. A port with a million packets or more and no busy vector is a
//     contradiction (a wrapped 32-bit counter, or vectors the rule cannot see):
//     the vector a loop should stay beside may be the hidden one, so the rule
//     steps aside. So it does when the ports list more than nicMaxVectors
//     vectors, which it does not read whole.
//  4. If a busy vector of ANOTHER port is serviced by a CPU that runs no loop
//     (a little CPU, or outside the allowed set), the rule steps aside. A loop
//     must stay on a TX-completion vector CPU (the #1036 stall finding); CPU 4
//     hosting enp1s0's vector is the msr1 boot placement, and its plan (CPUs 4
//     and 8 both without a loop) was not measured.
//  5. The candidates are the big CPUs that service a busy vector of the
//     receiving port, minus those that also service a busy vector of another
//     port: a CPU that hosts another port's TX completion keeps its loop.
//  6. At most rxFreeBudget(n) CPUs may go (the floor of loops, n big CPUs). More
//     candidates than that is how an RSS spread looks, with no hot spot: the
//     rule frees nothing rather than pick among equals.
//  7. At least one busy vector of the listener's ports must stay on a big CPU
//     that keeps its loop: the rule's premise is a loop kept beside a
//     TX-completion vector, and only the vectors of a port with at least
//     rxStayPercent of the pooled rx_packets count. With a single port, or when
//     no other port with that share has a busy vector on a CPU that keeps its
//     loop (a data port and a management port), the plan is #1049's.
//
// The rule is for one loop on each big CPU, the default count of msr1
// (rxFreeFor): with fewer loops (GOMAXPROCS under the big CPU count) the CPUs
// that keep a loop are not the ones the plan spreads the loops on, and the rule
// does not run.
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

	// 2. The vectors of the ports, each judged once, and the ports that list it.
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
	listers := map[int][]string{}
	var order []int
	for _, dev := range devs {
		irqs, ok := deviceIRQs(dev, src, rows)
		if !ok {
			return why("cannot list the interrupt vectors of %s", dev)
		}
		for _, irq := range irqs {
			if _, listed := listers[irq]; !listed {
				if len(order) >= nicMaxVectors {
					return why("more than %d interrupt vectors behind the listener's ports: the rule does not read them all, "+
						"and a busy vector it skipped could be the one a loop stays beside", nicMaxVectors)
				}
				order = append(order, irq)
			}
			if !slices.Contains(listers[irq], dev) {
				listers[irq] = append(listers[irq], dev)
			}
		}
	}
	var rxVecs, otherVecs []vecInfo
	hasBusy := map[string]bool{}
	for _, irq := range order {
		row, listed := table[irq]
		if !listed || row.total < rxVectorMinInterrupts {
			continue
		}
		ports := listers[irq]
		for _, d := range ports {
			hasBusy[d] = true
		}
		name := row.label
		if name == "" {
			name = "irq" + strconv.Itoa(irq)
		}
		v := vecInfo{RxVector: RxVector{IRQ: irq, Name: name, Count: row.total}, ports: ports}
		if v.CPU = effectiveCPU(irq, src); v.CPU < 0 {
			return why("IRQ %d (%s) %s took %d interrupts since boot but is not serviced by a single known CPU", irq, name, v.from(), row.total)
		}
		// A vector the receiving port lists alone is its own; one that another
		// port lists too (shared MSI-X vectors) is also that port's TX
		// completion, whatever the order of the names.
		if len(ports) == 1 && ports[0] == port {
			rxVecs = append(rxVecs, v)
		} else {
			otherVecs = append(otherVecs, v)
		}
	}

	// 3. A port with packets and no busy vector: a wrapped counter, or vectors
	// the rule cannot see.
	for i, dev := range devs {
		if rxPkts[i] >= rxMinPackets && !hasBusy[dev] {
			return why("%s received %d packets since boot and has no busy vector (a wrapped 32-bit counter, or vectors the rule "+
				"cannot read): the vector a loop should stay beside may be that one", dev, rxPkts[i])
		}
	}

	// 4. A busy vector of another port on a CPU with no loop: step aside.
	var aside []vecInfo
	for _, v := range otherVecs {
		if !slices.Contains(big, v.CPU) {
			aside = append(aside, v)
			rf.NoLoop = append(rf.NoLoop, v.RxVector)
		}
	}
	if len(aside) > 0 {
		v := aside[0]
		return why("IRQ %d (%s) %s, busy since boot, is serviced by CPU %d, which runs no loop: "+
			"its TX completion has no loop to stay beside and freeing %s's CPUs is not measured there",
			v.IRQ, v.Name, v.from(), v.CPU, port)
	}
	for _, v := range otherVecs {
		note("kept: IRQ %d (%s) %s on CPU %d is busy and keeps its loop", v.IRQ, v.Name, v.from(), v.CPU)
	}

	// 5. The candidates.
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
	// 6. The floor.
	if len(freed) > budget {
		slices.Sort(freed)
		return why("%d CPUs (%s) service busy vectors of %s, but the floor of loops allows only %d of %d non-little CPUs to lose their loop; "+
			"the rule frees nothing rather than pick among equals",
			len(freed), FormatCPUs(freed), port, budget, len(big))
	}
	slices.Sort(freed)
	// 7. The premise: a loop that stays beside a busy vector of a port that
	// carries a real share of the packets (a management port with a few
	// interrupts is not a TX completion worth keeping a loop beside).
	carries := func(ports []string) bool {
		for _, d := range ports {
			if i := slices.Index(devs, d); i >= 0 && atLeastPercent(rxPkts[i], total, rxStayPercent) {
				return true
			}
		}
		return false
	}
	stays := false
	for _, vs := range [][]vecInfo{rxVecs, otherVecs} {
		for _, v := range vs {
			stays = stays || (slices.Contains(big, v.CPU) && !slices.Contains(freed, v.CPU) && carries(v.ports))
		}
	}
	if !stays {
		return why("freeing %s would leave no loop to stay beside a busy vector of a port with at least %d%% of the packets "+
			"(none keeps a loop): the rule keeps a loop beside a TX completion, and with none to stay beside the plan is not measured",
			FormatCPUs(freed), rxStayPercent)
	}
	rf.Port, rf.SharePercent, rf.Freed, rf.Vectors = port, share, freed, freedVecs
	return rf
}

// rxFreeFor is the rule for the default count of numWorkers loops. It is
// measured with one loop on each big CPU (msr1's default 8 of 8); below that
// (GOMAXPROCS under the big CPU count, the default under a Kubernetes CPU
// limit) #1049 spreads the few loops over the big CPUs in order, which are not
// the CPUs the rule keeps a loop on, so it steps aside before reading a file.
func rxFreeFor(numWorkers int, addr string, big []int, src nicSource) RxFree {
	if numWorkers < len(big) {
		return RxFree{Why: []string{fmt.Sprintf("%d loops by default, under the %d non-little CPUs: the rule is measured with a loop on each of them, "+
			"and with fewer the CPUs it means to keep may run none", numWorkers, len(big))}}
	}
	return rxFreeCPUs(addr, big, src)
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
