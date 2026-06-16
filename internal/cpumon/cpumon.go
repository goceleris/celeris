// Package cpumon provides CPU utilization monitoring with platform-specific implementations.
package cpumon

import (
	"errors"
	"time"

	"github.com/goceleris/celeris/engine"
)

// ErrClosed is returned by Sample after the monitor has been closed.
var ErrClosed = errors.New("cpumon: monitor closed")

// CPUSample is a point-in-time CPU utilization measurement. It is an alias of
// the public engine.CPUSample so the internal implementations (ProcStat,
// RuntimeMon) directly satisfy the public engine.CPUMonitor interface and can
// be passed into adaptive.New from outside the module.
type CPUSample = engine.CPUSample

// Monitor samples CPU utilization. It is an alias of the public
// engine.CPUMonitor interface, keeping internal/cpumon as the implementation
// while the type surfaced in public signatures lives in the engine root.
type Monitor = engine.CPUMonitor

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
