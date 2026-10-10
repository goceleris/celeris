//go:build linux

package epoll

import "github.com/goceleris/celeris/internal/platform"

// pinThreadToCPU is the call that pins a loop's locked thread. A variable so
// a test can make it fail.
var pinThreadToCPU = platform.PinToCPU

// planWorkerCPUs chooses the CPU of every loop. A variable so a test can
// stand in a plan.
var planWorkerCPUs = platform.PlanWorkerCPUs

// pinFailures collects the loops whose pin failed until the engine logs them
// (celeris#909).
var pinFailures platform.PinFailures

// pinOwnThread pins the calling (locked) loop thread to the CPU the engine
// planned for this loop. A loop planned unpinned (cpuID < 0: a little CPU of a
// big.LITTLE host is left to the scheduler) does nothing. A pin the kernel
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
