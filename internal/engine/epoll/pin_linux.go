//go:build linux

package epoll

import (
	"log/slog"

	"github.com/goceleris/celeris/internal/platform"
)

// pinThreadToCPU is the call that pins a loop's locked thread. A variable so
// a test can make it fail.
var pinThreadToCPU = platform.PinToCPU

// planWorkerCPUs chooses the CPU of every loop for a listener bound to addr
// (the concrete address the engine binds). A variable so a test can stand in a
// plan.
var planWorkerCPUs = platform.PlanWorkerCPUsFor

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
