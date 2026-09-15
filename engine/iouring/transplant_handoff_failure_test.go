//go:build linux

package iouring

import (
	"sync/atomic"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/resource"
)

// celeris#624, the io_uring half of the #383 hand-off's silent drop points.

// newRefusalWorker is newLedgerWorker plus the two celeris#624 drop-point
// counters createWorkers wires per worker.
func newRefusalWorker(fds ...int) *Worker {
	w := newLedgerWorker(fds...)
	w.transplantHandoffRefused = new(atomic.Uint64)
	w.transplantAdoptRefused = new(atomic.Uint64)
	return w
}

// TestRefuseAdoptClosesAndFiresTheHook covers the target-side refusal that is
// NOT an occupied slot: the descriptor falls outside this worker's conn table.
// The source relinquished the connection without firing OnDisconnect, so a
// bare close here — which is what this branch used to do — loses it from
// accepted - closed - active for good, with nothing recorded. The target that
// ends the connection owes the hook.
func TestRefuseAdoptClosesAndFiresTheHook(t *testing.T) {
	fd, other := socketPairFDs(t)
	defer func() { _ = unix.Close(other) }()

	w := newRefusalWorker()
	// Size the conn table so this exact descriptor is one slot past its end,
	// which is the branch under test. Deterministic for any fd number the
	// runtime hands us, unlike relying on where socketpair happens to land.
	w.conns = make([]*connState, fd)
	hooks := 0
	var lastAddr string
	w.cfg = resource.Config{OnDisconnect: func(a string) { hooks++; lastAddr = a }}

	w.attachAdoptedFD(fd, engine.Carryover{RemoteAddr: "127.0.0.1:7"})

	if got := w.transplantAdoptRefused.Load(); got != 1 {
		t.Errorf("transplantAdoptRefused = %d, want 1 — the refusal is still only a "+
			"generic ErrorCount bump", got)
	}
	if got := w.errs.ConnTableCap.Load(); got != 1 {
		t.Errorf("errs.ConnTableCap = %d, want 1 — the cause bucket must still move", got)
	}
	if hooks != 1 {
		t.Fatalf("OnDisconnect fired %d times, want 1 — the source fired none, so a "+
			"target that closes the descriptor owes the hook (celeris#624)", hooks)
	}
	if lastAddr != "127.0.0.1:7" {
		t.Errorf("OnDisconnect got remote %q, want the carried-over address", lastAddr)
	}
	if got := w.closeCount.Load(); got != 1 {
		t.Errorf("closeCount = %d, want 1 — the engine that ended the connection "+
			"must count the close", got)
	}
	if got := w.activeConns.Load(); got != 0 {
		t.Errorf("activeConns = %d, want 0 — the conn never entered this engine's gauge", got)
	}
	if got := w.transplantCount.Load(); got != 0 {
		t.Errorf("transplantCount = %d, want 0 — a refused adopt is not an adopt", got)
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err == nil {
		t.Error("the refused descriptor was left open")
		_ = unix.Close(fd)
	}
}

// TestMetricsCarriesTheHandoffFailureBuckets is the surfacing control for the
// two new io_uring buckets: a counter nothing can read from outside the engine
// is not an instrument.
func TestMetricsCarriesTheHandoffFailureBuckets(t *testing.T) {
	e := &Engine{}
	e.metrics.transplantHandoffRefused.Store(29)
	e.metrics.transplantAdoptRefused.Store(31)

	m := e.Metrics()
	if m.TransplantHandoffRefused != 29 {
		t.Errorf("Metrics().TransplantHandoffRefused = %d, want 29", m.TransplantHandoffRefused)
	}
	if m.TransplantAdoptRefused != 31 {
		t.Errorf("Metrics().TransplantAdoptRefused = %d, want 31", m.TransplantAdoptRefused)
	}
	// io_uring is never the source of a stopped FORWARD drain, and its detach
	// queue has no transplant branch to strand: both buckets are epoll-only.
	if m.TransplantDrainStopped != 0 || m.TransplantStranded != 0 {
		t.Errorf("TransplantDrainStopped = %d, TransplantStranded = %d, want 0 and 0 "+
			"on io_uring", m.TransplantDrainStopped, m.TransplantStranded)
	}
}

// TestWorkersShareTheHandoffFailureBuckets proves createWorkers hands every
// worker the ENGINE's atomics for the two new buckets, not per-worker copies.
// The reflective-style guard that caught celeris#627 does not exist here, so
// this is the wiring's only check.
func TestWorkersShareTheHandoffFailureBuckets(t *testing.T) {
	e, err := New(resource.Config{
		Addr:      "127.0.0.1:0",
		Protocol:  engine.HTTP1,
		Resources: resource.Resources{Workers: 2},
	}, transplantTestHandler{})
	if err != nil {
		t.Skipf("iouring engine unavailable: %v", err)
	}
	resolved := e.cfg.Resources.Resolve()
	workers, err := e.createWorkers(SelectTier(e.profile, 0), make([]int, resolved.Workers), resolved)
	if err != nil {
		t.Skipf("cannot create io_uring workers here: %v", err)
	}
	t.Cleanup(func() {
		for _, w := range workers {
			w.shutdown()
		}
	})
	for i, w := range workers {
		if w.transplantHandoffRefused == nil || w.transplantAdoptRefused == nil {
			t.Fatalf("worker %d has an unwired hand-off failure ledger", i)
		}
		w.transplantHandoffRefused.Add(1)
		w.transplantAdoptRefused.Add(2)
	}
	m := e.Metrics()
	n := uint64(len(workers))
	if m.TransplantHandoffRefused != n {
		t.Errorf("Metrics().TransplantHandoffRefused = %d, want %d", m.TransplantHandoffRefused, n)
	}
	if m.TransplantAdoptRefused != 2*n {
		t.Errorf("Metrics().TransplantAdoptRefused = %d, want %d", m.TransplantAdoptRefused, 2*n)
	}
}
