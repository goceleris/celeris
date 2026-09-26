//go:build linux

package adaptive

import (
	"reflect"
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

// fillEveryField sets every field of an EngineMetrics to a distinct nonzero
// value, reflectively, so the test never has to name them. base separates the
// two sub-engines' values so a "sum" that silently picks one side is still
// caught by the exact-value assertions below.
func fillEveryField(base int) engine.EngineMetrics {
	var m engine.EngineMetrics
	v := reflect.ValueOf(&m).Elem()
	for i := range v.NumField() {
		f := v.Field(i)
		switch f.Kind() {
		case reflect.Int, reflect.Int32, reflect.Int64:
			f.SetInt(int64(base + i))
		case reflect.Uint, reflect.Uint32, reflect.Uint64:
			f.SetUint(uint64(base + i))
		case reflect.Float32, reflect.Float64:
			f.SetFloat(float64(base + i))
		default:
			// A new field of an unhandled kind would be left at zero and
			// would then fail the aggregation check below for the wrong
			// reason. Fail loudly instead of silently skipping it.
			panic("fillEveryField: unhandled kind " + f.Kind().String() +
				" for EngineMetrics." + v.Type().Field(i).Name)
		}
	}
	return m
}

// TestMetricsCarriesEveryFieldReflectively is celeris#627.
//
// adaptive.Engine.Metrics() builds its result as a field-by-field struct
// literal, and a field omitted from that literal is not inherited from the
// sub-engines — it is reported as ZERO. Ten fields were missing, including
// Workers, AcceptCount, BytesRead/BytesWritten and the must-stay-zero
// celeris#586 recv witnesses, so nightly 34893230678's adaptive column
// published engine_workers=0 and bytes_read=0 on a cell serving 101 live
// connections while the epoll and io_uring columns of the same refapp
// reported 12 workers and tens of megabytes.
//
// The check is driven over the struct by reflection ON PURPOSE. A test that
// enumerates today's field names would drop the next field added exactly the
// way the literal dropped these ten: the failure mode is an omission, and you
// cannot enumerate your way out of an omission.
func TestMetricsCarriesEveryFieldReflectively(t *testing.T) {
	e, _ := newAdaptiveStartingOnEpoll(t)
	e.primary.(*mockEngine).SetMetrics(fillEveryField(1000))
	e.secondary.(*mockEngine).SetMetrics(fillEveryField(2000))
	// AdaptiveSwitches is the one field sourced from the adaptive engine
	// itself rather than from a sub-engine, so give it a nonzero source too
	// — the rule below then applies to every field with no exemptions.
	e.switchesTotal.Store(7)

	got := reflect.ValueOf(e.Metrics())
	typ := got.Type()
	var dropped []string
	for i := range got.NumField() {
		// The one exemption: Throughput is deprecated as always 0 and nothing
		// sets or forwards it (celeris#653, engine.TestNothingSetsThroughput).
		if typ.Field(i).Name == "Throughput" {
			continue
		}
		if got.Field(i).IsZero() {
			dropped = append(dropped, typ.Field(i).Name)
		}
	}
	if len(dropped) > 0 {
		t.Errorf("adaptive Metrics() reports ZERO for %d field(s) that both "+
			"sub-engines report nonzero: %v\n"+
			"A field missing from the struct literal in adaptive.Engine.Metrics() "+
			"is published as 0, not inherited (celeris#627).", len(dropped), dropped)
	}
}

// TestMetricsSumsTheFieldsCelerisGH627Dropped pins the aggregation RULE for
// the fields #627 found missing, which the reflective test above can only see
// as nonzero. Workers is summed rather than taken from the active sub-engine
// because both sub-engines run at once — the standby keeps its loops up and
// keeps serving the keep-alives pinned to it — so the sum is the divisor that
// matches the summed ActiveConnections.
func TestMetricsSumsTheFieldsCelerisGH627Dropped(t *testing.T) {
	e, _ := newAdaptiveStartingOnEpoll(t)
	e.primary.(*mockEngine).SetMetrics(engine.EngineMetrics{
		Workers: 12, AcceptCount: 3000, CloseCount: 2900,
		BytesRead: 53170564, BytesWritten: 1011018543,
		RecvResumeWhileCancelPending: 11, RecvResumeWhileRecvInFlight: 5,
		RecvArmDeclined: 9, RecvDoubleArmed: 1, RecvCQEUnaccounted: 2,
	})
	e.secondary.(*mockEngine).SetMetrics(engine.EngineMetrics{
		Workers: 12, AcceptCount: 400, CloseCount: 380,
		BytesRead: 51373746, BytesWritten: 448635451,
		RecvResumeWhileCancelPending: 3, RecvResumeWhileRecvInFlight: 1,
		RecvArmDeclined: 4, RecvDoubleArmed: 2, RecvCQEUnaccounted: 3,
	})

	m := e.Metrics()
	for _, c := range []struct {
		field string
		got   uint64
		want  uint64
	}{
		{"Workers", uint64(m.Workers), 24},
		{"AcceptCount", m.AcceptCount, 3400},
		{"CloseCount", m.CloseCount, 3280},
		{"BytesRead", m.BytesRead, 104544310},
		{"BytesWritten", m.BytesWritten, 1459653994},
		{"RecvResumeWhileCancelPending", m.RecvResumeWhileCancelPending, 14},
		{"RecvResumeWhileRecvInFlight", m.RecvResumeWhileRecvInFlight, 6},
		{"RecvArmDeclined", m.RecvArmDeclined, 13},
		{"RecvDoubleArmed", m.RecvDoubleArmed, 3},
		{"RecvCQEUnaccounted", m.RecvCQEUnaccounted, 5},
	} {
		if c.got != c.want {
			t.Errorf("Metrics().%s = %d, want %d (sum of both sub-engines)",
				c.field, c.got, c.want)
		}
	}
}
