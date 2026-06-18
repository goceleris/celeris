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
	// CPUUtilization is the estimated CPU usage fraction (0.0-1.0). Read from
	// the live sampler's CPUMonitor; zero when no monitor is wired.
	CPUUtilization float64
	// ConnsPerWorker is ActiveConnections divided by the engine's worker
	// count. This is the PRIMARY load signal driving engine selection:
	// epoll and io_uring tie at low conns/worker, but io_uring pulls ahead
	// and keeps scaling above ~20/worker while epoll plateaus.
	ConnsPerWorker float64
	// AcceptRate is the new-connection arrival rate (accepts/sec) over the
	// last sampling interval, derived like ThroughputRPS. A secondary load
	// signal: a high accept rate indicates connection churn.
	AcceptRate float64
	// BytesPerReq is the average payload size (read+written bytes per
	// request) over the last interval. When this exceeds the controller's
	// large-payload threshold the workload is link-bound — the engines tie
	// — so the controller suppresses an io_uring switch to avoid churn.
	BytesPerReq float64
}

// TelemetrySampler produces telemetry snapshots from an engine.
type TelemetrySampler interface {
	Sample(e engine.Engine) TelemetrySnapshot
}

// liveSampler derives telemetry from engine metrics deltas and CPU monitoring.
type liveSampler struct {
	prevMetrics map[engine.EngineType]engine.EngineMetrics
	prevTime    map[engine.EngineType]time.Time
	cpuMon      engine.CPUMonitor
}

func newLiveSampler(cpuMon engine.CPUMonitor) *liveSampler {
	return &liveSampler{
		prevMetrics: make(map[engine.EngineType]engine.EngineMetrics),
		prevTime:    make(map[engine.EngineType]time.Time),
		cpuMon:      cpuMon,
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

	// ConnsPerWorker is a point-in-time ratio (not a delta), so it needs no
	// prior sample. max(Workers, 1) guards the pre-Listen window where the
	// worker count is still zero.
	snap.ConnsPerWorker = float64(m.ActiveConnections) / float64(max(m.Workers, 1))

	prev, hasPrev := s.prevMetrics[et]
	prevT, hasT := s.prevTime[et]
	if hasPrev && hasT {
		elapsed := now.Sub(prevT).Seconds()
		if elapsed > 0 {
			deltaReqs := m.RequestCount - prev.RequestCount
			deltaErrs := m.ErrorCount - prev.ErrorCount
			deltaAccepts := m.AcceptCount - prev.AcceptCount
			deltaBytes := (m.BytesRead + m.BytesWritten) - (prev.BytesRead + prev.BytesWritten)
			snap.ThroughputRPS = float64(deltaReqs) / elapsed
			snap.AcceptRate = float64(deltaAccepts) / elapsed
			if deltaReqs > 0 {
				snap.ErrorRate = float64(deltaErrs) / float64(deltaReqs)
				snap.BytesPerReq = float64(deltaBytes) / float64(deltaReqs)
			}
		}
	}

	s.prevMetrics[et] = m
	s.prevTime[et] = now

	// Sample CPU utilization if monitor is available.
	if s.cpuMon != nil {
		if sample, err := s.cpuMon.Sample(); err == nil {
			snap.CPUUtilization = sample.Utilization
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
