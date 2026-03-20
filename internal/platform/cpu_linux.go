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

// DistributeWorkers returns CPU IDs for numWorkers distributed across NUMA
// nodes. On multi-socket systems, workers are interleaved across sockets so
// that each socket handles roughly equal traffic. Falls back to sequential
// round-robin when NUMA topology is unavailable.
func DistributeWorkers(numWorkers, numCPU, numaNodes int) []int {
	if numCPU <= 0 {
		numCPU = 1
	}
	if numaNodes <= 1 {
		// Single socket or NUMA unavailable: simple round-robin.
		cpus := make([]int, numWorkers)
		for i := range numWorkers {
			cpus[i] = i % numCPU
		}
		return cpus
	}

	// Read per-node CPU lists from sysfs.
	nodeCPUs := readNodeCPUs(numaNodes)
	if nodeCPUs == nil {
		// Sysfs unavailable: fall back to round-robin.
		cpus := make([]int, numWorkers)
		for i := range numWorkers {
			cpus[i] = i % numCPU
		}
		return cpus
	}

	// Interleave: assign workers round-robin across nodes, picking the
	// next unused CPU from each node in order.
	nodeIdx := make([]int, numaNodes)
	cpus := make([]int, numWorkers)
	for i := range numWorkers {
		node := i % numaNodes
		nodeCPUList := nodeCPUs[node]
		if len(nodeCPUList) == 0 {
			// Empty node: fall back to sequential.
			cpus[i] = i % numCPU
			continue
		}
		cpus[i] = nodeCPUList[nodeIdx[node]%len(nodeCPUList)]
		nodeIdx[node]++
	}
	return cpus
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
