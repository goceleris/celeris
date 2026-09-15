//go:build linux

package iouring

import (
	"bytes"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine/internal/errclass"
	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/resource"
)

// celeris#609 regression guard, with no dependence on memory pressure.
//
// SEND_ZC pins the user buffer against RLIMIT_MEMLOCK, so a host with a low
// limit starts returning ENOMEM once enough sends are in flight. That is a
// resource shortage, not a broken connection, and the engine has always had a
// fallback for it: retire the opcode on this worker and re-issue the bytes as
// a plain SEND. The fallback was gated on w.sendZC — the flag its own body
// clears — so it fired exactly ONCE per worker. Every zero-copy send already
// in flight behind that first one arrived to find the flag false, fell through
// to the generic negative-result path, and was reported to middleware as
// OnError(-ENOMEM) and then closed.
//
// Provoking that needs a memlock low enough for the kernel to actually run
// out, which is why it only ever showed up under the WebSocket oracle's heavy
// defaults. These tests inject the completions instead: two connections each
// arm a genuine SEND_ZC SQE, and both are then completed with the errno. The
// first is the one the old guard handled; the SECOND is the regression.
//
// Deliberately written against no field that post-dates the fix, so it
// compiles unchanged on the pre-fix tree and can be used as its own negative
// control.

// zcConn is one connection in the fixture: the worker-side fd, its connState,
// and every error the engine handed to middleware for it.
type zcConn struct {
	fd int
	cs *connState

	mu   sync.Mutex
	errs []error
}

func (z *zcConn) note(err error) {
	z.mu.Lock()
	z.errs = append(z.errs, err)
	z.mu.Unlock()
}

func (z *zcConn) failures() []error {
	z.mu.Lock()
	defer z.mu.Unlock()
	return append([]error(nil), z.errs...)
}

// armZCSend queues a payload at the zero-copy threshold and flushes it, so
// prepSendSQE takes its SEND_ZC arm and the connection has a real zero-copy
// send outstanding. Asserts the ZC arm was actually the one taken —
// useSendZC is prepSendSQE's own branch condition — because a fixture that
// silently armed a plain SEND would make every assertion below vacuous.
func (z *zcConn) armZCSend(t *testing.T, w *Worker) {
	t.Helper()
	if !w.sendZC {
		t.Fatalf("fd %d: worker has already retired SEND_ZC; nothing to arm", z.fd)
	}
	z.cs.detachMu.Lock()
	z.cs.writeBuf = append(z.cs.writeBuf[:0], bytes.Repeat([]byte{'z'}, sendZCMinBytes)...)
	sqFull := w.flushSend(z.cs)
	z.cs.detachMu.Unlock()
	if sqFull {
		t.Fatalf("fd %d: SQ ring full while arming the zero-copy send", z.fd)
	}
	if !z.cs.sending {
		t.Fatalf("fd %d: flushSend armed no send at all", z.fd)
	}
	if !useSendZC(w.sendZC, false, len(z.cs.sendBuf)) {
		t.Fatalf("fd %d: flushSend armed a plain SEND (%d bytes, threshold %d); "+
			"the fixture is not exercising the zero-copy path",
			z.fd, len(z.cs.sendBuf), sendZCMinBytes)
	}
}

// completeZCNotified delivers the CQE pair the kernel produces for a SEND_ZC
// it accepted and then failed: the first carries the result together with
// IORING_CQE_F_MORE (the notification is still coming, so the engine must not
// touch the send buffer yet), the second is that notification. This is the
// shape the field reports show — the warning they carry is logged from
// completeSend, which is only reachable through the notification.
func (z *zcConn) completeZCNotified(w *Worker, errno unix.Errno) {
	w.handleSend(&completionEntry{Res: -int32(errno), Flags: cqeFMore}, z.fd, w.cachedNow)
	w.handleSend(&completionEntry{Res: 0, Flags: cqeFNotif}, z.fd, w.cachedNow)
}

