//go:build linux

// Package platform provides OS-level helpers for CPU pinning and NUMA distribution.
package platform

import (
	"os"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// PinToCPU pins the calling OS thread to the given CPU core via sched_setaffinity.
//
// The affinity belongs to the OS thread, not to the goroutine: the caller
// must hold the thread with runtime.LockOSThread for as long as the pin is
// wanted, and must not let the thread run any other goroutine afterwards
// (see SaveThreadAffinity, celeris#905).
//
// A negative cpu is refused (EINVAL): unix.CPUSet.Set would turn -1 into a
// far-away bit, and "no CPU" is how the engines say "unpinned" (PlanWorkerCPUs).
func PinToCPU(cpu int) error {
	if cpu < 0 {
		return unix.EINVAL
	}
	var set unix.CPUSet
	set.Zero()
	set.Set(cpu)
	return schedSetaffinity(0, &set)
}

// ThreadAffinity is the CPU affinity mask of an OS thread, saved by
// SaveThreadAffinity so that Restore can put it back.
type ThreadAffinity struct {
	set unix.CPUSet
}

// SaveThreadAffinity returns the calling OS thread's CPU affinity mask.
//
// A goroutine that pins its thread (PinToCPU) saves the mask first and
// restores it before the thread can run Go code again. The engine loops never
// unlock their thread, so the runtime terminates it when the loop goroutine
// exits; the one exception is the main thread, which cannot exit and which the
// runtime parks for good instead, and Restore is what leaves that one with the
// mask the process started with (celeris#905).
func SaveThreadAffinity() (ThreadAffinity, error) {
	var a ThreadAffinity
	if err := unix.SchedGetaffinity(0, &a.set); err != nil {
		return ThreadAffinity{}, err
	}
	return a, nil
}

// Restore sets the calling OS thread's CPU affinity mask back to a.
func (a ThreadAffinity) Restore() error {
	return schedSetaffinity(0, &a.set)
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

// BindNumaNode sets the calling thread's memory allocation policy to MPOL_BIND
// for the given NUMA node. All subsequent mmap allocations will be satisfied
// from that node's memory. Returns an error if the syscall fails; the caller
// should treat failure as non-fatal (allocations will use default policy).
func BindNumaNode(node int) error {
	if node < 0 || node > 63 {
		return nil // out of range, skip
	}
	nodemask := uint64(1) << uint(node)
	return setMempolicy(mpolBind, &nodemask, 65)
}

// ResetNumaPolicy restores the default (local) memory allocation policy.
func ResetNumaPolicy() error {
	return setMempolicy(mpolDefault, nil, 0)
}

const (
	mpolDefault = 0
	mpolBind    = 2
)

func setMempolicy(mode int, nodemask *uint64, maxnode uintptr) error {
	_, _, errno := unix.RawSyscall(
		unix.SYS_SET_MEMPOLICY,
		uintptr(mode),
		uintptr(unsafe.Pointer(nodemask)),
		maxnode,
	)
	if errno != 0 {
		return errno
	}
	return nil
}

// NUMATopology holds detected NUMA topology information.
type NUMATopology struct {
	NumNodes int
	NodeCPUs [][]int
}

// DetectNUMA probes the system's NUMA topology via sysfs.
// Returns NumNodes=1 if NUMA info is unavailable.
func DetectNUMA() NUMATopology {
	entries, err := os.ReadDir("/sys/devices/system/node")
	if err != nil {
		return NUMATopology{NumNodes: 1}
	}
	numNodes := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() && len(name) > 4 && name[:4] == "node" {
			if _, err := strconv.Atoi(name[4:]); err == nil {
				numNodes++
			}
		}
	}
	if numNodes == 0 {
		return NUMATopology{NumNodes: 1}
	}
	nodeCPUs := readNodeCPUs(numNodes)
	return NUMATopology{
		NumNodes: numNodes,
		NodeCPUs: nodeCPUs,
	}
}

// CPUForNode returns the NUMA node that the given CPU belongs to.
// Returns 0 if the node cannot be determined (safe default).
func CPUForNode(cpu int) int {
	// /sys/devices/system/cpu/cpuN/node<X> symlinks
	path := "/sys/devices/system/cpu/cpu" + strconv.Itoa(cpu)
	entries, err := os.ReadDir(path)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "node") && e.IsDir() {
			n, err := strconv.Atoi(name[4:])
			if err == nil {
				return n
			}
		}
	}
	// Fallback: read /sys/devices/system/cpu/cpuN/topology/physical_package_id
	data, err := os.ReadFile(path + "/topology/physical_package_id")
	if err == nil {
		n, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err == nil {
			return n
		}
	}
	return 0
}

// readNodeCPUs reads /sys/devices/system/node/nodeN/cpulist for each node
// and returns a slice of CPU ID lists per node. Returns nil if sysfs is
// unavailable.
func readNodeCPUs(numaNodes int) [][]int {
	result := make([][]int, numaNodes)
	anyFound := false
	for node := range numaNodes {
		path := "/sys/devices/system/node/node" + strconv.Itoa(node) + "/cpulist"
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		cpus := parseCPUList(strings.TrimSpace(string(data)))
		if len(cpus) > 0 {
			result[node] = cpus
			anyFound = true
		}
	}
	if !anyFound {
		return nil
	}
	return result
}
