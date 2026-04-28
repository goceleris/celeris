//go:build linux

package iouring

import (
	"context"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris/resource"
)

// ScalerConfig parameters control the dynamic worker activation loop.
// All fields can be overridden via env vars (see envScalerConfig). The
// default profile is conservative: starts at numCPU/2 active, scales up
// reactively when conns/active_worker exceeds Target, scales down with
// hysteresis to avoid flapping.
type ScalerConfig struct {
	Enabled              bool
	StartHigh            bool          // start at NumWorkers, scale-down on idle (default true via typed config)
	MinActive            int           // floor on active worker count
	TargetConnsPerWorker int           // scale-up threshold; desired = ceil(conns/Target)
	Interval             time.Duration // evaluation cadence
	ScaleUpStep          int           // max workers to wake per tick (burst)
	ScaleDownStep        int           // max workers to pause per tick
	ScaleDownHysteresis  int           // require desired < active - this before scaling down
	ScaleDownIdleTicks   int           // require N consecutive ticks below threshold before scaling down
	Trace                bool          // log every scaler decision
}

// resolveScalerConfig prefers the typed cfg.WorkerScaling when present,
// otherwise falls back to the env-var contract. Used by Engine.Listen.
func resolveScalerConfig(cfg resource.Config, numWorkers int) ScalerConfig {
	if cfg.WorkerScaling != nil {
		return scalerFromTyped(cfg.WorkerScaling, numWorkers)
	}
	return envScalerConfig(numWorkers)
}

func scalerFromTyped(c *resource.WorkerScalingConfig, numWorkers int) ScalerConfig {
	min := c.MinActive
	if min == 0 {
		min = numWorkers / 2
		if min < 2 {
			min = 2
		}
	}
	if min > numWorkers {
		min = numWorkers
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
	// Zero-value Strategy = ScalingStrategyStartHigh, the data-validated
	// default. ScalingStrategyStartLow opts the user into start-low.
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

// envScalerConfig reads the scaler config from env vars. Returns
// Enabled=false when CELERIS_DYN_WORKERS is unset or 0.
//
// Env vars (defaults shown after =):
//
//	CELERIS_DYN_WORKERS=0           on/off (1 to enable)
//	CELERIS_DYN_MIN=numCPU/2        floor
//	CELERIS_DYN_TARGET=20           target conns per active worker
//	CELERIS_DYN_INTERVAL=250        evaluation period in ms
//	CELERIS_DYN_UPSTEP=2            max workers added per tick
//	CELERIS_DYN_DOWNSTEP=1          max workers removed per tick
//	CELERIS_DYN_DOWNHYST=1          desired must be < active - this to scale down
//	CELERIS_DYN_DOWNIDLE=4          consecutive sub-threshold ticks required
func envScalerConfig(numWorkers int) ScalerConfig {
	getInt := func(k string, def int) int {
		if v := os.Getenv(k); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				return n
			}
		}
		return def
	}
	enabled := getInt("CELERIS_DYN_WORKERS", 0) != 0
	minActive := getInt("CELERIS_DYN_MIN", numWorkers/2)
	if minActive < 1 {
		minActive = 1
	}
	if minActive > numWorkers {
		minActive = numWorkers
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

// runScaler is started by Engine.Listen when scaler config is enabled.
// It owns the active count and uses Engine.PauseWorker/ResumeWorker to
// adjust which workers participate in the SO_REUSEPORT group.
//
// Two starting strategies, controlled by CELERIS_DYN_START_HIGH:
//   - 0 (default): start at MinActive, scale up reactively. Good for
//     services with steady ramp where SO_REUSEPORT can distribute new
//     connections across newly-resumed workers (typical of conn-churn
//     traffic).
//   - 1: start at full pool, scale down once load is low. Good for
//     long-lived keep-alive workloads where connections distribute
//     during the initial burst and stay sticky to their worker; scaling
//     up later cannot reshape an existing distribution. Trades startup
//     CPU for steady-state efficiency.
func (e *Engine) runScaler(ctx context.Context, cfg ScalerConfig, totalWorkers int, activeConns *atomic.Int64) {
	// StartHigh: typed config takes precedence; fall back to env var.
	startHigh := cfg.StartHigh
	if !startHigh && e.cfg.WorkerScaling == nil {
		startHigh = os.Getenv("CELERIS_DYN_START_HIGH") == "1"
	}
	var active int
	if startHigh {
		active = totalWorkers
	} else {
		active = cfg.MinActive
		// Initial state: deactivate workers above MinActive.
		for i := cfg.MinActive; i < totalWorkers; i++ {
			e.PauseWorker(i)
		}
	}
	idleTicks := 0

	if e.cfg.Logger != nil {
		e.cfg.Logger.Info("dynamic worker scaler started",
			"min_active", cfg.MinActive,
			"max", totalWorkers,
			"target_conns_per_worker", cfg.TargetConnsPerWorker,
			"interval_ms", int(cfg.Interval/time.Millisecond),
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
			// desired = ceil(conns / target), clamped to [min, totalWorkers]
			desired := int((conns + int64(cfg.TargetConnsPerWorker) - 1) / int64(cfg.TargetConnsPerWorker))
			if desired < cfg.MinActive {
				desired = cfg.MinActive
			}
			if desired > totalWorkers {
				desired = totalWorkers
			}
			if traceTick && e.cfg.Logger != nil {
				e.cfg.Logger.Info("scaler tick",
					"conns", conns, "desired", desired, "active", active, "idle_ticks", idleTicks)
			}

			switch {
			case desired > active:
				// Scale up — burst up to ScaleUpStep per tick.
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

// PauseWorker deactivates worker i. The worker drains in-flight conns
// and goes SUSPENDED. Asynchronous; returns immediately.
func (e *Engine) PauseWorker(i int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if i < 0 || i >= len(e.workers) {
		return
	}
	e.workers[i].inactive.Store(true)
}

// ResumeWorker reactivates worker i. Wakes the worker from SUSPENDED if
// it was already idle.
func (e *Engine) ResumeWorker(i int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if i < 0 || i >= len(e.workers) {
		return
	}
	w := e.workers[i]
	w.inactive.Store(false)
	w.listenFDClosed.Store(false)
	w.wakeMu.Lock()
	if w.suspended.Load() {
		close(w.wake)
		w.wake = make(chan struct{})
		w.suspended.Store(false)
	}
	w.wakeMu.Unlock()
}
