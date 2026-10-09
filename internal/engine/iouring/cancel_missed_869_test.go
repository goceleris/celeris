//go:build linux

package iouring

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// celeris#869: getCancelSQE returns nil when the SQ ring is still full after a
// submit, and cancelConnOps then proceeds without the cancel of an armed recv.
// The recv stays armed, and the two paths that depended on the cancel went on
// as if it had been placed:
//
//   - a hijack handed the socket to the handler with the recv still reading
//     it: the multishot recv stays armed across its request, and takes the
//     hijacker's first bytes (celeris#685);
//   - a close path queued the connState for release, and the 5 s backstop
//     released it with the recv still owed (the connState to the pool or, if
//     detached, to the garbage collector), where the kernel writes the recv's
//     bytes into its buffer (cs.buf, for a single-shot recv).
//
// cancelSQEFull is the seam that makes getCancelSQE report that full ring.

func newMultishotFixture869(t *testing.T) *fdlFixture {
	t.Helper()
	f := newFDLFixture(t, false)
	br, err := NewBufferRing(f.w.ring, bufRingGroupID, 8, 4096)
	if err != nil {
		skipOrFail656(t, "provided-buffer ring unavailable: %v", err)
	}
	t.Cleanup(func() { br.Close(f.w.ring) })
	f.w.bufRing = br
	return f
}

