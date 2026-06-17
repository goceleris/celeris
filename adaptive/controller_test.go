//go:build linux

package adaptive

import (
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/resource"
)

// TestControllerOrganicSwitch verifies that, in the io_uring sweet spot
// (high connection count + high CPU), the controller eventually recommends an
// epoll→io_uring switch driven purely by the io_uring bias — no pre-seeded
// standby history, no active degradation. The bias is opt-in (celeris#341), so
// this exercises it with biasEnabled forced on.
func TestControllerOrganicSwitch(t *testing.T) {
	primary := newMockEngine(engine.Epoll)     // active
	secondary := newMockEngine(engine.IOUring) // standby
	sampler := newSyntheticSampler()

	cfg := resource.Config{Protocol: engine.HTTP1}
	e := newFromEngines(primary, secondary, sampler, cfg)
	e.ctrl.cooldown = 0
	e.ctrl.biasEnabled = true // bias is opt-in; this test exercises it

	// Active epoll snapshot lands squarely in io_uring's empirical sweet spot.
	sampler.Set(engine.Epoll, TelemetrySnapshot{
		ThroughputRPS:     1000,
		ActiveConnections: 2048,
		CPUUtilization:    0.9,
	})

	if e.ActiveEngine().Type() != engine.Epoll {
		t.Fatal("expected epoll active initially")
	}

	now := time.Now()
	switched := false
	// Advance well past any cooldown/observation window and let the estimate
	// settle over a few ticks.
	for i := range 5 {
		if e.ctrl.evaluate(now.Add(time.Duration(i+1)*time.Minute), false) {
			switched = true
			break
		}
	}
	if !switched {
		t.Fatal("expected organic epoll→io_uring switch in the io_uring sweet spot")
	}
}

// TestControllerRevertsFromSlowerExploredEngine is the celeris#338 reversibility
// guard — the core of the safe bias. io_uring is active (as if just explored to)
// in the bias sweet spot, but it MEASURES slower than epoll's known score. The
// controller MUST revert to epoll: the bias may explore but must never block a
// measurement-driven reversion (it neither inflates the active score nor
// suppresses the epoll standby). This is exactly the case the old sticky bias
// got wrong (it parked adaptive on the slower engine).
func TestControllerRevertsFromSlowerExploredEngine(t *testing.T) {
	primary := newMockEngine(engine.IOUring) // active (explored-to)
	secondary := newMockEngine(engine.Epoll) // standby, measured-faster historically
	sampler := newSyntheticSampler()

	cfg := resource.Config{Protocol: engine.HTTP1}
	e := newFromEngines(primary, secondary, sampler, cfg)
	e.ctrl.cooldown = 0
	e.ctrl.biasEnabled = true

	now := time.Now()
	// epoll was measured fast before; io_uring now measures slow IN the sweet spot.
	e.ctrl.state.lastActiveScore[engine.Epoll] = 1000
	e.ctrl.state.lastActiveTime[engine.Epoll] = now
	sampler.Set(engine.IOUring, TelemetrySnapshot{
		ThroughputRPS:     500,
		ActiveConnections: 2048,
		CPUUtilization:    0.9,
	})

	if !e.ctrl.evaluate(now, false) {
		t.Fatal("io_uring measuring slower than epoll's historical must REVERT to epoll even in the io_uring bias sweet spot — the bias must not block measured reversion")
	}
}

// TestControllerBiasOffNoExplore verifies the kill-switch: with the bias forced
// off (CELERIS_ADAPTIVE_IOURING_BIAS=0), the controller is purely
// measurement-driven and does NOT explore the unmeasured io_uring standby even
// in the sweet spot.
func TestControllerBiasOffNoExplore(t *testing.T) {
	primary := newMockEngine(engine.Epoll)     // active
	secondary := newMockEngine(engine.IOUring) // standby, never measured
	sampler := newSyntheticSampler()

	cfg := resource.Config{Protocol: engine.HTTP1}
	e := newFromEngines(primary, secondary, sampler, cfg)
	e.ctrl.cooldown = 0
	e.ctrl.biasEnabled = false // kill-switch

	sampler.Set(engine.Epoll, TelemetrySnapshot{
		ThroughputRPS:     1000,
		ActiveConnections: 2048,
		CPUUtilization:    0.9,
	})

	now := time.Now()
	for i := range 5 {
		if e.ctrl.evaluate(now.Add(time.Duration(i+1)*time.Minute), false) {
			t.Fatal("bias off: must not explore the unmeasured io_uring standby")
		}
	}
}

