// Package cpumon provides CPU utilization monitoring with platform-specific implementations.
package cpumon

import (
	"errors"
	"time"
)

// ErrClosed is returned by Sample after the monitor has been closed.
var ErrClosed = errors.New("cpumon: monitor closed")

// CPUSample is a point-in-time CPU utilization measurement supplied by a
// Monitor. Utilization is a fraction in [0,1]; zero means "no signal" and
// is the documented fallback when system-wide CPU data is unavailable.
//
// It is defined here, and engine.CPUSample is an alias of it, so that
// observe can use this package without importing the engine: the engine
// imports observe for EngineMetrics.
type CPUSample struct {
	Utilization float64
	Timestamp   time.Time
}

// Monitor samples CPU utilization. engine.CPUMonitor is an alias of it, and
// the adaptive engine accepts one, so a caller can supply its own monitor or
// the built-in /proc/stat implementation. Implementations must be safe for
// concurrent use: a single monitor may be sampled from more than one
// goroutine.
type Monitor interface {
	Sample() (CPUSample, error)
}

// Synthetic is a deterministic CPU monitor for testing.
type Synthetic struct {
	util float64
}

// NewSynthetic creates a synthetic monitor with initial utilization.
func NewSynthetic(initial float64) *Synthetic {
	return &Synthetic{util: initial}
}

// Set updates the synthetic utilization value.
func (s *Synthetic) Set(util float64) { s.util = util }

// Sample returns the current synthetic utilization.
func (s *Synthetic) Sample() (CPUSample, error) {
	return CPUSample{
		Utilization: s.util,
		Timestamp:   time.Now(),
	}, nil
}
