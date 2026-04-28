//go:build linux

package epoll

import (
	"context"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris/resource"
)

// ScalerConfig parameters control the dynamic loop activation. Mirrored
// from engine/iouring/scaler.go — same env var contract so a process
// running adaptive can use one set of knobs across both engines.
type ScalerConfig struct {
	Enabled              bool
	StartHigh            bool
	MinActive            int
	TargetConnsPerWorker int
	Interval             time.Duration
	ScaleUpStep          int
	ScaleDownStep        int
	ScaleDownHysteresis  int
	ScaleDownIdleTicks   int
	Trace                bool
}

// resolveScalerConfig prefers cfg.WorkerScaling when present, otherwise
// falls back to the env-var contract. Used by Engine.Listen.
func resolveScalerConfig(cfg resource.Config, numLoops int) ScalerConfig {
	if cfg.WorkerScaling != nil {
		return scalerFromTyped(cfg.WorkerScaling, numLoops)
	}
	return envScalerConfig(numLoops)
}

func scalerFromTyped(c *resource.WorkerScalingConfig, numLoops int) ScalerConfig {
	min := c.MinActive
	if min == 0 {
		min = numLoops / 2
		if min < 2 {
			min = 2
		}
	}
	if min > numLoops {
		min = numLoops
	}
	target := c.TargetConnsPerWorker
	if target == 0 {
		target = 20
	}
	interval := c.Interval
	if interval == 0 {
		interval = 250 * time.Millisecond
	}
	upStep := c.ScaleUpStep
	if upStep == 0 {
		upStep = 2
	}
	downStep := c.ScaleDownStep
	if downStep == 0 {
		downStep = 1
	}
	hyst := c.ScaleDownHysteresis
	if hyst == 0 {
		hyst = 1
	}
	idleTicks := c.ScaleDownIdleTicks
	if idleTicks == 0 {
		idleTicks = 4
	}
	startHigh := c.Strategy != resource.ScalingStrategyStartLow
	return ScalerConfig{
		Enabled:              true,
		StartHigh:            startHigh,
		MinActive:            min,
		TargetConnsPerWorker: target,
		Interval:             interval,
		ScaleUpStep:          upStep,
		ScaleDownStep:        downStep,
		ScaleDownHysteresis:  hyst,
		ScaleDownIdleTicks:   idleTicks,
		Trace:                c.Trace,
	}
}

func envScalerConfig(numLoops int) ScalerConfig {
	getInt := func(k string, def int) int {
		if v := os.Getenv(k); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				return n
			}
		}
		return def
	}
	enabled := getInt("CELERIS_DYN_WORKERS", 0) != 0
	minActive := getInt("CELERIS_DYN_MIN", numLoops/2)
	if minActive < 1 {
		minActive = 1
	}
	if minActive > numLoops {
		minActive = numLoops
	}
	return ScalerConfig{
		Enabled:              enabled,
		MinActive:            minActive,
		TargetConnsPerWorker: getInt("CELERIS_DYN_TARGET", 20),
		Interval:             time.Duration(getInt("CELERIS_DYN_INTERVAL", 250)) * time.Millisecond,
		ScaleUpStep:          getInt("CELERIS_DYN_UPSTEP", 2),
		ScaleDownStep:        getInt("CELERIS_DYN_DOWNSTEP", 1),
		ScaleDownHysteresis:  getInt("CELERIS_DYN_DOWNHYST", 1),
		ScaleDownIdleTicks:   getInt("CELERIS_DYN_DOWNIDLE", 4),
	}
}

// runScaler owns the active loop count and uses Engine.PauseWorker/ResumeWorker
// to adjust which loops participate in the SO_REUSEPORT group. Two starting
// strategies, controlled by CELERIS_DYN_START_HIGH:
//   - 0: start at MinActive, scale up reactively (good for conn-churn traffic)
//   - 1: start at full pool, scale down once load is low (good for sticky
//     keep-alive workloads where SO_REUSEPORT can only distribute at SYN time)
func (e *Engine) runScaler(ctx context.Context, cfg ScalerConfig, totalLoops int, activeConns *atomic.Int64) {
	startHigh := cfg.StartHigh
	if !startHigh && e.cfg.WorkerScaling == nil {
		startHigh = os.Getenv("CELERIS_DYN_START_HIGH") == "1"
	}
	var active int
	if startHigh {
		active = totalLoops
	} else {
		active = cfg.MinActive
		for i := cfg.MinActive; i < totalLoops; i++ {
			e.PauseWorker(i)
		}
	}
	idleTicks := 0

	if e.cfg.Logger != nil {
		e.cfg.Logger.Info("dynamic loop scaler started",
			"min_active", cfg.MinActive,
			"max", totalLoops,
			"target_conns_per_worker", cfg.TargetConnsPerWorker,
			"interval_ms", int(cfg.Interval/time.Millisecond),
			"start_high", startHigh,
			"up_step", cfg.ScaleUpStep,
			"down_step", cfg.ScaleDownStep,
			"down_hyst", cfg.ScaleDownHysteresis,
			"down_idle_ticks", cfg.ScaleDownIdleTicks)
	}

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	traceTick := os.Getenv("CELERIS_DYN_TRACE") == "1"
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			conns := activeConns.Load()
			desired := int((conns + int64(cfg.TargetConnsPerWorker) - 1) / int64(cfg.TargetConnsPerWorker))
			if desired < cfg.MinActive {
				desired = cfg.MinActive
			}
			if desired > totalLoops {
				desired = totalLoops
			}
			if traceTick && e.cfg.Logger != nil {
				e.cfg.Logger.Info("scaler tick",
					"conns", conns, "desired", desired, "active", active, "idle_ticks", idleTicks)
			}

			switch {
			case desired > active:
				step := desired - active
				if step > cfg.ScaleUpStep {
					step = cfg.ScaleUpStep
				}
				for n := 0; n < step; n++ {
					e.ResumeWorker(active)
					active++
				}
				idleTicks = 0
			case desired <= active-cfg.ScaleDownHysteresis-1:
				idleTicks++
				if idleTicks >= cfg.ScaleDownIdleTicks {
					step := active - desired
					if step > cfg.ScaleDownStep {
						step = cfg.ScaleDownStep
					}
					for n := 0; n < step && active > cfg.MinActive; n++ {
						active--
						e.PauseWorker(active)
					}
					idleTicks = 0
				}
			default:
				idleTicks = 0
			}
		}
	}
}

// PauseWorker deactivates loop i. The loop drains in-flight conns and goes
// SUSPENDED. Asynchronous; returns immediately.
func (e *Engine) PauseWorker(i int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if i < 0 || i >= len(e.loops) {
		return
	}
	e.loops[i].inactive.Store(true)
}

// ResumeWorker reactivates loop i. Wakes the loop from SUSPENDED if it was
// already idle.
func (e *Engine) ResumeWorker(i int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if i < 0 || i >= len(e.loops) {
		return
	}
	l := e.loops[i]
	l.inactive.Store(false)
	l.listenFDClosed.Store(false)
	l.wakeMu.Lock()
	if l.suspended.Load() {
		close(l.wake)
		l.wake = make(chan struct{})
		l.suspended.Store(false)
	}
	l.wakeMu.Unlock()
}
