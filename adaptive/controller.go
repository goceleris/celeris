//go:build linux

package adaptive

import (
	"log/slog"
	"time"

	"github.com/goceleris/celeris/engine"
)

// The adaptive engine starts on epoll and switches to io_uring under load
// using a DIRECT conns-per-worker policy (no benchmark fingerprinting, no
// CPU monitor required). The thresholds come from the empirical crossover on
// this hardware: epoll and io_uring tie up to ~16 conns/worker; io_uring
// pulls ahead above ~20/worker and keeps scaling while epoll plateaus
// (io_uring ~+14% at 64 conns/worker); epoll wins at ~1 conn (lower latency).
//
// Policy:
//   - On epoll, switch UP to io_uring when conns/worker sustains the up
//     threshold for sustainTicks consecutive ticks, OR snap immediately when
//     conns/worker crosses the heavy-load high-watermark (the fast path).
//   - On io_uring, revert DOWN to epoll when conns/worker sustains BELOW the
//     down threshold for sustainTicks ticks. The down threshold sits well
//     under the up threshold so the band between them is a hysteresis zone
//     that prevents flapping.
//   - Large-payload workloads (avg bytes/req above largePayloadBytes) are
//     link-bound — the engines tie — so an io_uring switch is suppressed to
//     avoid pointless churn.
//   - A safety revert fires if io_uring is active and the error rate climbs
//     above errorRevertRate, regardless of load.
//
// The oscillation lock (3 switches in 5 min → 5 min lock) and the post-switch
// cooldown bound any residual thrash and hold io_uring after the fast snap.

type controllerState struct {
	activeIsPrimary bool
	lastSwitch      time.Time
	switchTimes     [6]time.Time
	switchIdx       int
	switchCount     int
	locked          bool
	lockUntil       time.Time

	// upTicks / downTicks count consecutive evaluations that satisfy the
	// switch-up / switch-down condition; a normal switch needs sustainTicks
	// of them, the heavy-load fast path needs only one. Reset on a switch
	// (recordSwitch) and whenever the condition lapses.
	upTicks   int
	downTicks int
}

type controller struct {
	primary   engine.Engine // epoll  (low-conns winner / starting engine)
	secondary engine.Engine // io_uring (high-conns winner)
	sampler   TelemetrySampler
	state     controllerState

	evalInterval time.Duration
	cooldown     time.Duration

	upThreshold       float64 // conns/worker: epoll → io_uring
	downThreshold     float64 // conns/worker: io_uring → epoll (hysteresis low edge)
	highWatermark     float64 // conns/worker: heavy-load fast-path snap
	largePayloadBytes float64 // avg bytes/req above which io_uring is suppressed
	errorRevertRate   float64 // io_uring error rate above which we revert to epoll
	sustainTicks      int     // consecutive ticks required for a normal switch

	// connSwitchEnabled gates the conns-per-worker UP switch (epoll→io_uring).
	// Production sets it true ONLY on the epoll-start path with io_uring viable
	// and a non-h2c protocol: there, a sustained high-concurrency ramp should
	// promote NEW connections to io_uring (it wins ≥~24 conns/worker for h1
	// small payloads). It is false when io_uring is the start engine (nothing
	// better to switch up to), when io_uring is unviable, or for h2c. The
	// always-on error-revert below is independent of this flag. The engine sets
	// it directly from the profile in New().
	connSwitchEnabled bool

	// loadDownRevert gates the LOAD-driven io_uring→epoll revert (evaluateDown).
	// It is OFF in production: because pinned conns never migrate, reverting on
	// a load dip strands established io_uring keep-alives and routes new conns
	// back to epoll mid-ramp — pure harm. The always-on error-revert is
	// independent of this flag. Defaults true in newController so the
	// conns-per-worker unit tests exercise evaluateDown; New() sets it false.
	loadDownRevert bool

	logger *slog.Logger
}

func newController(primary, secondary engine.Engine, sampler TelemetrySampler, logger *slog.Logger) *controller {
	return &controller{
		primary:      primary,
		secondary:    secondary,
		sampler:      sampler,
		evalInterval: 1 * time.Second,
		cooldown:     30 * time.Second,
		// Thresholds from the epoll-vs-io_uring sweep: io_uring overtakes epoll
		// at ~24 conns/worker for h1 small payloads (was 20); the heavy-load
		// fast-path snaps only well past the crossover (48); large payloads are
		// link-bound (engines tie) above 8 KB so suppress the switch there.
		upThreshold:       24.0,
		downThreshold:     12.0,
		highWatermark:     48.0,
		largePayloadBytes: 8192.0,
		errorRevertRate:   0.05,
		sustainTicks:      2,
		// Both default ON so the conns-per-worker unit tests exercise the policy;
		// the production New() path sets connSwitchEnabled from the kernel/feature
		// profile and loadDownRevert=false (pinning makes load-revert harmful).
		connSwitchEnabled: true,
		loadDownRevert:    true,
		logger:            logger,
		state: controllerState{
			activeIsPrimary: true,
		},
	}
}

