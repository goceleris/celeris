//go:build linux

package adaptive

import (
	"log/slog"
	"time"

	"github.com/goceleris/celeris/engine"
)

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

	// io_uring bias: the modeled io_uring advantage given the current workload
	// (connection count + CPU pressure). Zero outside the empirical sweet spot.
	bias := ioUringBias(activeSnap)

	// Apply the bias to the ACTIVE score: reinforce io_uring when it is already
	// active (resist leaving it), lightly penalise epoll when conditions favor
	// io_uring (encourage leaving it).
	activeScore := baselineActiveScore
	if active.Type() == engine.IOUring {
		activeScore *= (1.0 + bias) // Bonus: io_uring already active, reinforce
	} else if bias > 0 {
		activeScore *= (1.0 - bias*0.5) // Penalty: epoll active but conditions favor io_uring
	}

	// Store active's (biased) score for historical reference.
	c.state.lastActiveScore[active.Type()] = activeScore
	c.state.lastActiveTime[active.Type()] = now

	// Seed standby with 80% of active if no history exists.
	if _, ok := c.state.lastActiveScore[standby.Type()]; !ok {
		c.state.lastActiveScore[standby.Type()] = activeScore * 0.80
		c.state.lastActiveTime[standby.Type()] = now
	}

	// Standby estimate. Two independent signals, combined by max:
	//
	//  1. Historical: the last directly-observed score for the standby engine,
	//     decayed at 1%/sec. This drives switching when the ACTIVE engine
	//     genuinely degrades below a previously-measured standby.
	//
	//  2. Bias-modeled: the standby score implied by the io_uring bias for the
	//     CURRENT workload, recomputed EACH tick. When io_uring is the standby
	//     and conditions favor it, the standby is modeled as the unbiased active
	//     baseline scaled up by the bias; when epoll is the standby (io_uring
	//     active and favored) it is scaled DOWN so we do not switch back. This
	//     is what makes an organic epoll→io_uring switch reachable — the
	//     historical-only path could never exceed the threshold (max attainable
	//     ratio ~0.70 < 1+threshold).
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

// biasModeledStandbyScore models the standby engine's score for the CURRENT
// workload from the io_uring bias, using the unbiased active baseline as the
// reference point. The sign of the adjustment depends on which engine is the
// standby:
//
//   - io_uring standby: when conditions favor io_uring (bias>0) it is modeled
//     as bias-better than the active baseline → standby = baseline*(1+bias).
//   - epoll standby (io_uring active and favored): epoll is modeled as
//     bias-worse → standby = baseline*(1-bias), so a favorable-for-io_uring
//     workload never recommends switching back to epoll.
//
// Recomputed every tick so the estimate tracks live conditions rather than a
// stale seed.
func (c *controller) biasModeledStandbyScore(standby engine.EngineType, baselineActiveScore, bias float64) float64 {
	if bias <= 0 {
		return baselineActiveScore
	}
	if standby == engine.IOUring {
		return baselineActiveScore * (1.0 + bias)
	}
	return baselineActiveScore * (1.0 - bias)
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
