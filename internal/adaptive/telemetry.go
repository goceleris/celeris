//go:build linux

package adaptive

import (
	"time"

	"github.com/goceleris/celeris/internal/engine"
)

// TelemetrySnapshot captures a point-in-time view of engine performance,
// used by the controller to decide whether to switch engines.
type TelemetrySnapshot struct {
	// Timestamp is when this snapshot was taken.
	Timestamp time.Time
	// ThroughputRPS is the recent requests-per-second rate.
	ThroughputRPS float64
	// ErrorRate is the engine's fault rate over the last sampling interval:
	// errors counted by the engine, less the sends to peers that had already
	// gone away (see revertErrors), per request. It drives the error-rate
	// safety revert, so it is a measure of the ENGINE failing, not of the
	// clients leaving. It is not capped at 1: it is a ratio of two counters.
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

// revertErrors is the part of m.ErrorCount that says the ENGINE is failing,
// which is what the error-rate safety revert exists to catch.
//
// It leaves out ErrorSendPeerGone (celeris#856): an io_uring send completing
// with EPIPE, ECONNRESET, ECONNABORTED or ENOTCONN is a client that left
// before the response flushed. That is a property of the client population,
// and a streaming workload (SSE, a WebSocket hub) produces a burst of them
// whenever its subscribers drop at once, while the HTTP request count it is
// divided by is a handful of subscriptions. The ratio then reads 0.056 to 355
// with every client request answered, and the revert fires on an engine that
// is serving. epoll never counts it (celeris#645), so leaving it out also
// makes the two engines' rates the same measurement. It stays in ErrorCount
// and in its own bucket: Metrics() and the published series still show it.
func revertErrors(m engine.EngineMetrics) uint64 {
	return satSub(m.ErrorCount, m.ErrorSendPeerGone)
}

// satSub is a-b, or 0 when b > a. The counters it is used on only grow, so
// the zero is a guard against a bucket wired ahead of its total, not a case
// that is expected to occur.
func satSub(a, b uint64) uint64 {
	if b > a {
		return 0
	}
	return a - b
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
			deltaErrs := satSub(revertErrors(m), revertErrors(prev))
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
