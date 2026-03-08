//go:build linux

package adaptive

import (
	"time"

	"github.com/goceleris/celeris/engine"
)

// TelemetrySnapshot captures a point-in-time view of engine performance.
type TelemetrySnapshot struct {
	Timestamp         time.Time
	ThroughputRPS     float64
	ErrorRate         float64
	ActiveConnections int64
	CPUUtilization    float64
}

// TelemetrySampler produces telemetry snapshots from an engine.
type TelemetrySampler interface {
	Sample(e engine.Engine) TelemetrySnapshot
}

// liveSampler derives telemetry from engine metrics deltas.
type liveSampler struct {
	prevMetrics map[engine.EngineType]engine.EngineMetrics
	prevTime    map[engine.EngineType]time.Time
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
