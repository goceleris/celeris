package overload

import "time"

// StageConfig defines the thresholds and timing for a single stage transition.
type StageConfig struct {
	EscalateThreshold   float64
	EscalateSustained   time.Duration
	DeescalateThreshold float64
	DeescalateSustained time.Duration
	Cooldown            time.Duration
}

// Config configures the overload manager.
type Config struct {
	Enabled  bool
	Stages   [6]StageConfig
	Interval time.Duration
}

// DefaultConfig returns the default overload configuration per SDD 10.1.
func DefaultConfig() Config {
	return Config{
		Enabled:  true,
		Interval: 1 * time.Second,
		Stages: [6]StageConfig{
			// Stage 0 (Normal) — no thresholds, always the base state.
			{},
			// Stage 1 (Expand)
			{
				EscalateThreshold:   0.70,
				EscalateSustained:   10 * time.Second,
				DeescalateThreshold: 0.60,
				DeescalateSustained: 30 * time.Second,
			},
			// Stage 2 (Reap)
			{
				EscalateThreshold:   0.80,
				EscalateSustained:   10 * time.Second,
				DeescalateThreshold: 0.70,
				DeescalateSustained: 10 * time.Second,
			},
			// Stage 3 (Reorder)
			{
				EscalateThreshold:   0.85,
				EscalateSustained:   10 * time.Second,
				DeescalateThreshold: 0.75,
				DeescalateSustained: 10 * time.Second,
				Cooldown:            5 * time.Second,
			},
			// Stage 4 (Backpressure)
			{
				EscalateThreshold:   0.90,
				EscalateSustained:   10 * time.Second,
				DeescalateThreshold: 0.80,
				DeescalateSustained: 10 * time.Second,
				Cooldown:            10 * time.Second,
			},
			// Stage 5 (Reject)
			{
				EscalateThreshold:   0.95,
				EscalateSustained:   5 * time.Second,
				DeescalateThreshold: 0.85,
				DeescalateSustained: 10 * time.Second,
				Cooldown:            15 * time.Second,
			},
		},
	}
}
