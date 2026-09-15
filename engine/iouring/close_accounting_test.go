//go:build linux

package iouring

import (
	"sync/atomic"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/engine/internal/errclass"
	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/resource"
)

// celeris#624: an adaptive cell's live-connection gauge lost two connections
// with no OnDisconnect, and nothing in the engine could say whether a close had
// run and skipped its hook or a transplant hand-off had gone missing. Both of
// the io_uring paths that can drop a connection silently are instrumented here:
// finishClose on an already-nil connState (the hook is guarded on cs != nil,
// the gauge decrement is not), and the detach-for-transplant, which fires no
// hook by design.

// newLedgerWorker extends newDirtyTestWorker with the counters the accounting
// paths need, mirroring what createWorkers wires per worker.
func newLedgerWorker(fds ...int) *Worker {
	w := newDirtyTestWorker(fds...)
	w.errs = &errclass.Counters{}
	w.transplantCount = new(atomic.Uint64)
	w.transplantDetached = new(atomic.Uint64)
	w.transplantSlotOccupied = new(atomic.Uint64)
	w.closeMissingConnState = new(atomic.Uint64)
	return w
}

// TestFinishCloseCountsAMissingConnState pins hypothesis (B) of celeris#624.
// finishClose decrements the live gauge and bumps closeCount unconditionally
// but fires OnDisconnect only for a non-nil connState, so a close on an
// already-cleared slot moves the engine's numbers while every hook-derived
// counter stays put — silently, before this counter existed. The live-state
// case is the control: it must fire the hook and leave the counter at zero.
func TestFinishCloseCountsAMissingConnState(t *testing.T) {
	for _, tc := range []struct {
		name      string
		nilState  bool
		wantHook  int
		wantCount uint64
	}{
		{"nil_state", true, 0, 1},
		{"live_state", false, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fd, other := socketPairFDs(t)
			defer func() { _ = unix.Close(other) }()
			// Non-blocking so the close path's recv drain can never park on
			// a socketpair that has no data and a peer still open.
			_ = unix.SetNonblock(fd, true)

			hooks := 0
			w := newLedgerWorker(fd)
			w.cfg = resource.Config{OnDisconnect: func(string) { hooks++ }}
			if !tc.nilState {
				w.conns[fd] = &connState{fd: fd, liveIdx: -1, remoteAddr: "127.0.0.1:1"}
				w.connCount = 1
			}
			w.activeConns.Add(1)

			// A live connState reaches cancelConnOps, which needs a ring this
			// unit Worker does not have. The accounting under test is
			// sequenced BEFORE that, so recovering still pins it — and
			// asserts the ordering: a counter placed after the cancel would
			// never run on this path.
			func() {
				defer func() {
					if r := recover(); r != nil && tc.nilState {
						panic(r) // the nil path touches no ring: a panic here is a real bug
					}
				}()
				w.finishClose(fd)
			}()

			if got := w.closeMissingConnState.Load(); got != tc.wantCount {
				t.Errorf("closeMissingConnState = %d, want %d — a close that "+
					"skipped its hook is still invisible", got, tc.wantCount)
			}
			if hooks != tc.wantHook {
				t.Errorf("OnDisconnect fired %d times, want %d", hooks, tc.wantHook)
			}
			// Whichever path ran, the engine's own numbers moved: that is
			// precisely why the hook-derived view can disagree with them.
			if got := w.closeCount.Load(); got != 1 {
				t.Errorf("closeCount = %d, want 1", got)
			}
			if got := w.activeConns.Load(); got != 0 {
				t.Errorf("activeConns = %d, want 0", got)
			}
			if !tc.nilState {
				_ = unix.Close(fd)
			}
		})
	}
}

// TestAttachAdoptedFDCountsAnOccupiedSlot pins the first silent drop point
// celeris#624 names on the io_uring side: the conn-table slot is taken, so the
// adopt is refused, the descriptor is deliberately NOT closed, and no hook
// fires. The source has already relinquished the fd, so the connection is lost
// outright — previously visible only as a +1 on the generic ErrorCount.
func TestAttachAdoptedFDCountsAnOccupiedSlot(t *testing.T) {
	fd, other := socketPairFDs(t)
	defer func() { _ = unix.Close(other) }()
	defer func() { _ = unix.Close(fd) }()

	w := newLedgerWorker(fd)
	occupant := &connState{fd: fd, liveIdx: -1}
	w.conns[fd] = occupant
	w.connCount = 1

	w.attachAdoptedFD(fd, engine.Carryover{})

	if got := w.transplantSlotOccupied.Load(); got != 1 {
		t.Errorf("transplantSlotOccupied = %d, want 1 — the refusal is still "+
			"indistinguishable from every other ErrorCount bump", got)
	}
	if got := w.errs.TransplantAdopt.Load(); got != 1 {
		t.Errorf("errs.TransplantAdopt = %d, want 1 (celeris#645: the refusal must "+
			"be counted in the adoption bucket, not folded into the generic total)", got)
	}
	if got := w.errs.Total(); got != 1 {
		t.Errorf("errs.Total() = %d, want 1 (ErrorCount must still move)", got)
	}
	if w.conns[fd] != occupant {
		t.Error("the occupied slot was clobbered")
	}
	if got := w.transplantCount.Load(); got != 0 {
		t.Errorf("transplantCount = %d, want 0 — a refused adopt must not pair "+
			"with a detach", got)
	}
	// The branch leaves the descriptor open on purpose; if that ever changes,
	// the counter's meaning changes with it.
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil {
		t.Errorf("descriptor was closed by the refusal path: %v", err)
	}
}

