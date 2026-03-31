//go:build !linux

package observe

import "github.com/goceleris/celeris/internal/cpumon"

// NewCPUMonitor creates a platform-appropriate CPU utilization monitor.
// On non-Linux platforms, it uses Go runtime/metrics for approximate CPU usage.
func NewCPUMonitor() (CPUMonitor, error) {
	return &cpuMonAdapter{m: cpumon.NewRuntimeMon()}, nil
}
