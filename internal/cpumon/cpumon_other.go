//go:build !linux

package cpumon

import (
	"runtime/metrics"
	"time"
)

// RuntimeMon estimates CPU utilization using Go runtime metrics.
type RuntimeMon struct {
	prevCPU  float64
	prevTime time.Time
	sample   []metrics.Sample
}

// NewRuntimeMon creates a runtime-based CPU monitor for non-Linux platforms.
func NewRuntimeMon() *RuntimeMon {
	m := &RuntimeMon{
		prevTime: time.Now(),
		sample:   []metrics.Sample{{Name: "/cpu/classes/user:cpu-seconds"}},
	}
	metrics.Read(m.sample)
	m.prevCPU = m.sample[0].Value.Float64()
	return m
}

// Sample returns approximate CPU utilization based on Go runtime metrics.
func (m *RuntimeMon) Sample() (CPUSample, error) {
	now := time.Now()
	metrics.Read(m.sample)
	cpuSec := m.sample[0].Value.Float64()

	elapsed := now.Sub(m.prevTime).Seconds()
	deltaCPU := cpuSec - m.prevCPU
	m.prevCPU = cpuSec
	m.prevTime = now

	util := 0.0
	if elapsed > 0 {
		util = deltaCPU / elapsed
		if util > 1.0 {
			util = 1.0
		}
	}

	return CPUSample{
		Utilization: util,
		Timestamp:   now,
	}, nil
}
