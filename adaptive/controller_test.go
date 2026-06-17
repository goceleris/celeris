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

// TestControllerNoSpeculativeSwitchBiasOff is the celeris#341 safety guard: with
// the io_uring bias OFF (the default), the SAME io_uring-sweet-spot workload
// must NOT switch — the standby has never been measured, so the only basis for a
// switch would be the fabricated bias estimate, which could land adaptive on a
// measurably-slower engine. Off-by-default keeps adaptive measurement-driven.
func TestControllerNoSpeculativeSwitchBiasOff(t *testing.T) {
	primary := newMockEngine(engine.Epoll)     // active
	secondary := newMockEngine(engine.IOUring) // standby, never measured
	sampler := newSyntheticSampler()

	cfg := resource.Config{Protocol: engine.HTTP1}
	e := newFromEngines(primary, secondary, sampler, cfg)
	e.ctrl.cooldown = 0
	if e.ctrl.biasEnabled {
		t.Skip("CELERIS_ADAPTIVE_IOURING_BIAS set in env; default-off assertion N/A")
	}

	// Squarely in the io_uring bias sweet spot — would switch if the bias were on.
	sampler.Set(engine.Epoll, TelemetrySnapshot{
		ThroughputRPS:     1000,
		ActiveConnections: 2048,
		CPUUtilization:    0.9,
	})

	now := time.Now()
	for i := range 5 {
		if e.ctrl.evaluate(now.Add(time.Duration(i+1)*time.Minute), false) {
			t.Fatal("bias off: must NOT speculatively switch to the unmeasured io_uring standby")
		}
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
