//go:build linux

package observe

import "github.com/goceleris/celeris/internal/cpumon"

// NewCPUMonitor creates a platform-appropriate CPU utilization monitor.
// On Linux, it reads /proc/stat for accurate system-wide CPU usage.
// Returns an error if the monitor cannot be initialized.
func NewCPUMonitor() (CPUMonitor, error) {
	m, err := cpumon.NewProcStat()
	if err != nil {
		return nil, err
	}
	return &cpuMonAdapter{m: m}, nil
}
