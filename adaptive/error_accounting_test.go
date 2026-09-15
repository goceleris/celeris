//go:build linux

package adaptive

import (
	"testing"

	"github.com/goceleris/celeris/engine"
)

// celeris#645: one adaptive cell reported 421 engine errors in a 112 s window
// against 63 for the same refapp on io_uring and 0 on epoll, all of them
// arriving after the promotion at about five a second. ErrorCount was a single
// number, so the artifact could bound the answer ("not what either sub-engine
// produces on its own") and nothing more: it could not say whether the engine
// was running out of descriptors, overflowing a conn table, tearing accepts
// down at the switch, or failing sends, and it could not say which sub-engine
// was doing it. These tests pin both halves of the instrument that replaces it.
//
// NOTE ON WHERE THESE LIVE: CI does not run ./adaptive/... (celeris#641), so
// these are local/cluster-enforced only. The rules they pin that CAN be
// enforced by the pipeline are pinned again where CI does run — the bucket
// partition in engine/internal/errclass, the wiring in engine, and the
// per-branch attribution in engine/epoll and engine/iouring.

// TestMetricsSumsEveryErrorBucket pins the aggregation RULE for the eleven cause
// buckets. The reflective guard next door can only ask whether a field is
// nonzero, and a bucket wired to the wrong sub-engine, taken from one side
// instead of summed, or maxed instead of added is still nonzero.
//
// Every bucket is cumulative and engine-local, so every one of them adds —
// unlike the two *MaxNanos fields celeris#642 got wrong the other way.
func TestMetricsSumsEveryErrorBucket(t *testing.T) {
	e, _ := newAdaptiveStartingOnEpoll(t)
	// Deliberately asymmetric: no bucket has the same value on both sides,
	// and no side dominates on all of them, so a rule that took one side or
	// picked a maximum lands on a different number for at least one field.
	e.primary.(*mockEngine).SetMetrics(engine.EngineMetrics{
		ErrorCount:            1 + 2 + 3 + 4 + 5 + 6 + 7 + 8 + 9 + 10 + 12,
		ErrorAcceptFDLimit:    1,
		ErrorAcceptCancelled:  2,
		ErrorAcceptOther:      3,
		ErrorConnTableCap:     4,
		ErrorConnRegister:     5,
		ErrorListenerRecreate: 6,
		ErrorTransplantAdopt:  7,
		ErrorSendPeerGone:     12,
		ErrorSend:             8,
		ErrorRequestBody:      9,
		ErrorHandler:          10,
	})
	e.secondary.(*mockEngine).SetMetrics(engine.EngineMetrics{
		ErrorCount:            100 + 90 + 80 + 70 + 60 + 50 + 40 + 35 + 30 + 20 + 11,
		ErrorAcceptFDLimit:    100,
		ErrorAcceptCancelled:  90,
		ErrorAcceptOther:      80,
		ErrorConnTableCap:     70,
		ErrorConnRegister:     60,
		ErrorListenerRecreate: 50,
		ErrorTransplantAdopt:  40,
		ErrorSendPeerGone:     35,
		ErrorSend:             30,
		ErrorRequestBody:      20,
		ErrorHandler:          11,
	})

	m := e.Metrics()
	for _, c := range []struct {
		field string
		got   uint64
		want  uint64
	}{
		{"ErrorAcceptFDLimit", m.ErrorAcceptFDLimit, 101},
		{"ErrorAcceptCancelled", m.ErrorAcceptCancelled, 92},
		{"ErrorAcceptOther", m.ErrorAcceptOther, 83},
		{"ErrorConnTableCap", m.ErrorConnTableCap, 74},
		{"ErrorConnRegister", m.ErrorConnRegister, 65},
		{"ErrorListenerRecreate", m.ErrorListenerRecreate, 56},
		{"ErrorTransplantAdopt", m.ErrorTransplantAdopt, 47},
		{"ErrorSendPeerGone", m.ErrorSendPeerGone, 47},
		{"ErrorSend", m.ErrorSend, 38},
		{"ErrorRequestBody", m.ErrorRequestBody, 29},
		{"ErrorHandler", m.ErrorHandler, 21},
	} {
		if c.got != c.want {
			t.Errorf("Metrics().%s = %d, want %d (the sum of both sub-engines)",
				c.field, c.got, c.want)
		}
	}

	// The breakdown must account for the whole total, or the split answers a
	// different question than the number the nightly gates on.
	var sum uint64
	for _, v := range []uint64{
		m.ErrorAcceptFDLimit, m.ErrorAcceptCancelled, m.ErrorAcceptOther,
		m.ErrorConnTableCap, m.ErrorConnRegister, m.ErrorListenerRecreate,
		m.ErrorTransplantAdopt, m.ErrorSendPeerGone, m.ErrorSend,
		m.ErrorRequestBody, m.ErrorHandler,
	} {
		sum += v
	}
	if sum != m.ErrorCount {
		t.Errorf("the adaptive buckets sum to %d but ErrorCount is %d — on the "+
			"engine celeris#645 could not read, the parts must still add up to "+
			"the whole", sum, m.ErrorCount)
	}
}

