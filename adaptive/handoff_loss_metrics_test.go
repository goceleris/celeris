//go:build linux

package adaptive

import (
	"testing"

	"github.com/goceleris/celeris/engine"
)

// TestMetricsSumsTheHandoffLossWitnesses pins the combining rule for the
// celeris#657 witnesses, which the reflective guard can only see as nonzero.
// Each event — a stale data CQE, a hand-off with an op in flight — happens on
// exactly one sub-engine, so the adaptive figure is the SUM. The stale CQEs
// of a revert's hand-offs arrive on the sub-engine that made them, which is
// the STANDBY by then; a rule that reported only the active side would read
// zero through the very loss the witnesses exist to report. The two halves
// are set to different values on purpose, so a rule that takes either side
// alone fails.
func TestMetricsSumsTheHandoffLossWitnesses(t *testing.T) {
	e, _ := newAdaptiveStartingOnEpoll(t)
	e.primary.(*mockEngine).SetMetrics(engine.EngineMetrics{
		StaleRecvDataClosed: 1, StaleRecvDataTransplanted: 2,
		StaleRecvDataUnattributed: 3, TransplantHandoffInFlight: 4,
		TransplantHeld: 5, TransplantReaps: 6, TransplantReapMisses: 7,
		TransplantHoldRescued: 8, TransplantDoubleClaim: 9,
	})
	e.secondary.(*mockEngine).SetMetrics(engine.EngineMetrics{
		StaleRecvDataClosed: 10, StaleRecvDataTransplanted: 20,
		StaleRecvDataUnattributed: 30, TransplantHandoffInFlight: 40,
		TransplantHeld: 50, TransplantReaps: 60, TransplantReapMisses: 70,
		TransplantHoldRescued: 80, TransplantDoubleClaim: 90,
	})

	m := e.Metrics()
	for _, c := range []struct {
		field     string
		got, want uint64
	}{
		{"StaleRecvDataClosed", m.StaleRecvDataClosed, 11},
		{"StaleRecvDataTransplanted", m.StaleRecvDataTransplanted, 22},
		{"StaleRecvDataUnattributed", m.StaleRecvDataUnattributed, 33},
		{"TransplantHandoffInFlight", m.TransplantHandoffInFlight, 44},
		{"TransplantHeld", m.TransplantHeld, 55},
		{"TransplantReaps", m.TransplantReaps, 66},
		{"TransplantReapMisses", m.TransplantReapMisses, 77},
		{"TransplantHoldRescued", m.TransplantHoldRescued, 88},
		{"TransplantDoubleClaim", m.TransplantDoubleClaim, 99},
	} {
		if c.got != c.want {
			t.Errorf("Metrics().%s = %d, want %d (sum of both sub-engines)",
				c.field, c.got, c.want)
		}
	}
}