// TestHijackRefusedWhenItsMultishotRecvCannotBeCancelled: a hijack that
// cannot place the cancel of the recv still armed on the socket must refuse,
// leaving the conn as it was, and succeed once the ring has room.
func TestHijackRefusedWhenItsMultishotRecvCannotBeCancelled(t *testing.T) {
	f := newMultishotFixture869(t)
	w, cs := f.w, f.cs
	if !w.prepareRecv(cs, cs.buf) || !cs.recvArmed || cs.kernelInflight != 1 {
		t.Fatalf("multishot recv not armed: armed=%v kernelInflight=%d", cs.recvArmed, cs.kernelInflight)
	}
	if _, err := w.ring.Submit(); err != nil {
		t.Fatalf("submit: %v", err)
	}
	w.cancelSQEFull = func() bool { return true }

	c, err := w.hijackConn(f.fd)
	if err == nil {
		defer func() { _ = c.Close() }()
		// What the handler would do next: the client sends, the hijacker reads.
		const payload = "first bytes of the hijacked session\n"
		if _, werr := unix.Write(f.peer, []byte(payload)); werr != nil {
			t.Fatalf("peer write: %v", werr)
		}
		got := make([]byte, 64)
		type rd struct {
			n   int
			err error
		}
		ch := make(chan rd, 1)
		go func() { n, rerr := c.Read(got); ch <- rd{n, rerr} }()
		var r rd
		select {
		case r = <-ch:
		case <-time.After(500 * time.Millisecond):
			r = rd{-1, nil}
		}
		t.Fatalf("hijackConn handed the socket to the handler with its multishot recv still armed and not cancelled "+
			"(no SQE): the hijacker's first read returned n=%d err=%v of %d bytes the client sent; the recv takes them (celeris#869)",
			r.n, r.err, len(payload))
	}
	t.Logf("celeris869 HIJACK refused: %v", err)
	if w.conns[f.fd] != cs || !cs.recvArmed || cs.closing || w.connCount != 1 {
		t.Errorf("a refused hijack changed the conn: registered=%v recvArmed=%v closing=%v connCount=%d",
			w.conns[f.fd] == cs, cs.recvArmed, cs.closing, w.connCount)
	}
	if !fdOpen704(f.fd) {
		t.Errorf("fd %d closed by a refused hijack", f.fd)
	}
	if len(w.pendingRelease) != 0 {
		t.Errorf("a refused hijack queued %d release(s)", len(w.pendingRelease))
	}

	// The ring has room again: the same hijack goes through, and the
	// hijacker reads what the client sends next.
	w.cancelSQEFull = nil
	c, err = w.hijackConn(f.fd)
	if err != nil {
		t.Fatalf("hijack with room in the ring: %v", err)
	}
	defer func() { _ = c.Close() }()
	const payload = "first bytes of the hijacked session\n"
	if _, werr := unix.Write(f.peer, []byte(payload)); werr != nil {
		t.Fatalf("peer write: %v", werr)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := make([]byte, 64)
	n, rerr := c.Read(got)
	if rerr != nil || string(got[:n]) != payload {
		t.Errorf("hijacker read %q, %v; want %q", got[:n], rerr, payload)
	}
}

// TestHijackWithNothingToCancelNeedsNoSQE is the control: a single-shot
// hijack finds no recv armed (the recv that brought the request has
// completed), so it has nothing to cancel and a full ring does not stop it.
func TestHijackWithNothingToCancelNeedsNoSQE(t *testing.T) {
	f := newFDLFixture(t, false)
	w, cs := f.w, f.cs
	if cs.recvArmed || cs.kernelInflight != 0 {
		t.Fatalf("fixture has an op in flight: recvArmed=%v kernelInflight=%d", cs.recvArmed, cs.kernelInflight)
	}
	w.cancelSQEFull = func() bool { return true }
	c, err := w.hijackConn(f.fd)
	if err != nil {
		t.Fatalf("hijack with nothing to cancel was refused: %v", err)
	}
	_ = c.Close()
}

// TestBackstopHoldsAConnStateWhoseCancelWasNeverPlaced: a close path that
// could not place the cancel of the recv it leaves armed must not let the
// backstop release the connState while that recv is owed. It retries the
// cancel on the passes that follow, holds the connState (and the descriptor)
// until the recv's terminal completion, and releases it then.
func TestBackstopHoldsAConnStateWhoseCancelWasNeverPlaced(t *testing.T) {
	for _, tc := range []struct {
		name      string
		multishot bool
	}{
		{"single-shot", false},
		{"multishot", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f *fdlFixture
			if tc.multishot {
				f = newMultishotFixture869(t)
			} else {
				f = newFDLFixture(t, false)
			}
			w, cs := f.w, f.cs
			f.armFirstRecv()
			if !cs.recvArmed || cs.kernelInflight != 1 {
				t.Fatalf("recv not armed: armed=%v kernelInflight=%d", cs.recvArmed, cs.kernelInflight)
			}
			recvUD := encodeUserDataGen(udRecv, f.fd, f.gen)

			full := true
			w.cancelSQEFull = func() bool { return full }
			w.closeConn(f.fd)
			if w.conns[f.fd] != nil || len(w.pendingRelease) != 1 || !w.pendingRelease[0].holdsFD {
				t.Fatalf("the close did not queue the connState with its descriptor: slot=%p pendingRelease=%d", w.conns[f.fd], len(w.pendingRelease))
			}
			for _, r := range takeSQEs(w.ring) {
				if r.op == opASYNCCANCEL {
					t.Fatalf("a cancel was placed (%v) although the ring was full: the seam does not work", r)
				}
			}

			// The backstop fires with the recv still owed and its cancel never placed.
			w.pendingRelease[0].releaseAtNanos = 1
			w.cachedNow = time.Now().UnixNano()
			w.drainPendingRelease()
			held := len(w.pendingRelease)
			t.Logf("celeris869 BACKSTOP arm=%s pendingRelease_after_backstop=%d fd_open=%v kernelInflight=%d CloseFDForced=%d",
				tc.name, held, fdOpen704(f.fd), cs.kernelInflight, w.handoffLoss.closeFDForced.Load())
			if held != 1 {
				t.Fatalf("the backstop released a connState whose recv is still owed and was never cancelled "+
					"(pendingRelease=%d, kernelInflight=%d): the kernel can still write into it", held, cs.kernelInflight)
			}
			if !fdOpen704(f.fd) {
				t.Errorf("the descriptor was closed past the backstop with the recv owed")
			}

			// The ring has room: the next pass places the cancel.
			full = false
			w.drainPendingRelease()
			var cancels []sqeRec
			for _, r := range takeSQEs(w.ring) {
				if r.op == opASYNCCANCEL {
					cancels = append(cancels, r)
				}
			}
			if len(cancels) != 1 || cancels[0].addr != recvUD {
				t.Fatalf("after the ring had room the cancel of the recv was not placed: %v (want one aimed at %#x)", cancels, recvUD)
			}
			if len(w.pendingRelease) != 1 {
				t.Fatalf("pendingRelease = %d after the retry, want the entry still held until the recv's terminal completion", len(w.pendingRelease))
			}

			// The recv ends (-ECANCELED): the connState and the descriptor go.
			c := &completionEntry{UserData: recvUD, Res: -int32(unix.ECANCELED)}
			w.staleConnCQE(c, f.fd, recvUD)
			w.cachedNow = time.Now().UnixNano()
			w.drainPendingRelease()
			if len(w.pendingRelease) != 0 || fdOpen704(f.fd) || w.closeFDOwed != 0 {
				t.Errorf("after the terminal completion: pendingRelease=%d fd_open=%v closeFDOwed=%d, want 0 false 0",
					len(w.pendingRelease), fdOpen704(f.fd), w.closeFDOwed)
			}
			if forced := w.handoffLoss.closeFDForced.Load(); forced != 0 {
				t.Errorf("CloseFDForced = %d, want 0", forced)
			}
		})
	}
}