// TestMetricsSplitsErrorCountBySubEngine pins the other half: WHICH sub-engine.
// ErrorCount stays the sum (that is the published contract and what the
// controller's error-rate signal reads), and the standby's share comes out
// beside it, so the active engine's own share is the difference — the same
// shape StandbyActiveConnections already has.
//
// #645's whole difficulty was that after a promotion BOTH sub-engines are
// live: the newly-promoted one accepting, the standby still serving every
// keep-alive established before the switch until the transplant drain moves
// it. A single total cannot say which of the two is producing five errors a
// second, and the two have completely different diagnoses.
func TestMetricsSplitsErrorCountBySubEngine(t *testing.T) {
	e, _ := newAdaptiveStartingOnEpoll(t)
	primary := e.primary.(*mockEngine)
	secondary := e.secondary.(*mockEngine)
	primary.SetMetrics(engine.EngineMetrics{ErrorCount: 400, ErrorAcceptCancelled: 400})
	secondary.SetMetrics(engine.EngineMetrics{ErrorCount: 21, ErrorSend: 21})

	m := e.Metrics()
	if m.ErrorCount != 421 {
		t.Errorf("ErrorCount = %d, want 421 — the sum stays the public contract",
			m.ErrorCount)
	}
	if m.StandbyErrorCount != 21 {
		t.Errorf("StandbyErrorCount = %d, want 21 (the io_uring standby's share)",
			m.StandbyErrorCount)
	}
	if got := m.ErrorCount - m.StandbyErrorCount; got != 400 {
		t.Errorf("derived active share = %d, want 400", got)
	}

	// After a promotion the halves swap: epoll becomes the standby, still
	// holding the pre-switch keep-alives, and its errors must now be reported
	// as the standby's.
	e.ForceSwitch()
	if got := e.ActiveEngine(); got != engine.Engine(secondary) {
		t.Fatalf("ForceSwitch did not promote the standby (active is %v)", got.Type())
	}
	m = e.Metrics()
	if m.StandbyErrorCount != 400 {
		t.Errorf("after the promotion StandbyErrorCount = %d, want 400 — the split "+
			"must follow the active pointer, or it names the wrong sub-engine for "+
			"exactly the window celeris#645 is about", m.StandbyErrorCount)
	}
	if m.ErrorCount != 421 {
		t.Errorf("ErrorCount = %d, want 421 — the sum does not move on a switch",
			m.ErrorCount)
	}
}

// TestStandbyErrorCountIsZeroWithNoStandby: on the lazy New() path the standby
// is nil until the first switch, and a sub-engine that does not exist has no
// errors. A split that reported the ACTIVE engine's errors here would invert
// the reading on every cell that never switched.
func TestStandbyErrorCountIsZeroWithNoStandby(t *testing.T) {
	e, _ := newAdaptiveStartingOnEpoll(t)
	e.primary.(*mockEngine).SetMetrics(engine.EngineMetrics{ErrorCount: 63, ErrorSend: 63})
	e.mu.Lock()
	e.secondary = nil
	e.mu.Unlock()

	m := e.Metrics()
	if m.ErrorCount != 63 {
		t.Errorf("ErrorCount = %d, want 63", m.ErrorCount)
	}
	if m.StandbyErrorCount != 0 {
		t.Errorf("StandbyErrorCount = %d, want 0 — an unbuilt standby holds nothing",
			m.StandbyErrorCount)
	}
}
