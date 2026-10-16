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
	// gone away (see revertErrors), divided by RateDenominator (see faultRate:
	// the larger of the interval's requests and its connection-seconds). It
	// drives the error-rate safety revert, so it is a measure of the ENGINE
	// failing, not of the clients leaving. It is not capped at 1: it is a
	// ratio of two counters.
	ErrorRate float64
	// RateDenominator is what ErrorRate was divided by: max(requests,
	// ConnSeconds) of the interval, 0 on the first sample of an engine and for
	// an interval with neither. The controller logs it with a switch so the
	// rate can be recomputed from the line (celeris#886 item 9).
	RateDenominator float64
	// ConnSeconds is the interval's connection-seconds: the live connections
	// integrated over its length, estimated as the mean of the gauge at the
	// two samples times the elapsed seconds (see connSeconds). 0 on the first
	// sample of an engine.
	ConnSeconds float64
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
	// ErrorDelta is the interval's growth of each Error* bucket of
	// EngineMetrics, indexed like errorBucketNames. Unlike ErrorRate it keeps
	// ErrorSendPeerGone: it exists so a switch can be attributed to a bucket
	// after the fact (celeris#886), and the controller logs the non-zero ones
	// with the "engine switch recommended" line. All zero on the first sample
	// of an engine, which has no baseline.
	ErrorDelta [len(errorBucketNames)]uint64
}

// errorBucketNames is every Error* bucket of EngineMetrics that
// engine.FillErrorClasses fills, ErrorCount (their sum) excluded, in the order
// of errorDeltas, with the attribute name the controller logs each under.
// TestSwitchLineNamesEveryErrorField886 fails when a bucket is added to
// EngineMetrics and not here, or listed in one of the two places and not the
// other.
var errorBucketNames = [...]string{
	"error_accept_fd_limit",
	"error_accept_cancelled",
	"error_accept_other",
	"error_conn_table_cap",
	"error_conn_register",
	"error_listener_recreate",
	"error_transplant_adopt",
	"error_send_peer_gone",
	"error_send",
	"error_request_body",
	"error_handler",
}

// errorDeltas is the growth of each Error* bucket from prev to m, saturating
// at 0, in the order of errorBucketNames. A direct function and not a table of
// getters: calling a getter through a func value made Sample move both
// EngineMetrics copies to the heap.
func errorDeltas(m, prev *engine.EngineMetrics) [len(errorBucketNames)]uint64 {
	return [len(errorBucketNames)]uint64{
		satSub(m.ErrorAcceptFDLimit, prev.ErrorAcceptFDLimit),
		satSub(m.ErrorAcceptCancelled, prev.ErrorAcceptCancelled),
		satSub(m.ErrorAcceptOther, prev.ErrorAcceptOther),
		satSub(m.ErrorConnTableCap, prev.ErrorConnTableCap),
		satSub(m.ErrorConnRegister, prev.ErrorConnRegister),
		satSub(m.ErrorListenerRecreate, prev.ErrorListenerRecreate),
		satSub(m.ErrorTransplantAdopt, prev.ErrorTransplantAdopt),
		satSub(m.ErrorSendPeerGone, prev.ErrorSendPeerGone),
		satSub(m.ErrorSend, prev.ErrorSend),
		satSub(m.ErrorRequestBody, prev.ErrorRequestBody),
		satSub(m.ErrorHandler, prev.ErrorHandler),
	}
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
	// clock is the time source, nil for time.Now; tests set it to control the
	// length of a sampling interval.
	clock func() time.Time
}

func (s *liveSampler) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}

// revertErrors is the part of m.ErrorCount that says the ENGINE is failing,
// which is what the error-rate safety revert exists to catch.
//
// It leaves out ErrorSendPeerGone (celeris#856): an io_uring send completing
// with EPIPE, ECONNRESET, ECONNABORTED or ENOTCONN is a client that left
// before the response flushed. That is a property of the client population,
// and a streaming workload (SSE, a WebSocket hub) produces a burst of them
// whenever its subscribers drop at once, while the HTTP request count that
// used to be the whole denominator is a handful of subscriptions. The ratio
// then read 0.056 to 355 with every client request answered, and the revert
// fired on an engine that was serving. epoll never counts it (celeris#645), so
// leaving it out removes the send-side asymmetry between the two engines'
// rates. send_peer_gone stays in ErrorCount and in its own bucket: Metrics()
// and the published series still show it.
//
// Everything else in ErrorCount stays in the rate, an accept that failed with
// ECONNABORTED (a peer that reset between SYN and accept) among it: both
// engines count it in AcceptCancelled since celeris#886 (epoll retries it in
// place and counts it). The controller reads the rate only while io_uring is
// the active engine, so on epoll that count changes the published figures and
// nothing a switch or revert decides on.
//
// What the part is divided by is faultRate's business (celeris#886 item 9).
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

