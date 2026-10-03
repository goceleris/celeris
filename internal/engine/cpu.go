package engine

import "time"

// CPUSample is a point-in-time CPU utilization measurement supplied by a
// CPUMonitor. Utilization is a fraction in [0,1]; zero means "no signal" and
// is the documented fallback when system-wide CPU data is unavailable.
type CPUSample struct {
	Utilization float64
	Timestamp   time.Time
}

// CPUMonitor samples CPU utilization. It is the public interface accepted by
// the adaptive engine so external callers can supply their own monitor (or the
// built-in /proc/stat implementation) without depending on an internal
// package. Implementations must be safe for concurrent use: a single monitor
// may be sampled from more than one goroutine.
type CPUMonitor interface {
	Sample() (CPUSample, error)
}
