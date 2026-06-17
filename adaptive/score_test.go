//go:build linux

package adaptive

import "testing"

func TestIoUringBiasConnFactorFalloff(t *testing.T) {
	tests := []struct {
		name      string
		conns     int64
		cpu       float64
		wantBias  float64
		tolerance float64
	}{
		{name: "below 128: zero", conns: 64, cpu: 0.9, wantBias: 0.0},
		{name: "at 128 boundary: connFactor 0.1", conns: 128, cpu: 1.0, wantBias: 0.1 * (1.0 - 0.3) / 0.7},
		{name: "256 conns peak: 0.5", conns: 2048, cpu: 1.0, wantBias: 0.5 * (1.0 - 0.3) / 0.7},
		{name: "4096 boundary: 0.5 still", conns: 4096, cpu: 1.0, wantBias: 0.5 * (1.0 - 0.3) / 0.7},
		{name: "4097: drops to 0.2", conns: 4097, cpu: 1.0, wantBias: 0.2 * (1.0 - 0.3) / 0.7},
		{name: "above 8192: zero", conns: 9000, cpu: 0.9, wantBias: 0.0},
		{name: "low CPU: zero cpuFactor", conns: 2048, cpu: 0.20, wantBias: 0.5 * 0.0},
		{name: "exactly 30% CPU: zero cpuFactor", conns: 2048, cpu: 0.30, wantBias: 0.5 * 0.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := TelemetrySnapshot{
				ActiveConnections: tt.conns,
				CPUUtilization:    tt.cpu,
			}
			got := ioUringBias(snap, true)
			if absDiff(got, tt.wantBias) > tt.tolerance {
				t.Errorf("ioUringBias(conns=%d, cpu=%.2f) = %v, want %v ± %v",
					tt.conns, tt.cpu, got, tt.wantBias, tt.tolerance)
			}
		})
	}
}

func TestComputeScoreTwoTerm(t *testing.T) {
	w := DefaultWeights()
	snap := TelemetrySnapshot{ThroughputRPS: 1000, ErrorRate: 0.05}
	got := computeScore(snap, w)
	want := 1.0*1000.0 - 2.0*0.05
	if got != want {
		t.Errorf("computeScore = %v, want %v", got, want)
	}
}

func absDiff(a, b float64) float64 {
	d := a - b
	if d < 0 {
		return -d
	}
	return d
}
