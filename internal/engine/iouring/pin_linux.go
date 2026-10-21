//go:build linux

package iouring

import (
	"log/slog"

	"github.com/goceleris/celeris/internal/platform"
)

// pinThreadToCPU is the call that pins a worker's locked thread. A variable so
// a test can make it fail.
var pinThreadToCPU = platform.PinToCPU

// planWorkerCPUs chooses the CPU of every worker for a listener bound to addr
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

// planWithinMemlock plans the workers and then holds the plan to what
// RLIMIT_MEMLOCK funds. The plan comes first because on a big.LITTLE host it
// decides the count (8 on a 12-CPU host with 4 little CPUs): a memlock ceiling
// of 9 to 11 workers then changes nothing and must not warn, and the cap that
// does bind is stated in the plan's own terms (requested = the loops planned).
// The planner fills distinct CPUs before it repeats one, so cutting the plan
// to its first ceiling entries keeps it balanced. Where the CPUs are alike this
// is the plan the planner makes for the capped count.
func planWithinMemlock(requested int, explicit bool, addr string, logger *slog.Logger) platform.CPUPlan {
	plan := planWorkerCPUs(requested, explicit, addr)
	if capped := capWorkersToMemlock(len(plan.CPUs), logger); capped < len(plan.CPUs) {
		plan = plan.Truncate(capped, "RLIMIT_MEMLOCK")
	}
	return plan
}

// pinFailures collects the workers whose pin failed until the engine logs them
// (celeris#909).
var pinFailures platform.PinFailures

// pinOwnThread pins the calling (locked) worker thread to the CPU the engine
// planned for this worker. A worker whose planned CPU is negative (cpuID < 0: the
// planner never plans one since celeris#1036, a test plan may) does nothing. A pin the
// kernel refuses leaves the worker unpinned and says so: cpuID becomes -1,
// which is what CPUID reports for a worker that is not pinned and what
// NewRingCPU reads as "no SQPOLL thread affinity", and the failure is recorded
// for the engine to log.
func (w *Worker) pinOwnThread() {
	if w.cpuID < 0 {
		return
	}
	if err := pinThreadToCPU(w.cpuID); err != nil {
		pinFailures.Record(w, w.cpuID, err)
		w.cpuID = -1
	}
}

// takePinFailures removes and returns the pin failures of workers.
func takePinFailures(workers []*Worker) []platform.PinFailure {
	var out []platform.PinFailure
	for _, w := range workers {
		if f, ok := pinFailures.Take(w); ok {
			out = append(out, f)
		}
	}
	return out
}
