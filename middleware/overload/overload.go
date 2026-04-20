// Package overload is a 5-stage CPU degradation ladder driven by
// [observe.Collector].
//
// A background polling goroutine samples
// collector.Snapshot().CPUUtilization every [Config.PollInterval]
// and transitions the stage atomically. Hot-path handlers read the
// stage via a single atomic load — no locks, no map lookups — so
// the Normal path adds only a few nanoseconds of overhead.
//
// Stages (thresholds configurable, hysteresis applied to downward
// transitions):
//
//	Normal       — pass through unchanged
//	Expand       — signal best-effort worker widening; pass through
//	Reap         — opt-in runtime.GC() then pass through
//	Reorder      — low-priority requests return 503; others pass
//	Backpressure — low-priority requests sleep BackpressureDelay; others
//	               return 503; exempt pass through
//	Reject       — all non-exempt requests return 503 + Retry-After
//
// Priority is application-defined via [Config.PriorityFunc]. Without it,
// Reorder passes everything and Backpressure delays everything.
package overload

import (
	"context"
	"math"
	"runtime"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris"
)

// Controller exposes runtime introspection and control of a running
// overload middleware instance.
type Controller struct {
	stage        *atomic.Int32
	inFlight     *atomic.Int32
	latencyEMAns *atomic.Uint64
	cpuSample    *atomic.Uint64 // float64 bits of last CPU sample (0..1)
	stopped      chan struct{}
	cancel       context.CancelFunc
}

// Stage returns the current stage.
func (c *Controller) Stage() Stage {
	return Stage(c.stage.Load())
}

// InFlight returns the current in-flight request count observed by the
// middleware. Not affected by SkipPaths / ExemptPaths (they never touch
// the counter).
func (c *Controller) InFlight() int32 {
	return c.inFlight.Load()
}

// LatencyEMA returns the current smoothed (EMA) tail latency across
// observed non-exempt requests. Zero if no requests have completed yet.
func (c *Controller) LatencyEMA() time.Duration {
	return time.Duration(c.latencyEMAns.Load())
}

// CPUSample returns the CPU utilization (0..1) from the last completed
// poll tick. Returns -1 if no sample has been taken yet.
func (c *Controller) CPUSample() float64 {
	bits := c.cpuSample.Load()
	if bits == 0 {
		return -1
	}
	return math.Float64frombits(bits)
}

// Stop cancels the background sampling goroutine. Safe to call
// multiple times. After Stop the stage stays frozen at whatever value
// it last observed.
func (c *Controller) Stop() {
	if c.cancel != nil {
		c.cancel()
	}
	if c.stopped != nil {
		<-c.stopped
	}
}

// New returns an overload middleware that uses the configured
// [Config.CollectorProvider] to sample CPU. A background goroutine is
// started; cancel it via [Config.StopContext] or use
// [NewWithController] for explicit control.
func New(config ...Config) celeris.HandlerFunc {
	mw, _ := NewWithController(config...)
	return mw
}

// NewWithController is like [New] but also returns a [Controller]
// for querying the current stage and stopping the poll goroutine.
func NewWithController(config ...Config) (celeris.HandlerFunc, *Controller) {
	cfg := Config{}
	if len(config) > 0 {
		cfg = config[0]
	}
	if cfg.CollectorProvider == nil {
		panic("overload: Config.CollectorProvider is required")
	}
	cfg = applyDefaults(cfg)

	var skip celeris.SkipHelper
	skip.Init(cfg.SkipPaths, cfg.Skip)

	exempt := make(map[string]struct{}, len(cfg.ExemptPaths))
	for _, p := range cfg.ExemptPaths {
		exempt[p] = struct{}{}
	}

	stage := &atomic.Int32{}
	inFlight := &atomic.Int32{}
	latencyEMAns := &atomic.Uint64{}
	cpuSample := &atomic.Uint64{}

	parent := cfg.StopContext
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	stopped := make(chan struct{})
	go run(ctx, cfg, stage, inFlight, latencyEMAns, cpuSample, stopped)

	ctrl := &Controller{
		stage:        stage,
		inFlight:     inFlight,
		latencyEMAns: latencyEMAns,
		cpuSample:    cpuSample,
		stopped:      stopped,
		cancel:       cancel,
	}

	priorityFn := cfg.PriorityFunc
	priorityThreshold := cfg.PriorityThreshold
	backpressureStatus := cfg.BackpressureStatus
	backpressureDelay := cfg.BackpressureDelay
	rejectStatus := cfg.RejectStatus
	retryAfter := strconv.FormatInt(int64(cfg.RetryAfter/time.Second), 10)
	emaAlpha := cfg.LatencyEMAAlpha
	// Depth thresholds snapshot (read-only after New).
	depth := cfg.DepthThresholds

	return func(c *celeris.Context) error {
		if skip.ShouldSkip(c) {
			return c.Next()
		}
		if _, ok := exempt[c.Path()]; ok {
			return c.Next()
		}
		if cfg.ExemptFunc != nil && cfg.ExemptFunc(c) {
			return c.Next()
		}

		// Fold depth into the effective stage — the depth signal can
		// escalate beyond (but not below) the poll-driven CPU stage.
		// Zero-valued thresholds are skipped so the hot path stays
		// branch-predictor-friendly when the user leaves depth off.
		effective := Stage(stage.Load())
		if depth.Reject > 0 || depth.Backpressure > 0 || depth.Reorder > 0 || depth.Reap > 0 || depth.Expand > 0 {
			n := inFlight.Load()
			if depth.Reject > 0 && n >= depth.Reject && effective < StageReject {
				effective = StageReject
			} else if depth.Backpressure > 0 && n >= depth.Backpressure && effective < StageBackpressure {
				effective = StageBackpressure
			} else if depth.Reorder > 0 && n >= depth.Reorder && effective < StageReorder {
				effective = StageReorder
			} else if depth.Reap > 0 && n >= depth.Reap && effective < StageReap {
				effective = StageReap
			} else if depth.Expand > 0 && n >= depth.Expand && effective < StageExpand {
				effective = StageExpand
			}
		}

		// Apply the effective stage's action before letting the request
		// into the critical section (otherwise rejections would count
		// against in-flight and skew depth readings).
		switch effective {
		case StageReorder:
			if priorityFn != nil && priorityFn(c) < priorityThreshold {
				c.SetHeader("retry-after", retryAfter)
				return c.AbortWithStatus(rejectStatus)
			}
		case StageBackpressure:
			if priorityFn != nil && priorityFn(c) < priorityThreshold {
				c.SetHeader("retry-after", retryAfter)
				return c.AbortWithStatus(backpressureStatus)
			}
		case StageReject:
			c.SetHeader("retry-after", retryAfter)
			return c.AbortWithStatus(rejectStatus)
		}

		// Passing through: record in-flight + latency around Next().
		inFlight.Add(1)
		if effective == StageBackpressure {
			time.Sleep(backpressureDelay)
		}
		start := time.Now()
		err := c.Next()
		elapsedNs := uint64(time.Since(start).Nanoseconds())
		inFlight.Add(-1)
		// EMA update: ema = ema*(1-α) + sample*α. Atomic CAS loop so
		// concurrent writers don't lose updates. One contended retry on
		// burst; noise on steady-state.
		for {
			prev := latencyEMAns.Load()
			var next uint64
			if prev == 0 {
				next = elapsedNs
			} else {
				next = uint64(float64(prev)*(1-emaAlpha) + float64(elapsedNs)*emaAlpha)
			}
			if latencyEMAns.CompareAndSwap(prev, next) {
				break
			}
		}
		return err
	}, ctrl
}

