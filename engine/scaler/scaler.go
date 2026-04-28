//go:build linux

// Package scaler implements the dynamic worker scaler shared by the
// iouring, epoll, and adaptive engines. The scaler tracks the active
// connection count and adjusts how many workers participate in the
// SO_REUSEPORT group so connections-per-active-worker stays near the
// configured target — this keeps CQE/event batching density in the
// sweet spot. See PR #257 for the full rationale and benchmark data.
//
// Engines plug in via the [Source] interface. Per-engine sources
// (iouring, epoll) report Generation()=0 always; the adaptive engine's
// source increments it on each engine switch so the scaler can
// re-baseline the new active engine's worker pause state.
package scaler

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/goceleris/celeris/resource"
)

// Source is the engine-side interface the scaler uses to read load and
// adjust worker activation. Implementations must be safe for concurrent
// use; the scaler calls these from its own goroutine.
type Source interface {
	// NumWorkers returns the total worker pool size (= max active count).
	// Must be stable across the lifetime of a scaler run.
	NumWorkers() int
	// ActiveConns returns the current active connection count. Read every tick.
	ActiveConns() int64
	// PauseWorker deactivates worker i. Idempotent.
	PauseWorker(i int)
	// ResumeWorker reactivates worker i. Idempotent.
	ResumeWorker(i int)
	// Generation returns a counter that increments whenever the
	// underlying engine identity changes (e.g., adaptive engine switch).
	// Per-engine sources return 0 always. The scaler uses this to detect
	// switches and re-baseline the active count on the new engine.
	Generation() uint64
	// Logger returns the slog.Logger used for scaler diagnostics. May
	// return nil to suppress log output.
	Logger() *slog.Logger
}

// Config holds the resolved scaler parameters. All fields are mutable
// before Run starts; the loop reads them once at startup. Use
// [Resolve] to derive a Config from a resource.Config (handles both
// the typed WorkerScaling field and the env-var fallback).
type Config struct {
	// Enabled gates the entire scaler. Resolve sets this to true when
	// either the typed config or the CELERIS_DYN_WORKERS env is set.
	Enabled bool
	// StartHigh seeds the scaler at NumWorkers active and scales down
	// on idle. Default true (data-validated; preserves SO_REUSEPORT
	// distribution at startup, +34-78% over start-low on ramp scenarios).
	StartHigh bool
	// MinActive is the floor on active worker count.
	MinActive int
	// TargetConnsPerWorker is the scaling target. desired = ceil(conns/Target).
	TargetConnsPerWorker int
	// Interval is the tick cadence. Default 250ms.
	Interval time.Duration
	// ScaleUpStep is the max workers added per tick (burst limit).
	ScaleUpStep int
	// ScaleDownStep is the max workers removed per tick.
	ScaleDownStep int
	// ScaleDownHysteresis: scale-down requires desired ≤ active - hyst - 1.
	ScaleDownHysteresis int
	// ScaleDownIdleTicks: consecutive sub-threshold ticks needed to scale down.
	ScaleDownIdleTicks int
	// Trace logs every scaler decision when true.
	Trace bool
}

// Resolve produces a Config from the engine's resource.Config. Typed
// cfg.WorkerScaling takes precedence; env vars are the legacy fallback.
// Returns Enabled=false when neither configures the scaler.
func Resolve(cfg resource.Config, numWorkers int) Config {
	if cfg.WorkerScaling != nil {
		return fromTyped(cfg.WorkerScaling, numWorkers)
	}
	return fromEnv(numWorkers)
}

