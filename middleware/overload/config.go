package overload

import (
	"context"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/observe"
)

// Stage identifies one of the five CPU-pressure stages the driver can
// occupy. Stages rise as CPU utilization crosses upward thresholds
// and fall after CPU drops below (upper - Hysteresis).
type Stage int32

// Stage values.
const (
	StageNormal       Stage = iota // 0: no action
	StageExpand                    // 1: widen engine workers (best-effort)
	StageReap                      // 2: opt-in runtime.GC()
	StageReorder                   // 3: priority-gated handling
	StageBackpressure              // 4: delay low-priority; 503 others
	StageReject                    // 5: 503 all non-exempt
)

// String returns the stage name for diagnostics.
func (s Stage) String() string {
	switch s {
	case StageNormal:
		return "normal"
	case StageExpand:
		return "expand"
	case StageReap:
		return "reap"
	case StageReorder:
		return "reorder"
	case StageBackpressure:
		return "backpressure"
	case StageReject:
		return "reject"
	}
	return "unknown"
}

// Thresholds defines the upward transition CPU fractions (0.0..1.0).
// Each field is the lower bound for entering that stage. The downward
// transition threshold is (upper - Hysteresis).
type Thresholds struct {
	Expand       float64 // default 0.70
	Reap         float64 // default 0.80
	Reorder      float64 // default 0.85
	Backpressure float64 // default 0.90
	Reject       float64 // default 0.95
	Hysteresis   float64 // default 0.05
}

// Config defines the overload middleware configuration.
type Config struct {
	// CollectorProvider returns the [observe.Collector] from which
	// CPU utilization is sampled. Required.
	CollectorProvider func() *observe.Collector

	// Thresholds sets the per-stage CPU fractions. Zero-valued fields
	// fall back to the defaults documented on [Thresholds].
	Thresholds Thresholds

	// PollInterval is how often the background goroutine samples CPU
	// and updates the stage. Default: 1 second.
	PollInterval time.Duration

	// ExemptPaths are request paths that never get degraded.
	ExemptPaths []string

	// ExemptFunc, when non-nil, short-circuits the degradation logic
	// for requests where it returns true.
	ExemptFunc func(*celeris.Context) bool

	// PriorityFunc classifies request priority for Reorder and
	// Backpressure stages. Higher values win. When nil, all requests
	// share the same priority (0) so Reorder passes everything and
	// Backpressure delays everything.
	PriorityFunc func(*celeris.Context) int

	// PriorityThreshold is the cutoff below which requests are rejected
	// at StageReorder and delayed/rejected at StageBackpressure.
	// Default: 0 (priority < 0 is "low").
	PriorityThreshold int

	// BackpressureDelay is the artificial latency added at
	// StageBackpressure for non-exempt low-priority requests.
	// Default: 50 ms.
	BackpressureDelay time.Duration

	// BackpressureStatus is the status code returned at StageBackpressure
	// for non-delayable requests. Default: 503.
	BackpressureStatus int

	// RejectStatus is the status code returned at StageReject.
	// Default: 503.
	RejectStatus int

	// RetryAfter is the Retry-After header value accompanying 503s.
	// Default: 5 seconds.
	RetryAfter time.Duration

	// EnableReap opts into calling runtime.GC() at StageReap. Default:
	// false. Forced GC can hurt tail latency; enable only when you
	// can measure the effect.
	EnableReap bool

	// ReapAggressiveness: 1=GC hint, 2=GC + encourage pool drain.
	// Default: 1.
	ReapAggressiveness int

	// Skip defines a function to skip this middleware for certain
	// requests (bypasses all stage logic).
	Skip func(*celeris.Context) bool

	// SkipPaths lists paths to skip entirely.
	SkipPaths []string

	// StopContext, when cancelled, stops the background sampling
	// goroutine. Default: context.Background (never cancels).
	StopContext context.Context
}

func defaultThresholds() Thresholds {
	return Thresholds{
		Expand:       0.70,
		Reap:         0.80,
		Reorder:      0.85,
		Backpressure: 0.90,
		Reject:       0.95,
		Hysteresis:   0.05,
	}
}

func applyDefaults(cfg Config) Config {
	dt := defaultThresholds()
	if cfg.Thresholds.Expand == 0 {
		cfg.Thresholds.Expand = dt.Expand
	}
	if cfg.Thresholds.Reap == 0 {
		cfg.Thresholds.Reap = dt.Reap
	}
	if cfg.Thresholds.Reorder == 0 {
		cfg.Thresholds.Reorder = dt.Reorder
	}
	if cfg.Thresholds.Backpressure == 0 {
		cfg.Thresholds.Backpressure = dt.Backpressure
	}
	if cfg.Thresholds.Reject == 0 {
		cfg.Thresholds.Reject = dt.Reject
	}
	if cfg.Thresholds.Hysteresis == 0 {
		cfg.Thresholds.Hysteresis = dt.Hysteresis
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.BackpressureDelay <= 0 {
		cfg.BackpressureDelay = 50 * time.Millisecond
	}
	if cfg.BackpressureStatus == 0 {
		cfg.BackpressureStatus = 503
	}
	if cfg.RejectStatus == 0 {
		cfg.RejectStatus = 503
	}
	if cfg.RetryAfter <= 0 {
		cfg.RetryAfter = 5 * time.Second
	}
	if cfg.ReapAggressiveness == 0 {
		cfg.ReapAggressiveness = 1
	}
	return cfg
}