func run(ctx context.Context, cfg Config,
	stage *atomic.Int32, inFlight *atomic.Int32,
	latencyEMAns *atomic.Uint64, cpuSample *atomic.Uint64,
	stopped chan struct{},
) {
	defer close(stopped)
	t := time.NewTicker(cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			col := cfg.CollectorProvider()
			if col == nil {
				continue
			}
			cpu := col.Snapshot().CPUUtilization
			if cpu < 0 {
				continue
			}
			cpuSample.Store(math.Float64bits(cpu))
			cpuStage := computeStage(Stage(stage.Load()), cpu, cfg.Thresholds)
			// Fold latency into the stage decision: whichever signal
			// produces the higher stage wins. Latency-driven escalation
			// doesn't need hysteresis because the EMA already smooths
			// the signal; it naturally decays as latencies recover.
			latencyStage := latencyToStage(time.Duration(latencyEMAns.Load()), cfg.LatencyThresholds)
			newStage := cpuStage
			if latencyStage > newStage {
				newStage = latencyStage
			}
			stage.Store(int32(newStage))
			if newStage == StageReap && cfg.EnableReap {
				if cfg.ReapAggressiveness >= 1 {
					runtime.GC()
				}
			}
			_ = inFlight // surfaced through Controller; not needed in poll
		}
	}
}

// latencyToStage maps a smoothed latency to its stage. Returns
// StageNormal when all thresholds are zero (latency signal disabled).
func latencyToStage(ema time.Duration, th LatencyThresholds) Stage {
	switch {
	case th.Reject > 0 && ema >= th.Reject:
		return StageReject
	case th.Backpressure > 0 && ema >= th.Backpressure:
		return StageBackpressure
	case th.Reorder > 0 && ema >= th.Reorder:
		return StageReorder
	case th.Reap > 0 && ema >= th.Reap:
		return StageReap
	case th.Expand > 0 && ema >= th.Expand:
		return StageExpand
	}
	return StageNormal
}

// computeStage maps CPU utilization to a stage using hysteresis on
// downward transitions. The current stage modulates the "fall" threshold
// for each step.
func computeStage(current Stage, cpu float64, th Thresholds) Stage {
	up := cpuStage(cpu, th)
	if up >= current {
		return up
	}
	// Downward: require CPU below (threshold-for-current - hysteresis).
	var lowerBound float64
	switch current {
	case StageExpand:
		lowerBound = th.Expand - th.Hysteresis
	case StageReap:
		lowerBound = th.Reap - th.Hysteresis
	case StageReorder:
		lowerBound = th.Reorder - th.Hysteresis
	case StageBackpressure:
		lowerBound = th.Backpressure - th.Hysteresis
	case StageReject:
		lowerBound = th.Reject - th.Hysteresis
	default:
		return up
	}
	if cpu < lowerBound {
		return up
	}
	return current
}

// cpuStage returns the stage that CPU maps to without considering the
// current stage (the upward direction).
func cpuStage(cpu float64, th Thresholds) Stage {
	switch {
	case cpu >= th.Reject:
		return StageReject
	case cpu >= th.Backpressure:
		return StageBackpressure
	case cpu >= th.Reorder:
		return StageReorder
	case cpu >= th.Reap:
		return StageReap
	case cpu >= th.Expand:
		return StageExpand
	default:
		return StageNormal
	}
}
