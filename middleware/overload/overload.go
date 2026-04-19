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
	"runtime"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris"
)

// Controller exposes runtime introspection and control of a running
// overload middleware instance.
type Controller struct {
	stage   *atomic.Int32
	stopped chan struct{}
	cancel  context.CancelFunc
}

// Stage returns the current stage.
func (c *Controller) Stage() Stage {
	return Stage(c.stage.Load())
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

	parent := cfg.StopContext
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	stopped := make(chan struct{})
	go run(ctx, cfg, stage, stopped)

	ctrl := &Controller{stage: stage, stopped: stopped, cancel: cancel}

	priorityFn := cfg.PriorityFunc
	priorityThreshold := cfg.PriorityThreshold
	backpressureStatus := cfg.BackpressureStatus
	backpressureDelay := cfg.BackpressureDelay
	rejectStatus := cfg.RejectStatus
	retryAfter := strconv.FormatInt(int64(cfg.RetryAfter/time.Second), 10)

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
		switch Stage(stage.Load()) {
		case StageNormal, StageExpand, StageReap:
			return c.Next()
		case StageReorder:
			if priorityFn == nil || priorityFn(c) >= priorityThreshold {
				return c.Next()
			}
			c.SetHeader("retry-after", retryAfter)
			return c.AbortWithStatus(rejectStatus)
		case StageBackpressure:
			if priorityFn == nil || priorityFn(c) >= priorityThreshold {
				time.Sleep(backpressureDelay)
				return c.Next()
			}
			c.SetHeader("retry-after", retryAfter)
			return c.AbortWithStatus(backpressureStatus)
		case StageReject:
			c.SetHeader("retry-after", retryAfter)
			return c.AbortWithStatus(rejectStatus)
		}
		return c.Next()
	}, ctrl
}

func run(ctx context.Context, cfg Config, stage *atomic.Int32, stopped chan struct{}) {
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
			newStage := computeStage(Stage(stage.Load()), cpu, cfg.Thresholds)
			stage.Store(int32(newStage))
			if newStage == StageReap && cfg.EnableReap {
				if cfg.ReapAggressiveness >= 1 {
					runtime.GC()
				}
			}
		}
	}
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
