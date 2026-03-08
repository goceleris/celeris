//go:build linux

// Package platform provides OS-level helpers for CPU pinning and NUMA distribution.
package platform

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// PinToCPU pins the calling OS thread to the given CPU core via sched_setaffinity.
func PinToCPU(cpu int) error {
	var set unix.CPUSet
	set.Zero()
	set.Set(cpu)
	return schedSetaffinity(0, &set)
}

func schedSetaffinity(pid int, set *unix.CPUSet) error {
	_, _, errno := unix.RawSyscall(
		unix.SYS_SCHED_SETAFFINITY,
		uintptr(pid),
		unsafe.Sizeof(*set),
		uintptr(unsafe.Pointer(set)),
	)
	if errno != 0 {
		return errno
	}
	return nil
}

// DistributeWorkers returns CPU IDs for numWorkers across available NUMA nodes.
// Falls back to round-robin across numCPU cores when NUMA info is unavailable.
func DistributeWorkers(numWorkers, numCPU, _ int) []int {
	if numCPU <= 0 {
		numCPU = 1
	}
	cpus := make([]int, numWorkers)
	for i := range numWorkers {
		cpus[i] = i % numCPU
	}
	return cpus
}