// TestBackstopKeepsTheDescriptorOfARecvWhoseCancelWasNeverPlacedBesideASendZC:
// the conn also owes a SEND_ZC's notification (closedZCOwed). The backstop's
// rule for that, hold the send buffer until the notification and give up the
// descriptor (holdZCPastBackstop, CloseFDForced), is for a descriptor only the
// SEND_ZC's own ops name. A recv whose cancel was never placed names it too:
// closing it under that recv is the celeris#685 hazard, and taking the entry
// off the walk stops the cancel's retries.
func TestBackstopKeepsTheDescriptorOfARecvWhoseCancelWasNeverPlacedBesideASendZC(t *testing.T) {
	f := newFDLFixture(t, false)
	w, cs := f.w, f.cs
	f.armFirstRecv()
	if !cs.recvArmed || cs.kernelInflight != 1 {
		t.Fatalf("recv not armed: armed=%v kernelInflight=%d", cs.recvArmed, cs.kernelInflight)
	}
	recvUD := encodeUserDataGen(udRecv, f.fd, f.gen)
	// A SEND_ZC whose first completion has been applied: only its notification is owed.
	cs.sendIsZC, cs.zcNotifPending, cs.zcSentBytes = true, true, 1
	cs.kernelInflight = 2

	full := true
	w.cancelSQEFull = func() bool { return full }
	cs.closing = true
	w.finishCloseAny(f.fd, cs)
	if w.conns[f.fd] != nil || len(w.pendingRelease) != 1 || !w.pendingRelease[0].holdsFD || !cs.cancelMissed {
		t.Fatalf("the close did not queue the connState with its descriptor and the missed cancel: slot=%p pendingRelease=%d cancelMissed=%v",
			w.conns[f.fd], len(w.pendingRelease), cs.cancelMissed)
	}
	if !w.closedZCOwed(cs) {
		t.Fatalf("the identity does not owe the SEND_ZC: the case did not form")
	}
	takeSQEs(w.ring)

	w.pendingRelease[0].releaseAtNanos = 1
	w.cachedNow = time.Now().UnixNano()
	w.drainPendingRelease()
	t.Logf("celeris869 ORDER pendingRelease=%d zcHolds=%d fd_open=%v CloseFDForced=%d cancelMissed=%v",
		len(w.pendingRelease), len(w.zcHolds), fdOpen704(f.fd), w.handoffLoss.closeFDForced.Load(), cs.cancelMissed)
	if forced := w.handoffLoss.closeFDForced.Load(); forced != 0 || !fdOpen704(f.fd) {
		t.Errorf("the backstop closed the descriptor (CloseFDForced=%d, open=%v) under a recv whose cancel was never placed", forced, fdOpen704(f.fd))
	}
	if len(w.pendingRelease) != 1 || len(w.zcHolds) != 0 {
		t.Fatalf("the entry left the walk (pendingRelease=%d, zcHolds=%d): the missed cancel is no longer retried", len(w.pendingRelease), len(w.zcHolds))
	}

	// The ring has room: the next pass places the recv's cancel.
	full = false
	w.drainPendingRelease()
	var cancelled bool
	for _, r := range takeSQEs(w.ring) {
		if r.op == opASYNCCANCEL && r.addr == recvUD {
			cancelled = true
		}
	}
	if !cancelled || cs.cancelMissed {
		t.Errorf("after the ring had room the recv's cancel was not placed (placed=%v, cancelMissed=%v)", cancelled, cs.cancelMissed)
	}
}

