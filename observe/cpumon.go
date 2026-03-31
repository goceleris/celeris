package observe

import "github.com/goceleris/celeris/internal/cpumon"

// closer is implemented by monitors that hold resources (e.g., ProcStat).
type closer interface {
	Close() error
}

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

func (a *cpuMonAdapter) Close() error {
	if c, ok := a.m.(closer); ok {
		return c.Close()
	}
	return nil
}