// evaluate decides whether a switch is warranted given the current load. It
// returns true when the engine should switch to its standby. The decision is
// driven entirely by conns-per-worker (with payload-size and error-rate
// refinements); the frozen check, oscillation lock and cooldown gate it.
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

	active := c.activeEngine()
	snap := c.sampler.Sample(active)
	cpw := snap.ConnsPerWorker

	if active.Type() == engine.IOUring {
		// Safety error-revert is ALWAYS active, independent of connSwitchEnabled:
		// if io_uring starts erroring on this deployment, fall back to epoll.
		if snap.ErrorRate > c.errorRevertRate {
			c.state.downTicks = 0
			c.state.upTicks = 0
			c.logSwitch("io_uring", "epoll", "error-rate safety revert", cpw, snap)
			return true
		}
		if !c.connSwitchEnabled || !c.loadDownRevert {
			// Load-driven down-revert is disabled in production: pinned conns
			// never migrate, so reverting only strands io_uring keep-alives and
			// routes new conns to epoll mid-ramp. Only the error-revert above moves us.
			return false
		}
		return c.evaluateDown(snap, cpw)
	}
	// epoll active.
	if !c.connSwitchEnabled {
		return false
	}
	return c.evaluateUp(snap, cpw)
}

// evaluateUp runs while epoll is active and considers a switch UP to io_uring.
func (c *controller) evaluateUp(snap TelemetrySnapshot, cpw float64) bool {
	// Large payloads are link-bound: the engines tie, so never switch up.
	// Keep upTicks pinned at zero so a later small-payload burst restarts the
	// sustain count from scratch.
	largePayload := snap.BytesPerReq >= c.largePayloadBytes

	switch {
	case !largePayload && cpw >= c.highWatermark:
		// Heavy-load fast path: snap immediately on a single tick.
		c.state.upTicks++
		c.state.downTicks = 0
		c.logSwitch("epoll", "io_uring", "heavy-load fast path", cpw, snap)
		return true
	case !largePayload && cpw >= c.upThreshold:
		c.state.upTicks++
		c.state.downTicks = 0
		if c.state.upTicks >= c.sustainTicks {
			c.logSwitch("epoll", "io_uring", "sustained high load", cpw, snap)
			return true
		}
	default:
		c.state.upTicks = 0
	}
	c.state.downTicks = 0
	return false
}

// evaluateDown runs while io_uring is active and considers a revert to epoll.
func (c *controller) evaluateDown(snap TelemetrySnapshot, cpw float64) bool {
	// Safety revert: an error storm on io_uring beats any load consideration.
	if snap.ErrorRate > c.errorRevertRate {
		c.state.downTicks = 0
		c.state.upTicks = 0
		c.logSwitch("io_uring", "epoll", "error-rate safety revert", cpw, snap)
		return true
	}

	if cpw < c.downThreshold {
		c.state.downTicks++
		if c.state.downTicks >= c.sustainTicks {
			c.state.upTicks = 0
			c.logSwitch("io_uring", "epoll", "sustained low load", cpw, snap)
			return true
		}
	} else {
		c.state.downTicks = 0
	}
	c.state.upTicks = 0
	return false
}

func (c *controller) activeEngine() engine.Engine {
	if c.state.activeIsPrimary {
		return c.primary
	}
	return c.secondary
}

func (c *controller) logSwitch(from, to, reason string, cpw float64, snap TelemetrySnapshot) {
	c.logger.Info("engine switch recommended",
		"from", from,
		"to", to,
		"reason", reason,
		"conns_per_worker", cpw,
		"bytes_per_req", snap.BytesPerReq,
		"error_rate", snap.ErrorRate,
		"active_connections", snap.ActiveConnections,
	)
}

// recordSwitch updates controller state after a switch has been performed.
func (c *controller) recordSwitch(now time.Time) {
	c.state.activeIsPrimary = !c.state.activeIsPrimary
	c.state.lastSwitch = now
	c.state.upTicks = 0
	c.state.downTicks = 0

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