// TestTryTransplantCountsTheDetach covers the worker-side (sync) half of the
// io_uring→epoll hand-off. It closes the ORIGINAL fd and bumps closeCount, but
// fires no OnDisconnect — the conn lives on under epoll — so without the
// ledger entry the hand-off's departure is unattributable.
func TestTryTransplantCountsTheDetach(t *testing.T) {
	fd, other := socketPairFDs(t)
	defer func() { _ = unix.Close(other) }()

	w := newLedgerWorker(fd)
	hooks := 0
	w.cfg = resource.Config{OnDisconnect: func(string) { hooks++ }}
	w.transplant.Store(&transplantTargetHolder{target: &recordingTarget{}})

	cs := &connState{fd: fd, liveIdx: -1, detected: true, h1State: conn.NewH1State()}
	cs.protocol.Store(int32(engine.HTTP1))
	w.conns[fd] = cs
	w.connCount = 1
	w.activeConns.Add(1)

	// Same recover contract as the other teardown unit tests: cancelConnOps
	// needs a live ring, and it runs AFTER the accounting under test.
	func() {
		defer func() { _ = recover() }()
		w.tryTransplant(fd)
	}()

	if got := w.transplantDetached.Load(); got != 1 {
		t.Errorf("transplantDetached = %d, want 1 — the conn left this engine "+
			"with no hook and no ledger entry", got)
	}
	if w.conns[fd] != nil {
		t.Fatal("the conn was not detached — the eligibility gates rejected the " +
			"setup, so this test proves nothing about the counter")
	}
	if hooks != 0 {
		t.Errorf("OnDisconnect fired %d times, want 0 — a transplant is not a close", hooks)
	}
	if got := w.transplantCount.Load(); got != 0 {
		t.Errorf("transplantCount = %d, want 0 — the source engine adopts nothing", got)
	}
}

// TestFinishAsyncTransplantCountsTheDetach is the same ledger entry on the
// self-initiated (promoted-async) path, which a promoted connection takes
// instead of tryTransplant. celeris#624's cell forces async handlers on, so
// this is the path its drain actually used.
func TestFinishAsyncTransplantCountsTheDetach(t *testing.T) {
	fd, other := socketPairFDs(t)
	defer func() { _ = unix.Close(other) }()

	w := newLedgerWorker(fd)
	hooks := 0
	w.cfg = resource.Config{OnDisconnect: func(string) { hooks++ }}
	w.transplant.Store(&transplantTargetHolder{target: &recordingTarget{}})

	cs := &connState{fd: fd, liveIdx: -1}
	w.conns[fd] = cs
	w.connCount = 1
	w.activeConns.Add(1)

	func() {
		defer func() { _ = recover() }()
		w.finishAsyncTransplant(cs)
	}()

	if got := w.transplantDetached.Load(); got != 1 {
		t.Errorf("transplantDetached = %d, want 1 — the async self-transplant "+
			"left no ledger entry", got)
	}
	if w.conns[fd] != nil {
		t.Fatal("the conn was not detached — setup rejected, the counter is unproven")
	}
	if hooks != 0 {
		t.Errorf("OnDisconnect fired %d times, want 0 — a transplant is not a close", hooks)
	}
}

// TestMetricsCarriesTheCloseLedger is the surfacing control: every counter
// above is engine-wide state that only matters if Metrics() reports it, since
// /debug/vars is the only way the validation artifact can read it. Needs no
// ring, so it runs wherever the package builds.
func TestMetricsCarriesTheCloseLedger(t *testing.T) {
	e := &Engine{}
	e.metrics.transplantCount.Store(11)
	e.metrics.transplantDetached.Store(13)
	e.metrics.transplantSlotOccupied.Store(17)
	e.metrics.closeMissingConnState.Store(19)
	e.metrics.closeCount.Store(23)

	m := e.Metrics()
	for _, c := range []struct {
		field string
		got   uint64
		want  uint64
	}{
		{"TransplantAdopted", m.TransplantAdopted, 11},
		{"TransplantDetached", m.TransplantDetached, 13},
		{"TransplantAdoptSlotOccupied", m.TransplantAdoptSlotOccupied, 17},
		{"CloseMissingConnState", m.CloseMissingConnState, 19},
		{"CloseCount", m.CloseCount, 23},
	} {
		if c.got != c.want {
			t.Errorf("Metrics().%s = %d, want %d — the counter exists but cannot "+
				"be read from outside the engine", c.field, c.got, c.want)
		}
	}
}

// TestWorkersShareTheEngineLedger proves createWorkers hands every worker the
// ENGINE's atomics, not per-worker copies: an increment on any worker must be
// visible through Engine.Metrics(). Skips where io_uring is unavailable, since
// building a worker needs a real ring.
func TestWorkersShareTheEngineLedger(t *testing.T) {
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
		if w.transplantCount == nil || w.transplantDetached == nil ||
			w.transplantSlotOccupied == nil || w.closeMissingConnState == nil {
			t.Fatalf("worker %d has an unwired close ledger — every increment is dropped", i)
		}
		w.transplantCount.Add(1)
		w.transplantDetached.Add(2)
		w.transplantSlotOccupied.Add(3)
		w.closeMissingConnState.Add(4)
	}

	m := e.Metrics()
	n := uint64(len(workers))
	if m.TransplantAdopted != n || m.TransplantDetached != 2*n ||
		m.TransplantAdoptSlotOccupied != 3*n || m.CloseMissingConnState != 4*n {
		t.Errorf("Metrics() = {adopted:%d detached:%d occupied:%d missing:%d}, "+
			"want {%d %d %d %d} over %d workers",
			m.TransplantAdopted, m.TransplantDetached, m.TransplantAdoptSlotOccupied,
			m.CloseMissingConnState, n, 2*n, 3*n, 4*n, n)
	}
}
