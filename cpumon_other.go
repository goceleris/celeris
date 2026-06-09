//go:build !linux

package celeris

import (
	"github.com/goceleris/celeris/internal/cpumon"
)

func newPlatformCPUMonitor() (cpumon.Monitor, error) {
	return cpumon.NewRuntimeMon(), nil
}
