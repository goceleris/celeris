//go:build linux

package iouring

import (
	"bytes"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// celeris#988: a socket whose struct socket lacks SOCK_SUPPORT_ZC makes
// io_send_zc return -EOPNOTSUPP at issue time (io_uring/net.c), before it sends
// a byte. io_sendrecv_fail adds IORING_CQE_F_MORE because the notification was
// allocated at prep, so the engine sees res=-95|F_MORE and then F_NOTIF. The
// engine used to read that pair as a broken connection (only ENOMEM and EINVAL
// had a fallback) and closed the conn with its response unsent.
//
// An AF_UNIX socket never carries SOCK_SUPPORT_ZC, so a socketpair produces
// the exact CQE pair on any SEND_ZC kernel, with no MPTCP and no memlock
// pressure. On the bare-metal cluster the same socket shape comes from a TCP
// conn accepted through Go's default MPTCP listener (the plain-TCP fallback
// accept does not copy the bit); see adopt_nonzc_socket_988_linux_test.go.

// reap988 submits r and returns every CQE it produces until one without
// F_MORE has been seen (a plain SEND or a NOTIF), or d elapses.
func reap988(t *testing.T, r *Ring, d time.Duration) []completionEntry {
	t.Helper()
	var got []completionEntry
	for dl := time.Now().Add(d); time.Now().Before(dl); {
		if err := r.SubmitAndWaitTimeout(50 * time.Millisecond); err != nil {
			t.Fatalf("submit: %v", err)
		}
		h, tl := r.BeginCQ()
		done := false
		for ; h != tl; h++ {
			c := *r.cqeAt(h)
			got = append(got, c)
			if c.Flags&cqeFMore == 0 {
				done = true
			}
		}
		r.EndCQ(tl)
		if done {
			return got
		}
	}
	return got
}

// TestSendZCNonZCSocketShape988 characterises the kernel, so the tests below
// can be trusted to model it: SEND_ZC on a socket without SOCK_SUPPORT_ZC
// completes res=-EOPNOTSUPP with F_MORE, then a NOTIF. It passes on any kernel
// that has the opcode and rejects SEND_ZC on AF_UNIX; it skips on a kernel
// without the opcode, and on one that accepts SEND_ZC on AF_UNIX.
func TestSendZCNonZCSocketShape988(t *testing.T) {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Skipf("socketpair: %v", err)
	}
	defer func() { _ = unix.Close(pair[0]) }()
	defer func() { _ = unix.Close(pair[1]) }()
	r := newTestRing(t)
	sqe := r.GetSQE()
	prepSendZC(sqe, pair[0], make([]byte, sendZCMinBytes), false)
	setSQEUserData(sqe, 7)
	got := reap988(t, r, 2*time.Second)
	t.Logf("CQEs: %+v", got)
	if len(got) >= 1 && got[0].Res == -int32(unix.EINVAL) {
		t.Skip("kernel has no SEND_ZC")
	}
	if len(got) >= 1 && got[0].Res != -int32(unix.EOPNOTSUPP) {
		t.Skip("this kernel supports SEND_ZC on AF_UNIX; the non-ZC socket shape needs another socket type")
	}
	if len(got) != 2 || got[0].Flags&cqeFMore == 0 || got[1].Flags&cqeFNotif == 0 {
		t.Fatalf("want [res=-95 F_MORE, F_NOTIF], got %+v", got)
	}
}

// assertResentPlain988 checks the state after an EOPNOTSUPP SEND_ZC on z: the
// conn is open, nothing was reported, its bytes are back in flight as a plain
// SEND, and SEND_ZC is still on for the worker.
func assertResentPlain988(t *testing.T, w *Worker, z *zcConn) {
	t.Helper()
	if errs := z.failures(); len(errs) != 0 {
		t.Errorf("an EOPNOTSUPP SEND_ZC was reported to middleware as %v", errs)
	}
	if w.errs.Total() != 0 {
		t.Errorf("an EOPNOTSUPP SEND_ZC counted %d engine error(s)", w.errs.Total())
	}
	cs := w.conns[z.fd]
	if cs == nil {
		t.Fatalf("celeris#988: a SEND_ZC that completed -EOPNOTSUPP (socket without SOCK_SUPPORT_ZC) closed its conn; its bytes were never sent")
	}
	if !cs.sending || cs.sendIsZC || len(cs.sendBuf) != sendZCMinBytes {
		t.Fatalf("celeris#988: the bytes were not re-armed as a plain SEND (sending=%v sendIsZC=%v sendBuf=%d)", cs.sending, cs.sendIsZC, len(cs.sendBuf))
	}
	if !w.sendZC {
		t.Errorf("celeris#988: one socket without SOCK_SUPPORT_ZC retired SEND_ZC for the whole worker")
	}
}

