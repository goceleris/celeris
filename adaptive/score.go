//go:build linux

package adaptive

// ScoreWeights defines the weighting for each telemetry signal in score
// computation. Higher throughput weight favors faster engines; higher error
// weight penalizes unreliable ones.
type ScoreWeights struct {
	// Throughput is the weight applied to requests-per-second in the score.
	Throughput float64
	// ErrorRate is the penalty weight applied to the error fraction.
	ErrorRate float64
}

// DefaultWeights returns the default score weights.
func DefaultWeights() ScoreWeights {
	return ScoreWeights{
		Throughput: 1.0,
		ErrorRate:  2.0,
	}
}

// computeScore produces a weighted score from a telemetry snapshot.
// Higher is better.
func computeScore(snap TelemetrySnapshot, w ScoreWeights) float64 {
	return w.Throughput*snap.ThroughputRPS - w.ErrorRate*snap.ErrorRate
}

// ioUringBias returns a positive value when conditions favor io_uring over
// epoll, based on benchmark data. The bias is added to io_uring's score (or
// subtracted from epoll's) to enable proactive switching without requiring
// a throughput advantage.
//
// Signals from 3-run median benchmarks (2026-03-27):
//   - io_uring CPU efficiency: 2-8% less CPU at 256-4096 connections
//   - io_uring p99 tail: 17-39% better at 1024-4096 connections
//   - Throughput: equivalent (±2%) at 256-4096 connections
//   - io_uring loses at <64 connections (fixed overhead) and >8192 on x86
func ioUringBias(snap TelemetrySnapshot) float64 {
	conns := snap.ActiveConnections
	cpu := snap.CPUUtilization

	// No bias at very low or very high connection counts.
	if conns < 128 || conns > 8192 {
		return 0
	}

	// Base bias: proportional to connection count in the sweet spot.
	// Peaks at 1024-4096 connections where io_uring's advantages are strongest.
	var connFactor float64
	switch {
	case conns < 256:
		connFactor = 0.1
	case conns < 1024:
		connFactor = 0.3
	case conns <= 4096:
		connFactor = 0.5 // Peak: io_uring's best range
	default:
		connFactor = 0.2 // 4096-8192: still beneficial but declining
	}

	// CPU factor: bias increases when CPU-bound (io_uring's efficiency advantage
	// matters most when cores are saturated). No bias below 30% CPU.
	cpuFactor := 0.0
	if cpu > 0.3 {
		cpuFactor = (cpu - 0.3) / 0.7 // 0.0 at 30% CPU, 1.0 at 100%
	}

	// Combined bias: connection suitability × CPU pressure.
	// Maximum bias is 0.5 (50% of throughput score).
	return connFactor * cpuFactor
}
