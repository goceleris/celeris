//go:build linux

package epoll

import (
	"context"
	"errors"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// celeris#624, the remaining silent drop points on the epoll half of the #383
// hand-off. Each one used to end in a bare unix.Close of a descriptor the loop
// had ALREADY dropped from its live gauge without firing OnDisconnect — so the
// connection died, accepted - closed - active stayed permanently short by one,
// and nothing recorded that it had happened. The connection is now reclaimed
// onto the loop it came from, which is both survivable for the peer and what
// pairs the detach with an adopt so the residual returns to zero.

// refusingTarget always refuses, which is what an io_uring engine with no
// workers (or an out-of-range descriptor) does.
type refusingTarget struct{ calls atomic.Int64 }

func (r *refusingTarget) AdoptConn(int, engine.Carryover) error {
	r.calls.Add(1)
	return errors.New("refused")
}

var _ engine.TransplantTarget = (*refusingTarget)(nil)

// newHandoffLoop is newLedgerLoop plus the four celeris#624 drop-point
// counters and an OnDisconnect hook, mirroring what Engine.Listen wires.
func newHandoffLoop(t *testing.T, onDisconnect func(string)) *Loop {
	t.Helper()
	l := newLedgerLoop(t)
	l.transplantHandoffRefused = &atomic.Uint64{}
	l.transplantDrainStopped = &atomic.Uint64{}
	l.transplantStranded = &atomic.Uint64{}
	l.transplantAdoptRefused = &atomic.Uint64{}
	l.cfg = resource.Config{OnDisconnect: onDisconnect}
	return l
}

// fdIsOpen reports whether fd is still a live descriptor in this process.
func fdIsOpen(fd int) bool {
	_, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	return err == nil
}

// pendingTransplant builds the state tryTransplant leaves behind on the
// deferred async path: the fd is out of epoll and out of the conn table, the
// live gauge has already been decremented, and the hand-off is owed.
func pendingTransplant(t *testing.T, l *Loop) (*connState, int) {
	t.Helper()
	fd := socketpairFD(t, l)
	cs := &connState{fd: fd, liveIdx: -1, remoteAddr: "127.0.0.1:9"}
	cs.transplantPending = true
	l.transplantInFlight++
	l.transplantDetached.Add(1)
	return cs, fd
}

// TestHandoffReclaimsTheConnWhenTheDrainStopped covers the revert race: the
// adaptive engine reverted between the detach and the hand-off, so
// l.transplant is nil by the time the dispatch goroutine's connState reaches
// the drain. This engine is the ACTIVE one again at that point, and the conn
// is healthy and at a clean boundary, so it belongs back here.
func TestHandoffReclaimsTheConnWhenTheDrainStopped(t *testing.T) {
	hooks := 0
	l := newHandoffLoop(t, func(string) { hooks++ })
	cs, fd := pendingTransplant(t, l)

	l.finishTransplantHandoff(cs) // l.transplant is nil: the drain was stopped

	if got := l.transplantDrainStopped.Load(); got != 1 {
		t.Errorf("transplantDrainStopped = %d, want 1 — the branch is still silent", got)
	}
	if !fdIsOpen(fd) {
		t.Fatal("the descriptor was closed: a stopped drain must not kill a healthy " +
			"keep-alive the loop is about to own again (celeris#624)")
	}
	if l.conns[fd] == nil {
		t.Error("the conn was not re-adopted onto the loop, so it is owned by nobody")
	}
	if got := l.transplantAdopted.Load(); got != 1 {
		t.Errorf("transplantAdopted = %d, want 1 — the reclaim must pair with the "+
			"detach or the residual never returns to zero", got)
	}
	if got := l.transplantDetached.Load() - l.transplantAdopted.Load(); got != 0 {
		t.Errorf("residual detached-adopted = %d, want 0", got)
	}
	if l.transplantInFlight != 0 {
		t.Errorf("transplantInFlight = %d, want 0 — the suspend gate would never "+
			"let this loop park again", l.transplantInFlight)
	}
	if hooks != 0 || l.closeCount.Load() != 0 {
		t.Errorf("OnDisconnect fired %d times and closeCount = %d, want 0 and 0 — "+
			"the connection did not end", hooks, l.closeCount.Load())
	}
}

// TestHandoffReclaimsTheConnWhenTheTargetRefusesIt is the same recovery for
// the other half of the branch: the drain is still on, but the target said no.
func TestHandoffReclaimsTheConnWhenTheTargetRefusesIt(t *testing.T) {
	hooks := 0
	l := newHandoffLoop(t, func(string) { hooks++ })
	target := &refusingTarget{}
	l.transplant.Store(&transplantState{target: target})
	cs, fd := pendingTransplant(t, l)

	l.finishTransplantHandoff(cs)

	if target.calls.Load() != 1 {
		t.Fatalf("AdoptConn called %d times, want 1 — the setup never reached the "+
			"refusal", target.calls.Load())
	}
	if got := l.transplantHandoffRefused.Load(); got != 1 {
		t.Errorf("transplantHandoffRefused = %d, want 1 — the branch is still silent", got)
	}
	if got := l.transplantDrainStopped.Load(); got != 0 {
		t.Errorf("transplantDrainStopped = %d, want 0 — a refusal is not a stopped "+
			"drain, and one bucket that answers both names neither", got)
	}
	if !fdIsOpen(fd) {
		t.Fatal("the descriptor was closed: the target refusing it says nothing about " +
			"the connection's health (celeris#624)")
	}
	if l.conns[fd] == nil {
		t.Error("the conn was not re-adopted onto the loop, so it is owned by nobody")
	}
	if got := l.transplantDetached.Load() - l.transplantAdopted.Load(); got != 0 {
		t.Errorf("residual detached-adopted = %d, want 0", got)
	}
	if hooks != 0 || l.closeCount.Load() != 0 {
		t.Errorf("OnDisconnect fired %d times and closeCount = %d, want 0 and 0", hooks, l.closeCount.Load())
	}
}

// TestDetachQueueRunsTheTransplantBranchBeforeTheClosedGuard pins the ORDER of
// the two checks at the top of drainDetachQueue. The already-closed guard used
// to run first, so a transplant-pending conn that somehow also carried the
// closed flag was dropped by a bare `continue`: not handed off, not closed, no
// hook, no counter, and its live-gauge decrement already taken.
//
// The two flags are mutually exclusive by construction — closeConn bails on a
// nil conn-table slot, and detachFromEpoll nils that slot before
// transplantPending is ever set, so the close can never reach the statement
// that sets detachClosed. This test is what makes that argument checkable: the
// transplant branch must win, and the impossible coincidence must be COUNTED
// rather than swallowed.
func TestDetachQueueRunsTheTransplantBranchBeforeTheClosedGuard(t *testing.T) {
	l := newHandoffLoop(t, nil)
	cs, fd := pendingTransplant(t, l)
	cs.detachClosed = true
	defer func() { _ = unix.Close(fd) }()

	l.detachQueue = append(l.detachQueue, cs)
	l.detachQPending.Store(1)
	l.drainDetachQueue()

	if got := l.transplantStranded.Load(); got != 1 {
		t.Errorf("transplantStranded = %d, want 1 — the entry was dropped by the "+
			"already-closed guard before the transplant branch could see it, which "+
			"is the ordering celeris#624 names", got)
	}
	if l.transplantInFlight != 0 {
		t.Errorf("transplantInFlight = %d, want 0 — the debt must be settled on "+
			"every exit from finishTransplantHandoff", l.transplantInFlight)
	}
}

// TestAttachAdoptedFDRefusalClosesAndFiresTheHook covers the TARGET-side
// refusals that are not an occupied slot: the descriptor cannot be registered
// on this loop's epoll set. The source relinquished the conn without firing
// OnDisconnect, so closing it here without the hook loses it from
// accepted - closed - active for good. A regular file drives the branch
// deterministically — epoll_ctl refuses it with EPERM.
func TestAttachAdoptedFDRefusalClosesAndFiresTheHook(t *testing.T) {
	hooks := 0
	var lastAddr string
	l := newHandoffLoop(t, func(a string) { hooks++; lastAddr = a })

	f, err := os.CreateTemp(t.TempDir(), "adopt")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	fd := int(f.Fd())
	if fd >= len(l.conns) {
		t.Skipf("temp-file fd %d outside the test conn table", fd)
	}

	l.attachAdoptedFD(context.Background(), fd, engine.Carryover{RemoteAddr: "127.0.0.1:7"}, 1)

	if got := l.transplantAdoptRefused.Load(); got != 1 {
		t.Errorf("transplantAdoptRefused = %d, want 1 — the refusal is still only a "+
			"generic ErrorCount bump", got)
	}
	if got := l.errs.ConnRegister.Load(); got != 1 {
		t.Errorf("errs.ConnRegister = %d, want 1 — the cause bucket must still move", got)
	}
	if hooks != 1 {
		t.Fatalf("OnDisconnect fired %d times, want 1 — the source fired none, so a "+
			"target that closes the descriptor owes the hook or the connection "+
			"vanishes from the ledger (celeris#624)", hooks)
	}
	if lastAddr != "127.0.0.1:7" {
		t.Errorf("OnDisconnect got remote %q, want the carried-over address", lastAddr)
	}
	if got := l.closeCount.Load(); got != 1 {
		t.Errorf("closeCount = %d, want 1 — the engine that ended the connection "+
			"must count the close", got)
	}
	if got := l.activeConns.Load(); got != 0 {
		t.Errorf("activeConns = %d, want 0 — the conn never entered this engine's "+
			"gauge, and the source already decremented its own", got)
	}
	if got := l.transplantAdopted.Load(); got != 0 {
		t.Errorf("transplantAdopted = %d, want 0 — a refused adopt is not an adopt", got)
	}
	if fdIsOpen(fd) {
		t.Error("the refused descriptor was left open")
	}
	// The *os.File finalizer would double-close the number we just closed.
	runtimeKeepFileClosed(f)
}

// runtimeKeepFileClosed detaches f from its descriptor after attachAdoptedFD's
// refusal path has closed it, so the *os.File does not close the same number
// again once it is garbage.
func runtimeKeepFileClosed(f *os.File) {
	fd, err := unix.Open(os.DevNull, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return
	}
	// Re-point f's descriptor number at /dev/null so f.Close() is harmless.
	_ = unix.Dup3(fd, int(f.Fd()), unix.O_CLOEXEC)
	_ = unix.Close(fd)
	_ = f.Close()
}

// TestEngineMetricsCarriesTheHandoffFailureBuckets is the wiring control for
// the four new drop-point counters: each is incremented on a LOOP and must be
// readable through the ENGINE, which is the only surface /debug/vars — and so
// the validation artifact — can see.
func TestEngineMetricsCarriesTheHandoffFailureBuckets(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	e, err := New(resource.Config{
		Addr:      addr,
		Protocol:  engine.HTTP1,
		Resources: resource.Resources{Workers: 2},
	}, stream.HandlerFunc(func(context.Context, *stream.Stream) error { return nil }))
	if err != nil {
		t.Skipf("epoll engine unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- e.Listen(ctx) }()
	t.Cleanup(func() { cancel(); <-errCh })
	for deadline := time.Now().Add(5 * time.Second); e.Addr() == nil && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Fatal("engine did not bind")
	}

	e.mu.Lock()
	loops := append([]*Loop(nil), e.loops...)
	e.mu.Unlock()
	for i, l := range loops {
		if l.transplantHandoffRefused == nil || l.transplantDrainStopped == nil ||
			l.transplantStranded == nil || l.transplantAdoptRefused == nil {
			t.Fatalf("loop %d has an unwired hand-off failure ledger — every "+
				"increment is dropped", i)
		}
		l.transplantHandoffRefused.Add(1)
		l.transplantDrainStopped.Add(2)
		l.transplantStranded.Add(3)
		l.transplantAdoptRefused.Add(4)
	}

	m := e.Metrics()
	n := uint64(len(loops))
	for _, c := range []struct {
		field string
		got   uint64
		want  uint64
	}{
		{"TransplantHandoffRefused", m.TransplantHandoffRefused, n},
		{"TransplantDrainStopped", m.TransplantDrainStopped, 2 * n},
		{"TransplantStranded", m.TransplantStranded, 3 * n},
		{"TransplantAdoptRefused", m.TransplantAdoptRefused, 4 * n},
	} {
		if c.got != c.want {
			t.Errorf("Metrics().%s = %d, want %d", c.field, c.got, c.want)
		}
	}
}
