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
	minObserve   time.Duration
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
		minObserve:   10 * time.Second,
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

	if !c.state.lastSwitch.IsZero() && now.Sub(c.state.lastSwitch) < c.minObserve {
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
	activeScore := computeScore(activeSnap, c.weights)

	// Apply io_uring bias: when conditions favor io_uring (256-4096 connections,
	// high CPU), add a bonus to io_uring's score or penalty to epoll's score.
	// This enables proactive switching even when throughput is equivalent.
	bias := ioUringBias(activeSnap)
	if active.Type() == engine.IOUring {
		activeScore *= (1.0 + bias) // Bonus: io_uring already active, reinforce
	} else if bias > 0 {
		activeScore *= (1.0 - bias*0.5) // Penalty: epoll active but conditions favor io_uring
	}

	// Store active's score for historical reference.
	c.state.lastActiveScore[active.Type()] = activeScore
	c.state.lastActiveTime[active.Type()] = now

	// Seed standby with 80% of active if no history exists.
	if _, ok := c.state.lastActiveScore[standby.Type()]; !ok {
		c.state.lastActiveScore[standby.Type()] = activeScore * 0.80
		c.state.lastActiveTime[standby.Type()] = now
	}

	// Use historical score for standby (with 1%/sec decay).
	standbyScore := c.historicalScore(standby.Type(), now)

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