// TestSendZCEOPNOTSUPPKeepsConn988 (injected CQEs, the shape the cluster
// logged): a SEND_ZC that fails EOPNOTSUPP must re-issue its bytes as a plain
// SEND on that conn, without OnError, without closing, and without retiring
// SEND_ZC for the worker's other conns.
func TestSendZCEOPNOTSUPPKeepsConn988(t *testing.T) {
	w, conns := newZCFallbackWorker(t, 2)
	z, sib := conns[0], conns[1]
	z.armZCSend(t, w)
	z.completeZCNotified(w, unix.EOPNOTSUPP)
	assertResentPlain988(t, w, z)
	sib.armZCSend(t, w) // fatal if the sibling's send no longer takes the ZC arm
}

// TestSendZCEOPNOTSUPPWithoutNotificationKeepsConn988 is the same failure
// delivered as a lone CQE with F_MORE clear: handleSend's branch rather than
// completeSend's.
func TestSendZCEOPNOTSUPPWithoutNotificationKeepsConn988(t *testing.T) {
	w, conns := newZCFallbackWorker(t, 2)
	z, sib := conns[0], conns[1]
	z.armZCSend(t, w)
	z.completeZCUnnotified(w, unix.EOPNOTSUPP)
	assertResentPlain988(t, w, z)
	sib.armZCSend(t, w)
}

// TestSendZCEOPNOTSUPPRealCompletions988: no injection. The conn's SEND_ZC is
// submitted on a real AF_UNIX socketpair, the kernel's own CQEs are fed to
// handleSend, and the plain re-send must then drain.
func TestSendZCEOPNOTSUPPRealCompletions988(t *testing.T) {
	w, conns := newZCFallbackWorker(t, 1)
	z := conns[0]
	z.armZCSend(t, w)
	got := reap988(t, w.ring, 2*time.Second)
	t.Logf("kernel CQEs for the ZC send: %+v", got)
	if len(got) >= 1 && got[0].Res == -int32(unix.EINVAL) {
		t.Skip("kernel has no SEND_ZC")
	}
	for i := range got {
		w.handleSend(&got[i], z.fd, w.cachedNow)
	}
	assertResentPlain988(t, w, z)
	for _, c := range reap988(t, w.ring, 2*time.Second) {
		w.handleSend(&c, z.fd, w.cachedNow)
	}
	if cs := w.conns[z.fd]; cs == nil || cs.sending || len(cs.sendBuf) != 0 {
		t.Fatalf("the plain re-send did not drain (cs=%v)", cs != nil)
	}
}

// TestSendZCEOPNOTSUPPIsPerConn988: after the fallback the conn's later large
// sends stay plain (no second wasted SEND_ZC round trip), a sibling's large
// send still takes the ZC arm, and a failure of the plain re-send is a real
// error: one retry per conn, never a loop.
func TestSendZCEOPNOTSUPPIsPerConn988(t *testing.T) {
	w, conns := newZCFallbackWorker(t, 2)
	z, sib := conns[0], conns[1]
	z.armZCSend(t, w)
	z.completeZCNotified(w, unix.EOPNOTSUPP)
	assertResentPlain988(t, w, z)

	// The re-send completes in full, then another large response is queued.
	w.handleSend(&completionEntry{Res: sendZCMinBytes}, z.fd, w.cachedNow)
	if z.cs.sending || len(z.cs.sendBuf) != 0 {
		t.Fatalf("the completed re-send left sending=%v sendBuf=%d", z.cs.sending, len(z.cs.sendBuf))
	}
	z.cs.detachMu.Lock()
	z.cs.writeBuf = append(z.cs.writeBuf[:0], bytes.Repeat([]byte{'n'}, 4*sendZCMinBytes)...)
	sqFull := w.flushSend(z.cs)
	z.cs.detachMu.Unlock()
	if sqFull || !z.cs.sending {
		t.Fatalf("the second large send was not armed (sqFull=%v sending=%v)", sqFull, z.cs.sending)
	}
	if z.cs.sendIsZC {
		t.Errorf("celeris#988: a conn that rejected SEND_ZC armed SEND_ZC again for its next large send")
	}

	sib.armZCSend(t, w)
	if !sib.cs.sendIsZC {
		t.Errorf("celeris#988: the sibling lost SEND_ZC")
	}

	// The plain re-send failing is not a ZC problem: the generic path closes.
	w.handleSend(&completionEntry{Res: -int32(unix.EPIPE)}, z.fd, w.cachedNow)
	if errs := z.failures(); len(errs) != 1 {
		t.Errorf("a failing plain SEND after the fallback reported %d error(s), want 1", len(errs))
	}
}
