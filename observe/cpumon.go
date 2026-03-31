package observe

import "github.com/goceleris/celeris/internal/cpumon"

// cpuMonAdapter wraps the internal cpumon.Monitor to implement CPUMonitor.
type cpuMonAdapter struct {
	m cpumon.Monitor
}

func (a *cpuMonAdapter) Sample() (float64, error) {
	s, err := a.m.Sample()
	if err != nil {
		return -1, err
	}
	return s.Utilization, nil
}
