//go:build !linux

// Package platform provides platform-specific CPU affinity and worker distribution utilities.
package platform

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
