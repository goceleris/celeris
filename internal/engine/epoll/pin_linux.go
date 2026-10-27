//go:build linux

package epoll

import (
	"log/slog"
	"slices"

	"github.com/goceleris/celeris/internal/platform"
)

// pinThreadToCPU is the call that pins a loop's locked thread. A variable so
// a test can make it fail.
var pinThreadToCPU = platform.PinToCPU

// planWorkerCPUs chooses the CPU of every loop for a listener bound to addr
// (the concrete address the engine binds). A variable so a test can stand in a
// plan.
var planWorkerCPUs = platform.PlanWorkerCPUsFor

// detectNUMA reads the host's NUMA topology, for the MaxEvents split in Listen.
// A variable so a test can stand in a topology.
var detectNUMA = platform.DetectNUMA

// splitMaxEvents is the MaxEvents a loop gets: the configured figure divided by
// the number of NUMA nodes the process's allowed CPU set meets, never under 64
// (celeris#885 item 31). It divided by the host's node count, so a process kept
// to one node (numactl --cpunodebind, a single-numa-node pod, a cpuset of one
// socket), whose every loop is on that node, still got MaxEvents / NumNodes.
// allowed is the plan's allowed set (platform.CPUPlan.Allowed). Where the nodes
// it meets cannot be told (no allowed set, no CPU lists for the nodes, or none
// of its CPUs on a listed node) the host's count is used, as before.
func splitMaxEvents(maxEvents int, topo platform.NUMATopology, allowed []int) int {
	nodes := topo.NumNodes
	if n := nodesMet(topo, allowed); n > 0 {
		nodes = n
	}
	if nodes > 1 {
		maxEvents /= nodes
		if maxEvents < 64 {
			maxEvents = 64
		}
	}
	return maxEvents
}

// nodesMet is the number of nodes of topo that hold a CPU of allowed; 0 when
// that cannot be told.
func nodesMet(topo platform.NUMATopology, allowed []int) int {
	if len(allowed) == 0 || len(topo.NodeCPUs) == 0 {
		return 0
	}
	n := 0
	for _, cpus := range topo.NodeCPUs {
		if slices.ContainsFunc(cpus, func(c int) bool { return slices.Contains(allowed, c) }) {
			n++
		}
	}
	return n
}

// annotateNICIRQs adds the report of the CPUs that service the interrupts of
// the NIC the listener is reachable through to a plan that is already decided
// (celeris#1036). It only reports: the loops are the plan's. A variable so a
// test can stand in the host's interrupts.
var annotateNICIRQs = platform.AnnotatePlanWithNICIRQs

// nicIRQReport is annotateNICIRQs when the logger shows a Warn, and the plan as
// it is otherwise: the report reads files, and nothing of it would be logged.
func nicIRQReport(log *slog.Logger, plan platform.CPUPlan, addr string) platform.CPUPlan {
	if !platform.NICIRQReportWanted(log) {
		return plan
	}
	return annotateNICIRQs(plan, addr)
}

// The plan is also the loop count: the engine starts len(plan.CPUs) of them
// (celeris#1036), which on a big.LITTLE host is fewer than the default.

// pinFailures collects the loops whose pin failed until the engine logs them
// (celeris#909).
var pinFailures platform.PinFailures

// pinOwnThread pins the calling (locked) loop thread to the CPU the engine
// planned for this loop. A loop whose planned CPU is negative (cpuID < 0: the
// planner never plans one since celeris#1036, a test plan may) does nothing. A pin the kernel
// refuses leaves the loop unpinned and says so: cpuID becomes -1, which is
// what CPUID reports for a loop that is not pinned, and the failure is
// recorded for the engine to log.
func (l *Loop) pinOwnThread() {
	if l.cpuID < 0 {
		return
	}
	if err := pinThreadToCPU(l.cpuID); err != nil {
		pinFailures.Record(l, l.cpuID, err)
		l.cpuID = -1
	}
}

// takePinFailures removes and returns the pin failures of loops.
func takePinFailures(loops []*Loop) []platform.PinFailure {
	var out []platform.PinFailure
	for _, l := range loops {
		if f, ok := pinFailures.Take(l); ok {
			out = append(out, f)
		}
	}
	return out
}
