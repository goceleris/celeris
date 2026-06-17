//go:build linux

package adaptive

import (
	"log/slog"
	"os"
	"time"

	"github.com/goceleris/celeris/engine"
)

// envIOUringBias gates the io_uring workload bias. The bias is now REVERSIBLE
// (celeris#338): it only EXPLORES — boosting the io_uring standby so an
// epoll→io_uring switch is reachable when the workload model favors it — and
// never inflates the active score nor suppresses the epoll standby, so a
// wrongly-explored io_uring always reverts on measurement. That makes it safe
// ON by default (adaptive picks the real high-concurrency winner instead of
// parking on epoll); set CELERIS_ADAPTIVE_IOURING_BIAS=0 to force the
// conservative measurement-only controller.
const envIOUringBias = "CELERIS_ADAPTIVE_IOURING_BIAS"

type controllerState struct {
	activeIsPrimary bool
	lastSwitch      time.Time
	switchTimes     [6]time.Time
	switchIdx       int
	switchCount     int
	locked          bool
	lockUntil       time.Time
	lastActiveScore map[engine.EngineType]float64
	lastActiveTime  map[engine.EngineType]time.Time
}

type controller struct {
	primary   engine.Engine
	secondary engine.Engine
	sampler   TelemetrySampler
	weights   ScoreWeights
	state     controllerState

	evalInterval time.Duration
	cooldown     time.Duration
	threshold    float64
	biasEnabled  bool
	logger       *slog.Logger
}

func newController(primary, secondary engine.Engine, sampler TelemetrySampler, logger *slog.Logger) *controller {
	return &controller{
		primary:      primary,
		secondary:    secondary,
		sampler:      sampler,
		weights:      DefaultWeights(),
		evalInterval: 5 * time.Second,
		cooldown:     30 * time.Second,
		threshold:    0.15,
		biasEnabled:  os.Getenv(envIOUringBias) != "0",
		logger:       logger,
		state: controllerState{
			activeIsPrimary: true,
			lastActiveScore: make(map[engine.EngineType]float64),
			lastActiveTime:  make(map[engine.EngineType]time.Time),
		},
	}
}

// evaluate checks whether a switch is warranted. Returns true if a switch should occur.
func (c *controller) evaluate(now time.Time, frozen bool) bool {
	if frozen {
		return false
	}

	if c.state.locked && now.Before(c.state.lockUntil) {
		return false
	}
	if c.state.locked && !now.Before(c.state.lockUntil) {
		c.state.locked = false
		c.logger.Info("oscillation lock expired")
	}

	if !c.state.lastSwitch.IsZero() && now.Sub(c.state.lastSwitch) < c.cooldown {
		return false
	}

	var active, standby engine.Engine
	if c.state.activeIsPrimary {
		active = c.primary
		standby = c.secondary
	} else {
		active = c.secondary
		standby = c.primary
	}

	activeSnap := c.sampler.Sample(active)
	baselineActiveScore := computeScore(activeSnap, c.weights)

	// io_uring bias: the modeled io_uring advantage for the current workload.
	// Zero outside the empirical sweet spot, AND zero unless explicitly enabled
	// (celeris#341, envIOUringBias). It never reads the standby's real
	// throughput, so off-by-default keeps adaptive from speculatively switching
	// onto a measurably-slower engine.
	bias := ioUringBias(activeSnap, c.biasEnabled)

	// Reversible bias (celeris#338): the ACTIVE score is ALWAYS the pure
	// measurement — never inflated or penalised — so leaving the active engine
	// is decided measured-vs-measured, never blocked by a sticky bias bonus.
	activeScore := baselineActiveScore

	// Record the measured (unbiased) score as history, so a later revert
	// compares real throughput rather than a biased estimate.
	c.state.lastActiveScore[active.Type()] = baselineActiveScore
	c.state.lastActiveTime[active.Type()] = now

	// Seed standby with 80% of active if no history exists. Harmless: 0.8 never
	// clears the switch threshold on its own — only the explore-bias does.
	if _, ok := c.state.lastActiveScore[standby.Type()]; !ok {
		c.state.lastActiveScore[standby.Type()] = baselineActiveScore * 0.80
		c.state.lastActiveTime[standby.Type()] = now
	}

	// Standby estimate. The historical (measured, decayed) score ALWAYS counts —
	// it is what drives a measurement-based revert. The io_uring bias may
	// additionally EXPLORE: it boosts the io_uring standby when the workload
	// model favors it (making an organic epoll→io_uring switch reachable), but
	// it NEVER suppresses the epoll standby — so reverting from a wrongly-explored
	// io_uring is always allowed on measurement. A bad exploration self-corrects
	// the next eval; the oscillation lock bounds any thrash.
	standbyScore := c.historicalScore(standby.Type(), now)
	if modeled := c.biasModeledStandbyScore(standby.Type(), baselineActiveScore, bias); modeled > standbyScore {
		standbyScore = modeled
	}

	if standbyScore > activeScore*(1.0+c.threshold) {
		c.logger.Info("switch recommended",
			"active", active.Type().String(),
			"standby", standby.Type().String(),
			"active_score", activeScore,
			"standby_score", standbyScore,
		)
		return true
	}

	return false
}

// biasModeledStandbyScore models the io_uring standby's score for the CURRENT
// workload from the io_uring bias (celeris#338): when conditions favor io_uring
// it is modeled as bias-better than the active baseline → standby =
// baseline*(1+bias), making an organic epoll→io_uring EXPLORATION reachable
// (the historical-only path could never clear 1+threshold from a cold standby).
//
// It returns 0 for the epoll standby — the bias never models epoll DOWN. That
// asymmetry is the reversibility guarantee: a revert from a wrongly-explored
// io_uring back to epoll is driven purely by epoll's real (historical)
// measurement and is never blocked by the bias.
func (c *controller) biasModeledStandbyScore(standby engine.EngineType, baselineActiveScore, bias float64) float64 {
	if bias <= 0 || standby != engine.IOUring {
		return 0
	}
	return baselineActiveScore * (1.0 + bias)
}

// historicalScore returns the last known score for an engine type, decayed
// at 1% per second since the score was recorded.
func (c *controller) historicalScore(et engine.EngineType, now time.Time) float64 {
	score, ok := c.state.lastActiveScore[et]
	if !ok {
		return 0
	}
	elapsed := now.Sub(c.state.lastActiveTime[et]).Seconds()
	return score * max(0, 1.0-0.01*elapsed)
}

// recordSwitch updates controller state after a switch has been performed.
func (c *controller) recordSwitch(now time.Time) {
	c.state.activeIsPrimary = !c.state.activeIsPrimary
	c.state.lastSwitch = now

	c.state.switchTimes[c.state.switchIdx%len(c.state.switchTimes)] = now
	c.state.switchIdx++
	if c.state.switchCount < len(c.state.switchTimes) {
		c.state.switchCount++
	}

	// Oscillation detection: 3+ switches in 5 minutes → lock for 5 minutes.
	if c.state.switchCount >= 3 {
		oldest := c.state.switchTimes[(c.state.switchIdx-3)%len(c.state.switchTimes)]
		if now.Sub(oldest) < 5*time.Minute {
			c.state.locked = true
			c.state.lockUntil = now.Add(5 * time.Minute)
			c.logger.Warn("oscillation detected, locking switches", "until", c.state.lockUntil)
		}
	}
}