// connSeconds is the live connections integrated over an interval of elapsed
// seconds, estimated from the gauge at its two ends: the trapezoid, mean of
// the two readings times the length. The sampler sees only the two readings,
// so a connection that opened and closed between them is not in it (the
// requests it made are, in the request count). The estimate is good over the
// sampler's 1 s tick; some intervals are much longer (the first after a switch
// spans the 30 s cooldown, the first after the oscillation lock up to 5 min),
// and over one of those a hub that filled and emptied between the two readings
// is in it as 0 connection-seconds, so faultRate falls back to the request
// count for that interval, the divisor it had before celeris#886 item 9. A gauge
// that reads negative counts as 0.
func connSeconds(prevActive, curActive int64, elapsed float64) float64 {
	return float64(max(prevActive, 0)+max(curActive, 0)) / 2 * elapsed
}

// faultRate is the error rate the safety revert compares with errorRevertRate:
// faults divided by max(reqs, connSecs), and that divisor (celeris#886 item 9).
//
// A request/response workload keeps its rate per request. A streaming
// workload (SSE, a WebSocket hub) is a handful of requests, the subscriptions,
// holding many connections for a long time, and a real send fault on it was
// divided by that handful: 7 faults over 10 requests and 200 connections read
// 0.7 and reverted an engine that was serving. Each live connection-second now
// counts as one unit of work, as a request does, so the same interval reads
// 7/200 = 0.035. Both measures are totals over the same interval, so
// which of them is the larger is a comparison of two per-second rates and does
// not depend on how long the interval is (the first interval after a switch is
// as long as the cooldown).
//
// The weight is 1 because the revert's own unit is "a fault per unit of
// work", and a connection that is open for a second is a unit of work as much
// as a request is: with it, any workload that averages at least one request
// per connection per second keeps exactly the rate it had. Below that
// (thousands of mostly idle keep-alive connections) the rate is lower than it
// was, by the factor reqs/connSecs, so the revert is less sensitive there:
// the cost of not reading a streaming workload as faulty.
//
// An interval with no request used to have rate 0 whatever the faults. It now
// has the faults per connection-second, so faults on a stream with no new
// request count. An interval with neither requests nor connections (nothing
// to divide by) is 0, and so is one whose counters fell (satSub). There is no
// floor on the faults or on the divisor: one accept error (AcceptCancelled
// counts ECONNABORTED, EINTR, ECANCELED, EBADF) in a second of fewer than 20
// connection-seconds and no request reads above errorRevertRate, as one fault
// in one to nineteen requests did before.
func faultRate(faults, reqs uint64, connSecs float64) (rate, denom float64) {
	denom = max(float64(reqs), connSecs)
	if denom <= 0 {
		return 0, 0
	}
	return float64(faults) / denom, denom
}

func newLiveSampler(cpuMon engine.CPUMonitor) *liveSampler {
	return &liveSampler{
		prevMetrics: make(map[engine.EngineType]engine.EngineMetrics),
		prevTime:    make(map[engine.EngineType]time.Time),
		cpuMon:      cpuMon,
	}
}

// Rebase makes e's current counters the baseline the next Sample of it is
// measured from (celeris#856). The sampler keeps one baseline per engine type
// and the controller samples only the ACTIVE engine, so without a rebase the
// first sample after an engine becomes active again reaches back to the last
// sample taken before it was switched away from: the whole time it spent as the
// standby, including its own teardown at the pause (the cancelled accept of
// every worker, celeris#645) and the stragglers it served while draining. Those
// are divided by the few requests it counted, and a window that long is not
// the engine's fault rate now.
func (s *liveSampler) Rebase(e engine.Engine) {
	et := e.Type()
	s.prevMetrics[et] = e.Metrics()
	s.prevTime[et] = s.now()
}

func (s *liveSampler) Sample(e engine.Engine) TelemetrySnapshot {
	now := s.now()
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
			deltaReqs := satSub(m.RequestCount, prev.RequestCount)
			deltaErrs := satSub(revertErrors(m), revertErrors(prev))
			deltaAccepts := satSub(m.AcceptCount, prev.AcceptCount)
			deltaBytes := satSub(m.BytesRead+m.BytesWritten, prev.BytesRead+prev.BytesWritten)
			snap.ThroughputRPS = float64(deltaReqs) / elapsed
			snap.AcceptRate = float64(deltaAccepts) / elapsed
			snap.ErrorDelta = errorDeltas(&m, &prev)
			snap.ConnSeconds = connSeconds(prev.ActiveConnections, m.ActiveConnections, elapsed)
			snap.ErrorRate, snap.RateDenominator = faultRate(deltaErrs, deltaReqs, snap.ConnSeconds)
			if deltaReqs > 0 {
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