func fromTyped(c *resource.WorkerScalingConfig, numWorkers int) Config {
	minActive := c.MinActive
	if minActive == 0 {
		minActive = numWorkers / 2
		if minActive < 2 {
			minActive = 2
		}
	}
	if minActive > numWorkers {
		minActive = numWorkers
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
	return Config{
		Enabled:              true,
		StartHigh:            c.Strategy != resource.ScalingStrategyStartLow,
		MinActive:            minActive,
		TargetConnsPerWorker: target,
		Interval:             interval,
		ScaleUpStep:          upStep,
		ScaleDownStep:        downStep,
		ScaleDownHysteresis:  hyst,
		ScaleDownIdleTicks:   idleTicks,
		Trace:                c.Trace,
	}
}

func fromEnv(numWorkers int) Config {
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
	return Config{
		Enabled: enabled,
		// Defaults to true (data-validated; matches the typed-config
		// Strategy=ScalingStrategyStartHigh default). start-low pauses
		// workers BEFORE the engine has finished settling its listen
		// FDs in the SO_REUSEPORT group, which races against incoming
		// H2 prior-knowledge dials and produces RST mid-handshake (seen
		// in the v1.4.1 strict-matrix run on get-json-64k-h2/adaptive
		// before this change). Users who explicitly want start-low can
		// set CELERIS_DYN_START_HIGH=0.
		StartHigh:            os.Getenv("CELERIS_DYN_START_HIGH") != "0",
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

// Run executes the scaler loop until ctx is canceled. Reads load from
// src.ActiveConns(), pauses/resumes workers via src.PauseWorker /
// src.ResumeWorker, and re-baselines on Generation changes.
//
// Caller must check cfg.Enabled before invoking Run; Run does not
// short-circuit when disabled.
func Run(ctx context.Context, src Source, cfg Config) {
	totalWorkers := src.NumWorkers()
	var active int
	if cfg.StartHigh {
		active = totalWorkers
	} else {
		active = cfg.MinActive
		for i := cfg.MinActive; i < totalWorkers; i++ {
			src.PauseWorker(i)
		}
	}

	if log := src.Logger(); log != nil {
		log.Info("dynamic worker scaler started",
			"min_active", cfg.MinActive,
			"max", totalWorkers,
			"target_conns_per_worker", cfg.TargetConnsPerWorker,
			"interval_ms", int(cfg.Interval/time.Millisecond),
			"start_high", cfg.StartHigh,
			"up_step", cfg.ScaleUpStep,
			"down_step", cfg.ScaleDownStep,
			"down_hyst", cfg.ScaleDownHysteresis,
			"down_idle_ticks", cfg.ScaleDownIdleTicks)
	}

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	idleTicks := 0
	lastGen := src.Generation()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if gen := src.Generation(); gen != lastGen {
				// Engine switched (only meaningful for adaptive). Re-baseline
				// the new engine to the current `active` count: 0..active-1
				// resumed, active..total-1 paused. This is idempotent against
				// the new engine's existing per-worker state.
				for i := 0; i < totalWorkers; i++ {
					if i < active {
						src.ResumeWorker(i)
					} else {
						src.PauseWorker(i)
					}
				}
				lastGen = gen
				idleTicks = 0
				continue
			}
			active, idleTicks = tick(src, cfg, active, idleTicks)
		}
	}
}

// tick is a single iteration of the scaler decision loop. Split out
// for unit-testing the algorithm without spinning up a real engine.
func tick(src Source, cfg Config, active, idleTicks int) (int, int) {
	totalWorkers := src.NumWorkers()
	conns := src.ActiveConns()
	desired := int((conns + int64(cfg.TargetConnsPerWorker) - 1) / int64(cfg.TargetConnsPerWorker))
	if desired < cfg.MinActive {
		desired = cfg.MinActive
	}
	if desired > totalWorkers {
		desired = totalWorkers
	}
	if cfg.Trace {
		if log := src.Logger(); log != nil {
			log.Info("scaler tick",
				"conns", conns, "desired", desired, "active", active, "idle_ticks", idleTicks)
		}
	}

	switch {
	case desired > active:
		step := desired - active
		if step > cfg.ScaleUpStep {
			step = cfg.ScaleUpStep
		}
		for n := 0; n < step; n++ {
			src.ResumeWorker(active)
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
				src.PauseWorker(active)
			}
			idleTicks = 0
		}
	default:
		idleTicks = 0
	}
	return active, idleTicks
}
