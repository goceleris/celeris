//go:build linux

package adaptive

import (
	"context"
	"os"
	"strconv"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/resource"
)

// scalerConfig parameters control the higher-level adaptive scaler.
// Same env-var contract as the per-engine scaler — when an adaptive engine
// is in use, only this one runs (sub-engine builtins are suppressed via
// resource.Config.SkipBuiltinScaler). Algorithm details and tuning
// guidance are documented in engine/iouring/scaler.go.
type scalerConfig struct {
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
// falls back to the env-var contract.
func resolveScalerConfig(cfg resource.Config, numWorkers int) scalerConfig {
	if cfg.WorkerScaling != nil {
		return scalerFromTyped(cfg.WorkerScaling, numWorkers)
	}
	return envScalerConfig(numWorkers)
}

func scalerFromTyped(c *resource.WorkerScalingConfig, numWorkers int) scalerConfig {
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
	startHigh := c.Strategy != resource.ScalingStrategyStartLow
	return scalerConfig{
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

func envScalerConfig(numWorkers int) scalerConfig {
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
	return scalerConfig{
		Enabled:              enabled,
		StartHigh:            os.Getenv("CELERIS_DYN_START_HIGH") == "1",
		MinActive:            minActive,
		TargetConnsPerWorker: getInt("CELERIS_DYN_TARGET", 20),
		Interval:             time.Duration(getInt("CELERIS_DYN_INTERVAL", 250)) * time.Millisecond,
		ScaleUpStep:          getInt("CELERIS_DYN_UPSTEP", 2),
		ScaleDownStep:        getInt("CELERIS_DYN_DOWNSTEP", 1),
		ScaleDownHysteresis:  getInt("CELERIS_DYN_DOWNHYST", 1),
		ScaleDownIdleTicks:   getInt("CELERIS_DYN_DOWNIDLE", 4),
		Trace:                os.Getenv("CELERIS_DYN_TRACE") == "1",
	}
}

// runScaler is the higher-level dynamic worker scaler used when the adaptive
// engine is in play. It reads activeConns from whichever sub-engine is
// currently active and pauses/resumes that engine's workers via the
// engine.WorkerScaler interface. On engine switch, the new active engine is
// re-baselined: any extra workers above the current `active` count are
// paused immediately so the active count is preserved across the switch.
//
// This is the only scaler that runs when the adaptive engine is used; the
// iouring + epoll built-ins are suppressed via Config.SkipBuiltinScaler.
func (e *Engine) runScaler(ctx context.Context, cfg scalerConfig) {
	// Both sub-engines have the same NumWorkers (full Resources cfg).
	// We track active count from the perspective of the active engine.
	primaryScaler, _ := e.primary.(engine.WorkerScaler)
	secondaryScaler, _ := e.secondary.(engine.WorkerScaler)
	if primaryScaler == nil || secondaryScaler == nil {
		// Defensive: both sub-engines must implement WorkerScaler. If one
		// does not, the scaler is silently disabled (better than panicking
		// in a perf-tuning code path).
		if e.logger != nil {
			e.logger.Warn("adaptive scaler disabled: sub-engine does not implement WorkerScaler")
		}
		return
	}
	totalWorkers := primaryScaler.NumWorkers()
	if n := secondaryScaler.NumWorkers(); n != totalWorkers {
		// Both sub-engines use the same Resources config so this should
		// never happen, but guard against it anyway by taking the smaller
		// of the two pools as the cap.
		if n < totalWorkers {
			totalWorkers = n
		}
	}

	// initialActive is the starting active count according to the strategy.
	// We seed BOTH sub-engines so a switch never lands on an under-sized
	// engine; the standby's per-worker pause flags are also flipped now,
	// even though all of its workers are already engine-paused via
	// PauseAccept (idempotent — flipping inactive=true on a worker that
	// is already engine-paused is a no-op for the worker).
	var active int
	if cfg.StartHigh {
		active = totalWorkers
	} else {
		active = cfg.MinActive
		for i := cfg.MinActive; i < totalWorkers; i++ {
			primaryScaler.PauseWorker(i)
			secondaryScaler.PauseWorker(i)
		}
	}

	if e.logger != nil {
		e.logger.Info("adaptive dynamic worker scaler started",
			"min_active", cfg.MinActive,
			"max", totalWorkers,
			"target_conns_per_worker", cfg.TargetConnsPerWorker,
			"interval_ms", int(cfg.Interval/time.Millisecond),
			"start_high", cfg.StartHigh)
	}

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	var (
		idleTicks    int
		lastActivePtr engine.Engine // tracks engine switches
	)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			active = e.scalerTick(ctx, cfg, primaryScaler, secondaryScaler,
				totalWorkers, active, &idleTicks, &lastActivePtr)
		}
	}
}

// scalerTick runs one iteration of the adaptive scaler's decision loop.
// Split out for testability and to keep runScaler readable.
func (e *Engine) scalerTick(
	_ context.Context,
	cfg scalerConfig,
	primaryScaler, secondaryScaler engine.WorkerScaler,
	totalWorkers, active int,
	idleTicks *int,
	lastActivePtr *engine.Engine,
) int {
	activeEng := *e.active.Load()
	activeScaler := primaryScaler
	if activeEng == e.secondary {
		activeScaler = secondaryScaler
	}

	// Engine switch: re-baseline the new active engine to the current
	// active worker count. The new active engine just got its listen FDs
	// recreated by adaptive's performSwitch + ResumeAccept; its workers
	// each saw `inactive=true` for whatever the scaler set previously,
	// so the right thing is to push our current `active` onto the new
	// engine and PauseWorker the rest. We don't re-run the activeConns
	// computation immediately — the next tick will do that.
	if *lastActivePtr != activeEng {
		for i := 0; i < totalWorkers; i++ {
			if i < active {
				activeScaler.ResumeWorker(i)
			} else {
				activeScaler.PauseWorker(i)
			}
		}
		*lastActivePtr = activeEng
		*idleTicks = 0
		return active
	}

	conns := activeEng.Metrics().ActiveConnections
	desired := int((conns + int64(cfg.TargetConnsPerWorker) - 1) / int64(cfg.TargetConnsPerWorker))
	if desired < cfg.MinActive {
		desired = cfg.MinActive
	}
	if desired > totalWorkers {
		desired = totalWorkers
	}
	if cfg.Trace && e.logger != nil {
		e.logger.Info("adaptive scaler tick",
			"active_eng", activeEng.Type().String(),
			"conns", conns, "desired", desired, "active", active,
			"idle_ticks", *idleTicks)
	}

	switch {
	case desired > active:
		step := desired - active
		if step > cfg.ScaleUpStep {
			step = cfg.ScaleUpStep
		}
		for n := 0; n < step; n++ {
			activeScaler.ResumeWorker(active)
			active++
		}
		*idleTicks = 0
	case desired <= active-cfg.ScaleDownHysteresis-1:
		*idleTicks++
		if *idleTicks >= cfg.ScaleDownIdleTicks {
			step := active - desired
			if step > cfg.ScaleDownStep {
				step = cfg.ScaleDownStep
			}
			for n := 0; n < step && active > cfg.MinActive; n++ {
				active--
				activeScaler.PauseWorker(active)
			}
			*idleTicks = 0
		}
	default:
		*idleTicks = 0
	}

	return active
}
