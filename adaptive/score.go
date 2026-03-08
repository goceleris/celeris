//go:build linux

package adaptive

// ScoreWeights defines the weighting for each telemetry signal in score computation.
type ScoreWeights struct {
	Throughput float64
	ErrorRate  float64
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
