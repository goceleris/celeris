//go:build linux

package adaptive

import (
	"testing"

	"github.com/goceleris/celeris/engine"
)

// celeris#624: one adaptive cell's live-connection gauge stepped down by two
// with no OnDisconnect and held there for 56 seconds. ActiveConnections was a
// bare sum of the two sub-engines and CloseCount was not aggregated at all, so
// the artifact could see neither which side lost the connections nor whether
// the engine had counted a close the hooks missed. These tests pin the shape
// of the instrument: the sums stay the public contract, and the standby's
// share plus the engine's own close ledger come out alongside them.

// TestMetricsSplitsTheLiveGaugeBySubEngine: ActiveConnections and CloseCount
// stay sums (the controller divides the former by Workers), and the standby's
// share is reported separately so the active engine's own share is the
// difference. The split must follow the ACTIVE pointer across a switch.
func TestMetricsSplitsTheLiveGaugeBySubEngine(t *testing.T) {
	e, _ := newAdaptiveStartingOnEpoll(t)
	primary := e.primary.(*mockEngine)
	secondary := e.secondary.(*mockEngine)
	primary.SetMetrics(engine.EngineMetrics{ActiveConnections: 96, CloseCount: 400})
	secondary.SetMetrics(engine.EngineMetrics{ActiveConnections: 14, CloseCount: 7})

	m := e.Metrics()
	if m.ActiveConnections != 110 {
		t.Errorf("ActiveConnections = %d, want 110 — the sum is the public "+
			"contract and the controller divides it by Workers", m.ActiveConnections)
	}
	if m.CloseCount != 407 {
		t.Errorf("CloseCount = %d, want 407 — without it engine_closed cannot be "+
			"compared against hook_closed on the only engine that can lose a "+
			"conn to a hand-off", m.CloseCount)
	}
	if m.StandbyActiveConnections != 14 {
		t.Errorf("StandbyActiveConnections = %d, want 14 (io_uring standby)",
			m.StandbyActiveConnections)
	}
	if m.StandbyCloseCount != 7 {
		t.Errorf("StandbyCloseCount = %d, want 7 (io_uring standby)", m.StandbyCloseCount)
	}
	if got := m.ActiveConnections - m.StandbyActiveConnections; got != 96 {
		t.Errorf("derived active share = %d, want 96", got)
	}

	// After a promotion the halves swap: epoll is the standby and still holds
	// every keep-alive established before the switch, which is exactly the
	// population celeris#624's drain was moving.
	e.ForceSwitch()
	if got := e.ActiveEngine(); got != engine.Engine(secondary) {
		t.Fatalf("ForceSwitch did not promote the standby (active is %v)", got.Type())
	}
	m = e.Metrics()
	if m.StandbyActiveConnections != 96 {
		t.Errorf("after the switch StandbyActiveConnections = %d, want 96 (epoll "+
			"is now the standby) — the split does not follow the active engine",
			m.StandbyActiveConnections)
	}
	if m.StandbyCloseCount != 400 {
		t.Errorf("after the switch StandbyCloseCount = %d, want 400", m.StandbyCloseCount)
	}
	if m.ActiveConnections != 110 || m.CloseCount != 407 {
		t.Errorf("a switch changed the sums: active=%d close=%d, want 110/407",
			m.ActiveConnections, m.CloseCount)
	}
}

// TestMetricsSumsTheTransplantLedger: both halves of a hand-off land on
// opposite sub-engines, so only the summed ledger makes
// TransplantDetached - TransplantAdopted the count of connections currently in
// flight between them — the residual that names hypothesis (A). The two
// must-stay-zero counters are summed for the same reason: either sub-engine
// can be the one that dropped a connection silently.
func TestMetricsSumsTheTransplantLedger(t *testing.T) {
	e, _ := newAdaptiveStartingOnEpoll(t)
	e.primary.(*mockEngine).SetMetrics(engine.EngineMetrics{
		TransplantDetached:          9, // epoll drained 9 to io_uring
		TransplantAdopted:           1, // and took 1 back on a revert
		TransplantAdoptSlotOccupied: 2,
		CloseMissingConnState:       3,
	})
	e.secondary.(*mockEngine).SetMetrics(engine.EngineMetrics{
		TransplantAdopted:           7, // io_uring adopted 7 of the 9
		TransplantDetached:          1,
		TransplantAdoptSlotOccupied: 4,
		CloseMissingConnState:       5,
	})

	m := e.Metrics()
	if m.TransplantDetached != 10 {
		t.Errorf("TransplantDetached = %d, want 10", m.TransplantDetached)
	}
	if m.TransplantAdopted != 8 {
		t.Errorf("TransplantAdopted = %d, want 8", m.TransplantAdopted)
	}
	// The whole point of summing: the residual is the in-flight count.
	if got := m.TransplantDetached - m.TransplantAdopted; got != 2 {
		t.Errorf("detached-adopted = %d, want 2 — the residual that names an "+
			"unpaired hand-off does not survive the aggregation", got)
	}
	if m.TransplantAdoptSlotOccupied != 6 {
		t.Errorf("TransplantAdoptSlotOccupied = %d, want 6", m.TransplantAdoptSlotOccupied)
	}
	if m.CloseMissingConnState != 8 {
		t.Errorf("CloseMissingConnState = %d, want 8", m.CloseMissingConnState)
	}
}

// TestMetricsStandbyZeroWhenStandbyUnbuilt: under the default policy the
// standby is lazy and the slot stays nil until the first switch needs it. A
// standby that does not exist holds no connections, so the split must read
// zero rather than fabricating the active engine's own numbers.
func TestMetricsStandbyZeroWhenStandbyUnbuilt(t *testing.T) {
	e, _ := newAdaptiveStartingOnEpoll(t)
	e.primary.(*mockEngine).SetMetrics(engine.EngineMetrics{ActiveConnections: 42, CloseCount: 17})
	e.mu.Lock()
	e.secondary = nil // the production lazy-standby state
	e.mu.Unlock()

	m := e.Metrics()
	if m.ActiveConnections != 42 || m.CloseCount != 17 {
		t.Errorf("sums = active %d / close %d, want 42 / 17", m.ActiveConnections, m.CloseCount)
	}
	if m.StandbyActiveConnections != 0 || m.StandbyCloseCount != 0 {
		t.Errorf("unbuilt standby reported active %d / close %d, want 0 / 0 — the "+
			"split is attributing the ACTIVE engine's conns to a standby that "+
			"does not exist", m.StandbyActiveConnections, m.StandbyCloseCount)
	}
}