// hijackProbe869 hijacks on /hj, and answers 503 when the hijack is refused,
// as a handler that checks Hijack's error does.
type hijackProbe869 struct {
	err  error
	conn net.Conn
}

func (h *hijackProbe869) HandleStream(_ context.Context, s *stream.Stream) error {
	hj, ok := s.ResponseWriter.(stream.Hijacker)
	if !ok {
		return errors.New("no Hijacker")
	}
	c, err := hj.Hijack(s)
	if err != nil {
		h.err = err
		return s.ResponseWriter.WriteResponse(s, 503,
			[][2]string{{"content-length", "7"}}, []byte("refused"))
	}
	h.conn = c
	return nil
}

// deliverMultishot869 completes the armed multishot recv with req, received
// into provided buffer id (the recv stays armed: F_MORE).
func deliverMultishot869(f *fdlFixture, id uint16, req string) {
	buf := f.w.bufRing.GetBuffer(id, len(req))
	copy(buf, req)
	f.process(&completionEntry{
		UserData: encodeUserDataGen(udRecv, f.fd, f.gen),
		Res:      int32(len(req)),
		Flags:    cqeFBuffer | cqeFMore | uint32(id)<<16,
	})
}

// TestRefusedHijackAnswersTheRequestAndKeepsServing runs the refusal through
// ProcessH1, as a request takes it: Hijack returns the error, the handler
// answers 503, the response goes out, the request's buffer goes back to the
// ring as any other (it is not retired), and the connection serves the next
// request, whose Hijack goes through once the ring has room (that buffer is
// retired, celeris#868).
func TestRefusedHijackAnswersTheRequestAndKeepsServing(t *testing.T) {
	f := newMultishotFixture869(t)
	w, cs := f.w, f.cs
	h := &hijackProbe869{}
	w.handler = h
	if !w.prepareRecv(cs, cs.buf) || !cs.recvArmed {
		t.Fatal("multishot recv not armed")
	}
	_ = takeSQEs(w.ring) // the kernel is not involved: every completion here is the test's
	const req = "GET /hj HTTP/1.1\r\nHost: x\r\n\r\n"

	w.cancelSQEFull = func() bool { return true }
	deliverMultishot869(f, 3, req)
	sqes := takeSQEs(w.ring)
	var sends int
	for _, r := range sqes {
		if r.op == opSEND {
			sends++
		}
	}
	t.Logf("celeris869 PROCESSH1 refused_err=%v registered=%v retired=%d sends=%d sendBuf_has_503=%v",
		h.err, w.conns[f.fd] == cs, w.bufRing.Retired(), sends, strings.Contains(string(cs.sendBuf), "503"))
	if h.err == nil || !strings.Contains(h.err.Error(), "submission queue is full") {
		t.Fatalf("Hijack error = %v, want the refusal for a full submission queue (a hijack with its recv still armed was let through)", h.err)
	}
	if w.conns[f.fd] != cs || cs.closing || !cs.recvArmed {
		t.Errorf("the refused hijack left the conn unserviceable: registered=%v closing=%v recvArmed=%v", w.conns[f.fd] == cs, cs.closing, cs.recvArmed)
	}
	if sends != 1 || !strings.Contains(string(cs.sendBuf), "503") {
		t.Errorf("the handler's 503 was not sent: SEND SQEs=%d sendBuf=%q", sends, cs.sendBuf)
	}
	if w.bufRing.Retired() != 0 {
		t.Errorf("the refused hijack retired %d buffer(s): the request's buffer belongs to the ring", w.bufRing.Retired())
	}
	f.process(f.sendCQE())
	_ = takeSQEs(w.ring)

	w.cancelSQEFull = nil
	h.err = nil
	deliverMultishot869(f, 4, req)
	if h.err != nil || h.conn == nil {
		t.Fatalf("the next request's Hijack, with room in the ring: conn=%v err=%v", h.conn, h.err)
	}
	defer func() { _ = h.conn.Close() }()
	if w.conns[f.fd] != nil || w.bufRing.Retired() != 1 {
		t.Errorf("after the hijack: registered=%v retired=%d, want gone and 1", w.conns[f.fd] != nil, w.bufRing.Retired())
	}
}
