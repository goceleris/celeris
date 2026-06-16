//go:build linux

package celeris

import (
	"fmt"

	"github.com/goceleris/celeris/internal/cpumon"
)

func newPlatformCPUMonitor() (cpumon.Monitor, error) {
	m, err := cpumon.NewProcStat()
	if err != nil {
		return nil, fmt.Errorf("procstat: %w", err)
	}
	return m, nil
}
