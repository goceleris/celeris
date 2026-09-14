//go:build linux

package epoll

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// The #383 transplant hand-off moves a connection between two engines without
// firing a single lifecycle hook: the source drops it from its live gauge with
// no OnDisconnect (the conn is not ending, it is moving) and the target adds it
// with no OnConnect. celeris#624 is an adaptive cell whose live gauge lost two
// connections with no hook movement at all, and the artifact could not tell an
// unpaired hand-off from a close that skipped its hook, because neither half of
// the hand-off was counted anywhere.
//
// These tests pin the epoll half of that ledger: the detach that fires no hook,
// the adopt that pairs with it, the occupied-slot refusal that silently drops
// one, and the wiring that carries all three out through Metrics().

// newLedgerLoop builds a bare Loop with a real epoll fd and the #383 ledger
// counters wired to fresh atomics, mirroring what Engine.Listen does per loop.
func newLedgerLoop(t *testing.T) *Loop {
	t.Helper()
	epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		t.Skipf("epoll_create1 unavailable: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(epfd) })
	return &Loop{
		epollFD:                epfd,
		conns:                  make([]*connState, 4096),
		liveConns:              make([]int, 0, 8),
		activeConns:            &atomic.Int64{},
		closeCount:             &atomic.Uint64{},
		acceptCount:            &atomic.Uint64{},
		errCount:               &atomic.Uint64{},
		bytesRead:              &atomic.Uint64{},
		bytesWritten:           &atomic.Uint64{},
		transplantAdopted:      &atomic.Uint64{},
		transplantDetached:     &atomic.Uint64{},
		transplantSlotOccupied: &atomic.Uint64{},
		resolved:               resource.ResolvedResources{BufferSize: 4096},
		cfg:                    resource.Config{},
	}
}

// socketpairFD returns one end of a connected AF_UNIX socketpair, small enough
// to land inside the loop's conn table, with the peer closed on cleanup.
func socketpairFD(t *testing.T, l *Loop) int {
	t.Helper()
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Skipf("socketpair unavailable: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(pair[1]) })
	if pair[0] >= len(l.conns) {
		_ = unix.Close(pair[0])
		t.Skipf("socketpair fd %d outside the test conn table", pair[0])
	}
	return pair[0]
}

// TestDetachFromEpollCountsTheDetach is the source half. detachFromEpoll is the
// ONLY epoll path that drops a conn from the live gauge without firing
// OnDisconnect, so its counter is the only record that the decrement happened
// at all. Asserts the gauge and the ledger move together, and that nothing
// mistook the detach for a close.
func TestDetachFromEpollCountsTheDetach(t *testing.T) {
	l := newLedgerLoop(t)
	fd := socketpairFD(t, l)
	t.Cleanup(func() { _ = unix.Close(fd) })
	cs := regLive(l, fd)
	l.connCount++
	l.activeConns.Add(1)

	l.detachFromEpoll(fd, cs)

	if got := l.transplantDetached.Load(); got != 1 {
		t.Errorf("transplantDetached = %d, want 1 — the hook-free live-gauge "+
			"decrement is unrecorded, which is exactly the celeris#624 blind spot", got)
	}
	if got := l.activeConns.Load(); got != 0 {
		t.Errorf("activeConns = %d, want 0", got)
	}
	// A detach is not a close: closeCount must not move, or the residual
	// engine_closed - hook_closed would name hypothesis (B) for a hand-off.
	if got := l.closeCount.Load(); got != 0 {
		t.Errorf("closeCount = %d, want 0 (a transplant detach is not a close)", got)
	}
	if l.conns[fd] != nil {
		t.Error("conn table slot still occupied after detach")
	}
}

// TestAttachAdoptedFDCountsTheAdopt is the target half of the reverse
// (io_uring→epoll) direction: a real connected fd the engine never accepted is
// adopted, so the live gauge rises with no OnConnect. TransplantAdopted must
// rise on the same statement, since TransplantDetached - TransplantAdopted is
// the residual that names a lost hand-off.
func TestAttachAdoptedFDCountsTheAdopt(t *testing.T) {
	l := newLedgerLoop(t)
	fd := socketpairFD(t, l)

	l.attachAdoptedFD(context.Background(), fd, engine.Carryover{RemoteAddr: "127.0.0.1:1"}, time.Now().UnixNano())

	if l.conns[fd] == nil {
		t.Fatalf("fd %d was not adopted (errCount=%d) — test setup, not the counter",
			fd, l.errCount.Load())
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	if got := l.transplantAdopted.Load(); got != 1 {
		t.Errorf("transplantAdopted = %d, want 1 — the hook-free live-gauge "+
			"increment is unrecorded, so a hand-off cannot be paired", got)
	}
	if got := l.activeConns.Load(); got != 1 {
		t.Errorf("activeConns = %d, want 1", got)
	}
}

// TestAttachAdoptedFDCountsAnOccupiedSlot pins the silent drop point
// celeris#624 names: the slot is already taken, so the adopt is refused, the
// descriptor is deliberately NOT closed (the slot holder may close that number
// later) and no hook fires. Before this counter the branch was visible only as
// a +1 on the generic ErrorCount every other error path shares.
func TestAttachAdoptedFDCountsAnOccupiedSlot(t *testing.T) {
	l := newLedgerLoop(t)
	fd := socketpairFD(t, l)
	t.Cleanup(func() { _ = unix.Close(fd) })
	occupant := regLive(l, fd)

	l.attachAdoptedFD(context.Background(), fd, engine.Carryover{}, time.Now().UnixNano())

	if got := l.transplantSlotOccupied.Load(); got != 1 {
		t.Errorf("transplantSlotOccupied = %d, want 1 — the refusal is still "+
			"indistinguishable from every other ErrorCount bump", got)
	}
	if got := l.errCount.Load(); got != 1 {
		t.Errorf("errCount = %d, want 1 (the generic counter must still move)", got)
	}
	if l.conns[fd] != occupant {
		t.Error("the occupied slot was clobbered")
	}
	if got := l.transplantAdopted.Load(); got != 0 {
		t.Errorf("transplantAdopted = %d, want 0 — a refused adopt must not "+
			"pair with a detach", got)
	}
}

// TestEngineMetricsCarriesTheTransplantLedger is the wiring control: a counter
// incremented on a LOOP must be readable through the ENGINE's Metrics(). It
// fails if Listen stops handing the loops the engine-wide atomics, or if
// Metrics() stops reporting any of the three fields.
func TestEngineMetricsCarriesTheTransplantLedger(t *testing.T) {
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
	for deadline := time.Now().Add(3 * time.Second); e.Addr() == nil && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Fatal("engine did not bind")
	}

	e.mu.Lock()
	loops := append([]*Loop(nil), e.loops...)
	e.mu.Unlock()
	if len(loops) == 0 {
		t.Fatal("no loops")
	}
	// Bump once per loop through the LOOP's pointer: a per-loop counter that
	// was never wired to the engine (or wired to a copy) reads back short.
	for _, l := range loops {
		if l.transplantAdopted == nil || l.transplantDetached == nil || l.transplantSlotOccupied == nil {
			t.Fatal("loop has an unwired transplant ledger — every increment is dropped")
		}
		l.transplantAdopted.Add(1)
		l.transplantDetached.Add(2)
		l.transplantSlotOccupied.Add(3)
	}

	m := e.Metrics()
	n := uint64(len(loops))
	if m.TransplantAdopted != n {
		t.Errorf("Metrics().TransplantAdopted = %d, want %d", m.TransplantAdopted, n)
	}
	if m.TransplantDetached != 2*n {
		t.Errorf("Metrics().TransplantDetached = %d, want %d", m.TransplantDetached, 2*n)
	}
	if m.TransplantAdoptSlotOccupied != 3*n {
		t.Errorf("Metrics().TransplantAdoptSlotOccupied = %d, want %d", m.TransplantAdoptSlotOccupied, 3*n)
	}
	// The epoll engine never runs the io_uring-only silent-close path.
	if m.CloseMissingConnState != 0 {
		t.Errorf("Metrics().CloseMissingConnState = %d, want 0 on epoll", m.CloseMissingConnState)
	}
}
