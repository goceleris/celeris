// Package cpumon provides CPU utilization monitoring with platform-specific implementations.
package cpumon

import "time"

// CPUSample is a point-in-time CPU utilization measurement.
type CPUSample struct {
	Utilization float64
	Timestamp   time.Time
}

// Monitor samples CPU utilization.
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
