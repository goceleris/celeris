//go:build linux

// Package platform provides OS-level helpers for CPU pinning and NUMA distribution.
package platform

import (
	"errors"
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
// A negative cpu is refused (EINVAL): the mask would name a far-away bit, and
// "no CPU" is how the engines say "unpinned" (a plan entry of -1, which
// PlanWorkerCPUs does not produce since celeris#1036). So is a cpu
// past maxAffinityBits, which no kernel has. A cpu of 1024 or more gets a mask
// as long as it needs: a 1024-bit unix.CPUSet drops such a bit and asks the
// kernel for an empty mask.
func PinToCPU(cpu int) error {
	if cpu < 0 || cpu >= maxAffinityBits {
		return unix.EINVAL
	}
	set := unix.NewCPUSet(max(cpu+1, initialAffinityBits))
	set.Set(cpu)
	return setAffinity(set)
}

// ThreadAffinity is the CPU affinity mask of an OS thread, saved by
// SaveThreadAffinity so that Restore can put it back.
type ThreadAffinity struct {
	set unix.CPUSetDynamic
}

// SaveThreadAffinity returns the calling OS thread's CPU affinity mask.
//
// A goroutine that pins its thread (PinToCPU) saves the mask first and
// restores it before the thread can run Go code again. The engine loops never
// unlock their thread, so the runtime terminates it when the loop goroutine
// exits; the one exception is the main thread, which cannot exit and which the
// runtime parks for good instead, and Restore is what leaves that one with the
// mask the process started with (celeris#905).
//
// The mask is read into a buffer as long as the kernel's: sched_getaffinity
// answers EINVAL to a buffer under the kernel's mask size, which is more than
// the 1024 bits of a unix.CPUSet on a kernel with more than 1024 possible
// CPUs. The buffer starts at 1024 bits and doubles on EINVAL up to
// maxAffinityBits (8 KiB, what the Go runtime reads). A kernel beyond that
// is an error. Not tested on a real host with more than 1024 CPUs, only on a
// stand-in kernel (affinity_linux_test.go).
func SaveThreadAffinity() (ThreadAffinity, error) {
	set, err := getAffinity()
	if err != nil {
		return ThreadAffinity{}, err
	}
	return ThreadAffinity{set: set}, nil
}

// Restore sets the calling OS thread's CPU affinity mask back to a.
func (a ThreadAffinity) Restore() error {
	if len(a.set) == 0 {
		return unix.EINVAL // the zero value: nothing was saved
	}
	return setAffinity(a.set)
}

const (
	// initialAffinityBits is the size of unix.CPUSet, and the first buffer
	// getAffinity tries.
	initialAffinityBits = 1024
	// maxAffinityBits is the largest mask getAffinity reads and PinToCPU
	// builds: 8 KiB, the Go runtime's own bound (runtime/os_linux.go
	// getCPUCount). Linux builds with at most 8192 CPUs.
	maxAffinityBits = 65536
)

// getAffinity reads the calling thread's CPU mask into a buffer as long as the
// kernel's, trying initialAffinityBits and doubling on EINVAL up to
// maxAffinityBits.
func getAffinity() (unix.CPUSetDynamic, error) {
	var err error
	for bits := initialAffinityBits; bits <= maxAffinityBits; bits *= 2 {
		set := unix.NewCPUSet(bits)
		err = sysSchedGetaffinity(0, setBytes(set), unsafe.Pointer(&set[0]))
		if err == nil {
			return set, nil
		}
		if !errors.Is(err, unix.EINVAL) {
			return nil, err
		}
	}
	return nil, err
}

// setAffinity sets the calling thread's CPU mask to set.
func setAffinity(set unix.CPUSetDynamic) error {
	return sysSchedSetaffinity(0, setBytes(set), unsafe.Pointer(&set[0]))
}

// setBytes is the size of set in bytes.
func setBytes(set unix.CPUSetDynamic) uintptr {
	return uintptr(len(set)) * unsafe.Sizeof(set[0])
}

// setBits is how many CPUs set can name.
func setBits(set unix.CPUSetDynamic) int {
	return len(set) * int(unsafe.Sizeof(set[0])) * 8
}

// sysSchedGetaffinity and sysSchedSetaffinity are the two affinity syscalls,
// as variables so a test can stand in for a kernel the test host is not: one
// with more than 1024 possible CPUs, whose sched_getaffinity answers EINVAL to
// a buffer under its mask size. size is in bytes. They are replaced only by
// tests, which restore them (and so must not run in parallel).
var (
	sysSchedGetaffinity = func(pid int, size uintptr, ptr unsafe.Pointer) error {
		_, _, errno := unix.RawSyscall(unix.SYS_SCHED_GETAFFINITY, uintptr(pid), size, uintptr(ptr))
		if errno != 0 {
			return errno
		}
		return nil
	}
	sysSchedSetaffinity = func(pid int, size uintptr, ptr unsafe.Pointer) error {
		_, _, errno := unix.RawSyscall(unix.SYS_SCHED_SETAFFINITY, uintptr(pid), size, uintptr(ptr))
		if errno != 0 {
			return errno
		}
		return nil
	}
)

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
