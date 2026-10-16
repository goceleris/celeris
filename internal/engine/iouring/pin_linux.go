//go:build linux

package iouring

import "github.com/goceleris/celeris/internal/platform"

// pinThreadToCPU is the call that pins a worker's locked thread. A variable so
// a test can make it fail.
var pinThreadToCPU = platform.PinToCPU

// planWorkerCPUs chooses the CPU of every worker. A variable so a test can
// stand in a plan.
var planWorkerCPUs = platform.PlanWorkerCPUs

// The plan is also the loop count: the engine starts len(plan.CPUs) of them
// (celeris#1036), which on a big.LITTLE host is fewer than the default.

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