// completeZCUnnotified delivers the single CQE the kernel produces for a
// SEND_ZC it rejected outright: no F_MORE, so no notification follows. This
// is handleSend's own fallback branch rather than completeSend's.
func (z *zcConn) completeZCUnnotified(w *Worker, errno unix.Errno) {
	w.handleSend(&completionEntry{Res: -int32(errno), Flags: 0}, z.fd, w.cachedNow)
}

// newZCFallbackWorker builds a worker with SEND_ZC available and n detached
// connections on real socketpair fds, each recording what the engine reports
// to middleware. Real fds matter: the close path this test must observe NOT
// happening does close the descriptor.
func newZCFallbackWorker(t *testing.T, n int) (*Worker, []*zcConn) {
	t.Helper()
	ring := newTestRing(t)

	locals := make([]int, 0, n)
	maxFD := 0
	for range n {
		pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
		if err != nil {
			t.Skipf("socketpair unavailable: %v", err)
		}
		peer := pair[1]
		t.Cleanup(func() { _ = unix.Close(peer) })
		locals = append(locals, pair[0])
		if pair[0] > maxFD {
			maxFD = pair[0]
		}
	}

	w := &Worker{
		ring:        ring,
		conns:       make([]*connState, maxFD+1),
		liveConns:   make([]int, 0, n),
		errs:        &errclass.Counters{},
		activeConns: &atomic.Int64{},
		closeCount:  &atomic.Uint64{},
		cfg:         resource.Config{IdleTimeout: time.Second},
		sendZC:      true,
		// A real logger, discarded: the fallback logs a warning, and the
		// pre-fix tree calls w.logger.Warn without a nil check. Leaving it
		// nil would make this test panic there instead of failing its
		// assertion, which would make it useless as its own negative
		// control.
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	w.cachedNow = time.Now().UnixNano()

	out := make([]*zcConn, 0, n)
	for _, fd := range locals {
		z := &zcConn{fd: fd}
		h1 := &conn.H1State{OnError: z.note}
		h1.Detached.Store(true)
		cs := &connState{
			fd:         fd,
			liveIdx:    -1,
			generation: 1,
			detachMu:   &sync.Mutex{},
			h1State:    h1,
		}
		z.cs = cs
		w.conns[fd] = cs
		w.addLiveConn(cs)
		w.connCount++
		w.activeConns.Add(1)
		out = append(out, z)
	}
	return w, out
}

// runSiblingCase is the shared body: two connections each with a zero-copy
// send in flight, the first completed with errno (which retires the opcode
// for the worker), then the second completed with the same errno. The second
// connection must survive.
func runSiblingCase(t *testing.T, errno unix.Errno, complete func(*zcConn, *Worker, unix.Errno)) {
	t.Helper()
	w, conns := newZCFallbackWorker(t, 2)
	first, sibling := conns[0], conns[1]

	// Both sends are armed BEFORE any completion, which is the whole
	// situation: by the time the sibling's completion arrives the worker has
	// already retired the opcode, but the sibling's SQE was submitted while
	// it was still on.
	first.armZCSend(t, w)
	sibling.armZCSend(t, w)

	complete(first, w, errno)

	// The first completion is the one the pre-fix guard already handled, so
	// these three are preconditions rather than the regression. Reported
	// before the w.sendZC check because a tree that does not recognise this
	// errno at all fails here, and "the first one was closed" is the useful
	// message — "the opcode was not retired" is a consequence of it.
	if errs := first.failures(); len(errs) != 0 {
		t.Fatalf("the FIRST %v completion was reported to middleware as %v", errno, errs)
	}
	if w.conns[first.fd] == nil {
		t.Fatalf("the FIRST %v completion closed its own connection; this errno "+
			"has no fallback on this tree at all", errno)
	}
	if w.sendZC {
		t.Fatalf("a %v completion did not retire SEND_ZC on the worker; "+
			"the fixture never reached the state under test", errno)
	}

	errsBefore := w.errs.Total()
	complete(sibling, w, errno)

	if errs := sibling.failures(); len(errs) != 0 {
		t.Errorf("celeris#609: a zero-copy send that completed with %v AFTER the "+
			"worker retired SEND_ZC was reported to middleware as %v. Its SQE was "+
			"armed while zero-copy was still on, so it is the same transient "+
			"shortage the first completion absorbed — classify on the send's own "+
			"provenance, not on the worker's forward-looking w.sendZC flag",
			errno, errs)
	}
	if got := w.errs.Total() - errsBefore; got != 0 {
		t.Errorf("celeris#609: the sibling's %v completion counted %d engine error(s); "+
			"a fallback is not an error", errno, got)
	}
	survivor := w.conns[sibling.fd]
	if survivor == nil {
		// Report and stop: everything below inspects the connection that
		// this branch just established no longer exists.
		t.Fatalf("celeris#609: a zero-copy send that completed with %v AFTER the "+
			"worker retired SEND_ZC tore its connection down. A transient resource "+
			"shortage must not close a healthy connection", errno)
	}
	if !survivor.sending {
		t.Errorf("celeris#609: the sibling's bytes were never re-issued as a plain "+
			"SEND after the %v fallback (sending=false, sendBuf=%d, writeBuf=%d)",
			errno, len(survivor.sendBuf), len(survivor.writeBuf))
	}
}

// TestSendZCENOMEMAfterFallbackKeepsSiblingOpen is the primary guard: the
// notification path (completeSend), which is the one the field reports reach.
func TestSendZCENOMEMAfterFallbackKeepsSiblingOpen(t *testing.T) {
	runSiblingCase(t, unix.ENOMEM, (*zcConn).completeZCNotified)
}

// TestSendZCEINVALAfterFallbackKeepsSiblingOpen covers the other errno the
// fallback exists for. EINVAL means the kernel will not run the opcode, which
// is a reason to stop issuing it and re-send the bytes plainly — never a
// reason to fail the connection. completeSend did not handle EINVAL at all
// before the fix, so this is a strictly wider guard than the ENOMEM case.
func TestSendZCEINVALAfterFallbackKeepsSiblingOpen(t *testing.T) {
	runSiblingCase(t, unix.EINVAL, (*zcConn).completeZCNotified)
}

// TestSendZCENOMEMWithoutNotificationKeepsSiblingOpen covers handleSend's own
// fallback branch: a SEND_ZC the kernel rejected before committing to a
// notification, so the failure arrives as a lone CQE with F_MORE clear. Same
// defect, same fix, a different one of the two sites.
func TestSendZCENOMEMWithoutNotificationKeepsSiblingOpen(t *testing.T) {
	runSiblingCase(t, unix.ENOMEM, (*zcConn).completeZCUnnotified)
}

// TestPlainSendErrorStillFailsTheConnection is the negative control for the
// fix itself: classifying on the send's provenance must not turn a genuine
// plain-SEND failure into a silent retry. A connection whose in-flight send
// was never zero-copy still gets the error and still closes.
func TestPlainSendErrorStillFailsTheConnection(t *testing.T) {
	w, conns := newZCFallbackWorker(t, 1)
	z := conns[0]
	w.sendZC = false // no zero-copy anywhere: this is an ordinary SEND

	z.cs.detachMu.Lock()
	z.cs.writeBuf = append(z.cs.writeBuf[:0], bytes.Repeat([]byte{'p'}, sendZCMinBytes)...)
	sqFull := w.flushSend(z.cs)
	z.cs.detachMu.Unlock()
	if sqFull || !z.cs.sending {
		t.Fatalf("flushSend armed no plain send (sqFull=%t sending=%t)", sqFull, z.cs.sending)
	}

	w.handleSend(&completionEntry{Res: -int32(unix.ENOMEM), Flags: 0}, z.fd, w.cachedNow)

	if errs := z.failures(); len(errs) == 0 {
		t.Error("a plain SEND that failed with ENOMEM was silently swallowed; " +
			"only zero-copy sends have a fallback to retry into")
	}
	if w.errs.Send.Load() == 0 {
		t.Error("a plain SEND failure was not counted in the celeris#645 send bucket")
	}
}
