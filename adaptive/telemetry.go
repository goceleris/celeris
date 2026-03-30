//go:build linux

package adaptive

import (
	"time"

	"github.com/goceleris/celeris/engine"
)

// TelemetrySnapshot captures a point-in-time view of engine performance,
// used by the controller to decide whether to switch engines.
type TelemetrySnapshot struct {
	// Timestamp is when this snapshot was taken.
	Timestamp time.Time
	// ThroughputRPS is the recent requests-per-second rate.
	ThroughputRPS float64
	// ErrorRate is the fraction of requests that resulted in errors (0.0-1.0).
	ErrorRate float64
	// ActiveConnections is the current number of open connections.
	ActiveConnections int64
	// CPUUtilization is the estimated CPU usage fraction (0.0-1.0). Currently unused.
	CPUUtilization float64
}

// TelemetrySampler produces telemetry snapshots from an engine.
type TelemetrySampler interface {
	Sample(e engine.Engine) TelemetrySnapshot
}

// liveSampler derives telemetry from engine metrics deltas and CPU monitoring.
type liveSampler struct {
	prevMetrics map[engine.EngineType]engine.EngineMetrics
	prevTime    map[engine.EngineType]time.Time
	cpuMon      cpuMonitor
}

// cpuMonitor abstracts CPU sampling (implemented by cpumon package).
type cpuMonitor interface {
	Sample() (float64, error) // returns utilization 0.0-1.0
}

func newLiveSampler() *liveSampler {
	return &liveSampler{
		prevMetrics: make(map[engine.EngineType]engine.EngineMetrics),
		prevTime:    make(map[engine.EngineType]time.Time),
	}
}

func (s *liveSampler) Sample(e engine.Engine) TelemetrySnapshot {
	now := time.Now()
	m := e.Metrics()
	et := e.Type()

	snap := TelemetrySnapshot{
		Timestamp:         now,
		ActiveConnections: m.ActiveConnections,
	}

	prev, hasPrev := s.prevMetrics[et]
	prevT, hasT := s.prevTime[et]
	if hasPrev && hasT {
		elapsed := now.Sub(prevT).Seconds()
		if elapsed > 0 {
			deltaReqs := m.RequestCount - prev.RequestCount
			deltaErrs := m.ErrorCount - prev.ErrorCount
			snap.ThroughputRPS = float64(deltaReqs) / elapsed
			if deltaReqs > 0 {
				snap.ErrorRate = float64(deltaErrs) / float64(deltaReqs)
			}
		}
	}

	s.prevMetrics[et] = m
	s.prevTime[et] = now

	// Sample CPU utilization if monitor is available.
	if s.cpuMon != nil {
		if util, err := s.cpuMon.Sample(); err == nil {
			snap.CPUUtilization = util
		}
	}

	return snap
}

// syntheticSampler returns pre-set telemetry for testing.
type syntheticSampler struct {
	snapshots map[engine.EngineType]TelemetrySnapshot
}

func newSyntheticSampler() *syntheticSampler {
	return &syntheticSampler{
		snapshots: make(map[engine.EngineType]TelemetrySnapshot),
	}
}

func (s *syntheticSampler) Set(et engine.EngineType, snap TelemetrySnapshot) {
	s.snapshots[et] = snap
}

func (s *syntheticSampler) Sample(e engine.Engine) TelemetrySnapshot {
	if snap, ok := s.snapshots[e.Type()]; ok {
		snap.Timestamp = time.Now()
		return snap
	}
	return TelemetrySnapshot{Timestamp: time.Now()}
}
