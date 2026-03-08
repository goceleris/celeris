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
	standbySnap := c.sampler.Sample(standby)

	activeScore := computeScore(activeSnap, c.weights)
	standbyScore := computeScore(standbySnap, c.weights)

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
