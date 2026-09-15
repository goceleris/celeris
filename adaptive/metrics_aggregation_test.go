//go:build linux

package adaptive

import (
	"testing"

	"github.com/goceleris/celeris/engine"
)

// TestMetricsCarriesEveryFieldReflectively proves no field is DROPPED. It
// cannot prove a field is COMBINED correctly, because its only question is
// "is this nonzero", and a wrong combining rule is still nonzero.
//
// That gap has teeth for the two *MaxNanos fields. Each is the longest SINGLE
// episode on its sub-engine, so adding two maxima reports an episode nothing
// observed. celeris#607's whole discriminator is duration: SQ-ring pressure
// resolving inside a pass is normal, while an episode measured in SECONDS is a
// connection that received nothing while its peer's bytes sat unread. Summing
// two sub-second maxima into a seconds-long one fabricates exactly the signal
// the field exists to detect, and the reflective guard would pass.
func TestMetricsCombinesMaximaWithMaxAndCountsWithSum(t *testing.T) {
	e, _ := newAdaptiveStartingOnEpoll(t)

	// Deliberately chosen so that a sum and a max are far apart, and so that
	// neither sub-engine dominates on both fields: primary is larger on one
	// maximum and smaller on the other, so a rule that always took one side
	// would fail too.
	e.primary.(*mockEngine).SetMetrics(engine.EngineMetrics{
		RecvSQFull: 3, RecvStallEpisodes: 5, RecvStallNanos: 700,
		RecvStallMaxNanos: 900,
		RecvLinkedArms:    11, RecvLinkedBlockedNanos: 1_300,
		RecvLinkedBlockedMaxNanos: 400,
	})
	e.secondary.(*mockEngine).SetMetrics(engine.EngineMetrics{
		RecvSQFull: 4, RecvStallEpisodes: 6, RecvStallNanos: 1_100,
		RecvStallMaxNanos: 250,
		RecvLinkedArms:    13, RecvLinkedBlockedNanos: 900,
		RecvLinkedBlockedMaxNanos: 2_500,
	})

	m := e.Metrics()
	for _, tc := range []struct {
		field string
		got   uint64
		want  uint64
		why   string
	}{
		{"RecvSQFull", m.RecvSQFull, 7, "a count, so it adds"},
		{"RecvStallEpisodes", m.RecvStallEpisodes, 11, "a count, so it adds"},
		{"RecvStallNanos", m.RecvStallNanos, 1_800, "a total duration, so it adds"},
		{"RecvStallMaxNanos", m.RecvStallMaxNanos, 900, "the longest SINGLE episode: max(900, 250), never 1150"},
		{"RecvLinkedArms", m.RecvLinkedArms, 24, "a count, so it adds"},
		{"RecvLinkedBlockedNanos", m.RecvLinkedBlockedNanos, 2_200, "a total duration, so it adds"},
		{"RecvLinkedBlockedMaxNanos", m.RecvLinkedBlockedMaxNanos, 2_500, "the longest SINGLE wait: max(400, 2500), never 2900"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d (%s)", tc.field, tc.got, tc.want, tc.why)
		}
	}
}

// A sub-engine that has never moved a counter must not drag a maximum down.
// max() over a zero is the identity, but a mistaken min() or an average would
// both pass the equal-sided case above, so the asymmetric case is pinned too.
func TestMetricsMaximaIgnoreAnIdleSubEngine(t *testing.T) {
	e, _ := newAdaptiveStartingOnEpoll(t)
	e.primary.(*mockEngine).SetMetrics(engine.EngineMetrics{
		RecvStallMaxNanos: 5_000, RecvLinkedBlockedMaxNanos: 6_000,
	})
	e.secondary.(*mockEngine).SetMetrics(engine.EngineMetrics{})

	m := e.Metrics()
	if m.RecvStallMaxNanos != 5_000 {
		t.Errorf("RecvStallMaxNanos = %d, want 5000: an idle sub-engine must not lower the maximum", m.RecvStallMaxNanos)
	}
	if m.RecvLinkedBlockedMaxNanos != 6_000 {
		t.Errorf("RecvLinkedBlockedMaxNanos = %d, want 6000: an idle sub-engine must not lower the maximum", m.RecvLinkedBlockedMaxNanos)
	}
}