// TestControllerStableUnderFluctuation is the celeris#338 real-load stability
// guard: at the 1024c sweet spot where io_uring is marginally faster (~+7%, well
// under the 15% switch threshold), with ±4% telemetry jitter on both engines,
// the controller must EXPLORE to io_uring and SETTLE there without thrashing.
// The 15% threshold provides the hysteresis; the reversible bias provides the
// explore. (Unit tests cover static telemetry; this covers fluctuation.)
func TestControllerStableUnderFluctuation(t *testing.T) {
	primary := newMockEngine(engine.Epoll)     // start active
	secondary := newMockEngine(engine.IOUring) // marginally faster at 1024c
	sampler := newSyntheticSampler()

	cfg := resource.Config{Protocol: engine.HTTP1}
	e := newFromEngines(primary, secondary, sampler, cfg)
	c := e.ctrl
	c.cooldown = 0
	c.biasEnabled = true

	now := time.Now()
	switches := 0
	// activeType derives the controller's chosen engine from its decision state
	// (recordSwitch toggles activeIsPrimary; the engine-level performSwitch is
	// out of scope for a controller-logic test).
	activeType := func() engine.EngineType {
		if c.state.activeIsPrimary {
			return primary.Type()
		}
		return secondary.Type()
	}
	for i := range 60 {
		// Deterministic ±4% jitter (no rand): pattern over -2..+2. Both engines
		// are set each tick; evaluate samples whichever it currently calls active.
		jit := func(base float64) float64 { return base * (1.0 + 0.04*float64((i%5)-2)/2) }
		sampler.Set(engine.Epoll, TelemetrySnapshot{ThroughputRPS: jit(1240), ActiveConnections: 1024, CPUUtilization: 0.85})
		sampler.Set(engine.IOUring, TelemetrySnapshot{ThroughputRPS: jit(1330), ActiveConnections: 1024, CPUUtilization: 0.85})
		tn := now.Add(time.Duration(i+1) * time.Second)
		if c.evaluate(tn, false) {
			c.recordSwitch(tn)
			switches++
		}
	}

	if activeType() != engine.IOUring {
		t.Fatalf("adaptive should explore + settle on the faster io_uring at 1024c, got %s", activeType())
	}
	if switches > 3 {
		t.Fatalf("excessive switching under fluctuation (%d) — possible thrash; 15%% threshold should hold once settled", switches)
	}
}

// TestControllerNoSwitchOutsideSweetSpot is the inverse: low CPU or too few
// connections yields zero bias, so the controller must NOT recommend a switch
// (no degradation, no favorable conditions).
func TestControllerNoSwitchOutsideSweetSpot(t *testing.T) {
	cases := []struct {
		name string
		snap TelemetrySnapshot
	}{
		{
			name: "low CPU",
			snap: TelemetrySnapshot{ThroughputRPS: 1000, ActiveConnections: 2048, CPUUtilization: 0.10},
		},
		{
			name: "too few connections",
			snap: TelemetrySnapshot{ThroughputRPS: 1000, ActiveConnections: 32, CPUUtilization: 0.9},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			primary := newMockEngine(engine.Epoll)
			secondary := newMockEngine(engine.IOUring)
			sampler := newSyntheticSampler()

			cfg := resource.Config{Protocol: engine.HTTP1}
			e := newFromEngines(primary, secondary, sampler, cfg)
			e.ctrl.cooldown = 0

			sampler.Set(engine.Epoll, tc.snap)

			now := time.Now()
			for i := range 5 {
				if e.ctrl.evaluate(now.Add(time.Duration(i+1)*time.Minute), false) {
					t.Fatalf("unexpected switch outside io_uring sweet spot (%s)", tc.name)
				}
			}
		})
	}
}
