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
// standby history, no active degradation.
//
// This FAILS against the pre-fix logic: the standby was seeded at
// activeScore*0.80 and only decayed, so standby/active maxed out at ~0.70 and
// could never clear the 1+threshold (1.15) bar. The bias-modeled standby
// estimate makes the switch reachable.
func TestControllerOrganicSwitch(t *testing.T) {
	primary := newMockEngine(engine.Epoll)     // active
	secondary := newMockEngine(engine.IOUring) // standby
	sampler := newSyntheticSampler()

	cfg := resource.Config{Protocol: engine.HTTP1}
	e := newFromEngines(primary, secondary, sampler, cfg)
	e.ctrl.cooldown = 0

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
