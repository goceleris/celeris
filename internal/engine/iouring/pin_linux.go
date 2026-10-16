//go:build linux

package iouring

import "github.com/goceleris/celeris/internal/platform"

// pinThreadToCPU is the call that pins a worker's locked thread. A variable so
// a test can make it fail.
var pinThreadToCPU = platform.PinToCPU

// planWorkerCPUs chooses the CPU of every worker. A variable so a test can
// stand in a plan.
var planWorkerCPUs = platform.PlanWorkerCPUs

// pinFailures collects the workers whose pin failed until the engine logs them
// (celeris#909).
var pinFailures platform.PinFailures

// floatMask is the plan's FloatMask of the engine that is starting (AB ARM C2,
// throwaway): the CPUs a loop planned unpinned is restricted to.
var floatMask []int

// pinOwnThread pins the calling (locked) worker thread to the CPU the engine
// planned for this worker. A worker planned unpinned (cpuID < 0: a little CPU
// of a big.LITTLE host is left to the scheduler) does nothing. A pin the
// kernel refuses leaves the worker unpinned and says so: cpuID becomes -1,
// which is what CPUID reports for a worker that is not pinned and what
// NewRingCPU reads as "no SQPOLL thread affinity", and the failure is recorded
// for the engine to log.
func (w *Worker) pinOwnThread() {
	if w.cpuID < 0 {
		// AB ARM C2 (throwaway): a loop planned unpinned still may not run on
		// a little CPU, nor on any CPU outside the mask.
		if len(floatMask) > 0 {
			if err := platform.PinToCPUsMask(floatMask); err != nil {
				pinFailures.Record(w, -1, err)
			}
		}
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
