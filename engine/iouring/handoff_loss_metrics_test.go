//go:build linux

package iouring

import (
	"testing"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/resource"
)

// TestMetricsCarriesTheHandoffLossWitnesses is the surfacing control: the
// witnesses only matter if Metrics() reports them, since /debug/vars is how
// the validation artifact reads an engine. Needs no ring.
func TestMetricsCarriesTheHandoffLossWitnesses(t *testing.T) {
	e := &Engine{}
	e.metrics.handoffLoss.staleRecvDataClosed.Store(3)
	e.metrics.handoffLoss.staleRecvDataTransplanted.Store(5)
	e.metrics.handoffLoss.staleRecvDataUnattributed.Store(7)
	e.metrics.handoffLoss.handoffInFlight.Store(11)

	m := e.Metrics()
	for _, c := range []struct {
		field     string
		got, want uint64
	}{
		{"StaleRecvDataClosed", m.StaleRecvDataClosed, 3},
		{"StaleRecvDataTransplanted", m.StaleRecvDataTransplanted, 5},
		{"StaleRecvDataUnattributed", m.StaleRecvDataUnattributed, 7},
		{"TransplantHandoffInFlight", m.TransplantHandoffInFlight, 11},
	} {
		if c.got != c.want {
			t.Errorf("Metrics().%s = %d, want %d — the witness exists but cannot "+
				"be read from outside the engine", c.field, c.got, c.want)
		}
	}
}

// TestWorkersShareTheHandoffLossWitnesses proves createWorkers hands every
// worker the ENGINE's witness set: an increment on any worker must be visible
// through Engine.Metrics(). Skips where io_uring is unavailable, since
// building a worker needs a real ring, unless CELERIS_REQUIRE_IOURING_WORKERS=1
// forbids the skip (skipOrFail656).
func TestWorkersShareTheHandoffLossWitnesses(t *testing.T) {
	e, err := New(resource.Config{
		Addr:      "127.0.0.1:0",
		Protocol:  engine.HTTP1,
		Resources: resource.Resources{Workers: 2},
	}, transplantTestHandler{})
	if err != nil {
		skipOrFail656(t, "iouring engine unavailable: %v", err)
	}
	resolved := e.cfg.Resources.Resolve()
	workers, err := e.createWorkers(SelectTier(e.profile, 0), make([]int, resolved.Workers), resolved)
	if err != nil {
		skipOrFail656(t, "cannot create io_uring workers here: %v", err)
	}
	t.Cleanup(func() {
		for _, w := range workers {
			w.shutdown()
		}
	})
	for i, w := range workers {
		if w.handoffLoss == nil {
			t.Fatalf("worker %d has no celeris#657 witness set — every increment is dropped", i)
		}
		w.handoffLoss.staleRecvDataTransplanted.Add(1)
		w.handoffLoss.handoffInFlight.Add(2)
	}
	m := e.Metrics()
	n := uint64(len(workers))
	if m.StaleRecvDataTransplanted != n || m.TransplantHandoffInFlight != 2*n {
		t.Errorf("Metrics() = {transplanted:%d inFlight:%d}, want {%d %d} over %d workers",
			m.StaleRecvDataTransplanted, m.TransplantHandoffInFlight, n, 2*n, n)
	}
}
