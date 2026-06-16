//go:build !linux

// Package platform provides platform-specific CPU affinity and worker distribution utilities.
package platform

import "runtime"

// DetectCoreTopology returns a single-node, no-SMT view on non-Linux platforms:
// Physical == Logical == runtime.NumCPU, SMT == 1. Sysfs topology probing is
// Linux-only, so the worker formula treats every logical CPU as a physical core
// here (which collapses the per-engine SMT adjustments to the historical
// numCPU sizing).
func DetectCoreTopology() CoreTopology {
	n := runtime.NumCPU()
	if n < 1 {
		n = 1
	}
	return CoreTopology{Logical: n, Physical: n, SMT: 1, NUMANodes: 1}
}

// PinToCPU is a no-op on non-Linux platforms.
func PinToCPU(_ int) error {
	return nil
}

// BindNumaNode is a no-op on non-Linux platforms.
func BindNumaNode(_ int) error {
	return nil
}

// ResetNumaPolicy is a no-op on non-Linux platforms.
func ResetNumaPolicy() error {
	return nil
}

// CPUForNode returns 0 on non-Linux platforms (no NUMA info).
func CPUForNode(_ int) int {
	return 0
}

// NUMATopology holds detected NUMA topology information.
type NUMATopology struct {
	NumNodes int
	NodeCPUs [][]int
}

// DetectNUMA returns a single-node topology on non-Linux platforms.
func DetectNUMA() NUMATopology {
	return NUMATopology{NumNodes: 1}
}

// DistributeWorkers returns CPU IDs for numWorkers using round-robin.
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
