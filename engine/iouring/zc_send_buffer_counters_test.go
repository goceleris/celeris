//go:build linux

package iouring

import (
	"runtime"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The accounting and the counters behind the celeris#812 hold (see
// zc_send_buffer.go). TestBackstopHoldsASendBufferAZCNotificationStillReads
// judges the hold by what reaches the wire; these judge what the engine
// records about it, and the shutdown half, which the wire cannot show.

// TestBackstopCountsItsZCHolds runs every wire case again and reads the
// counters: one hold (CloseZCNotifHeld) per case, however many passes the
// backstop ran past its deadline, no send buffer given up with its SEND_ZC
// owed (CloseZCNotifForced, which must stay 0), and no descriptor forced
// (CloseFDForced): a notification alone names none (celeris#798).
func TestBackstopCountsItsZCHolds(t *testing.T) {
	for _, tc := range zcBackstopCases {
		t.Run(tc.name, func(t *testing.T) {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			r := runZCBackstopCase(t, tc)
			held := r.w.handoffLoss.zcNotifHeld.Load()
			forced := r.w.handoffLoss.zcNotifForced.Load()
			fdForced := r.w.handoffLoss.closeFDForced.Load()
			t.Logf("celeris812 counters case=%s close_zc_notif_held=%d close_zc_notif_forced=%d close_fd_forced=%d", tc.name, held, forced, fdForced)
			if held != 1 || forced != 0 || fdForced != 0 {
				t.Errorf("CloseZCNotifHeld=%d CloseZCNotifForced=%d CloseFDForced=%d, want 1 0 0", held, forced, fdForced)
			}
			if r.corrupt != 0 || !r.heldPastBackstop || r.owedAtRelease != 0 {
				t.Errorf("the hold itself failed: corrupt=%d held_past_backstop=%v owed_at_release=%d", r.corrupt, r.heldPastBackstop, r.owedAtRelease)
			}
		})
	}
}

// TestPendingReleaseBackstopHoldsAZCSendPastItsDeadline is the hold's own
// decision on hand-built state, with no kernel, so it runs where SEND_ZC does
// not (the wire test skips there). Past its deadline, on every pass, an entry
// that still owes a SEND_ZC stays queued, counted once (CloseZCNotifHeld),
// and the notification releases it; nothing is forced (CloseZCNotifForced).
//
//   - notif-owed: the send's first CQE came before the close; the descriptor
//     went at the close (only the notification is owed, celeris#798).
//   - send-owed-fd-kept: the send itself was owed at the close, so the entry
//     kept the descriptor for it (celeris#685). Past the backstop the
//     descriptor goes, forced and counted as the backstop always did
//     (CloseFDForced), while the connState stays for the SEND_ZC; the send's
//     first CQE and then its notification end the hold.
func TestPendingReleaseBackstopHoldsAZCSendPastItsDeadline(t *testing.T) {
	for _, tc := range []struct {
		name     string
		keepFD   bool
		cqes     []uint32 // flags of the send's CQEs after the backstop, in order
		fdForced uint64
	}{
		{"notif-owed", false, []uint32{cqeFNotif}, 0},
		{"send-owed-fd-kept", true, []uint32{cqeFMore, cqeFNotif}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const fd, gen = 5, 7
			w := &Worker{conns: make([]*connState, 16), handoffLoss: &handoffLossStats{}}
			cs := &connState{fd: fd, generation: gen, sendIsZC: true, sending: true, zcNotifPending: !tc.keepFD, kernelInflight: 1}
			w.noteClosedInflight(cs)
			kept := -1
			if tc.keepFD {
				efd, err := unix.Eventfd(0, unix.EFD_CLOEXEC)
				if err != nil {
					t.Fatalf("eventfd: %v", err)
				}
				kept = efd
			}
			w.queuePendingReleaseFD(cs, false, kept)
			deadline := w.pendingRelease[0].releaseAtNanos
			for i := range 5 {
				w.cachedNow = deadline + int64(i+1)*int64(time.Second)
				w.drainPendingRelease()
				if len(w.pendingRelease) != 1 || w.pendingRelease[0].cs != cs || !w.pendingRelease[0].zcHeld {
					t.Fatalf("pass %d past the backstop: the entry was not held for its SEND_ZC: %+v", i, w.pendingRelease)
				}
				if w.pendingRelease[0].holdsFD || w.closeFDOwed != 0 {
					t.Fatalf("pass %d past the backstop: the descriptor is still kept (closeFDOwed=%d)", i, w.closeFDOwed)
				}
			}
			if held, forced, fdForced := w.handoffLoss.zcNotifHeld.Load(), w.handoffLoss.zcNotifForced.Load(), w.handoffLoss.closeFDForced.Load(); held != 1 || forced != 0 || fdForced != tc.fdForced {
				t.Fatalf("CloseZCNotifHeld=%d CloseZCNotifForced=%d CloseFDForced=%d, want 1 0 %d", held, forced, fdForced, tc.fdForced)
			}
			for i, flags := range tc.cqes {
				if len(w.pendingRelease) != 1 {
					t.Fatalf("released before CQE %d (flags %#x)", i, flags)
				}
				c := &completionEntry{UserData: encodeUserDataGen(udSend, fd, gen), Res: 100, Flags: flags}
				if !w.staleConnCQE(c, fd, c.UserData) {
					t.Fatalf("CQE %d not taken as stale", i)
				}
				w.drainPendingRelease()
			}
			if len(w.pendingRelease) != 0 {
				t.Fatalf("the notification did not release the entry: %+v", w.pendingRelease)
			}
			if forced := w.handoffLoss.zcNotifForced.Load(); forced != 0 {
				t.Fatalf("CloseZCNotifForced = %d, want 0", forced)
			}
		})
	}
}

// TestClosedOpsCountsTheZCSendApart drives the closedOps accounting with
// completions built by hand, so each way a SEND_ZC can end is checked without
// a kernel: zcOwed must be 1 from the close until the op's terminal CQE, and
// only that CQE may clear it.
//
//   - notif-owed: closed after the send's first CQE; the notification ends it.
//   - send-then-notif: closed with the send itself owed; its first CQE
//     (IORING_CQE_F_MORE) leaves the notification owed, which ends it.
//   - send-without-notif: a SEND_ZC whose first CQE came without F_MORE has
//     no notification to follow; that CQE ends it.
//   - recv-and-notif: a recv's terminal CQE must not end it.
//   - plain-send: a plain SEND owes no SEND_ZC at all.
//   - collision: two conns under one (fd, generation), one owing a SEND_ZC's
//     notification and one a plain send: the plain send's CQE cannot be told
//     from a SEND_ZC's first CQE without F_MORE, so it must not end the hold;
//     the notification does.
func TestClosedOpsCountsTheZCSendApart(t *testing.T) {
	const fd, gen = 7, 21
	sendUD := encodeUserDataGen(udSend, fd, gen)
	recvUD := encodeUserDataGen(udRecv, fd, gen)
	first := &completionEntry{UserData: sendUD, Res: 100, Flags: cqeFMore}
	notif := &completionEntry{UserData: sendUD, Flags: cqeFNotif}
	plain := &completionEntry{UserData: sendUD, Res: 100}
	recvEnd := &completionEntry{UserData: recvUD, Res: -int32(unix.ECANCELED)}
	type conn struct{ zc, sending, notifPending, recv bool }
	for _, tc := range []struct {
		name   string
		conns  []conn
		cqes   []*completionEntry
		owed   []uint8 // zcOwed after the close, then after each CQE
		closed bool    // the identity retired after the last CQE
	}{
		{"notif-owed", []conn{{zc: true, sending: true, notifPending: true}}, []*completionEntry{notif}, []uint8{1, 0}, true},
		{"send-then-notif", []conn{{zc: true, sending: true}}, []*completionEntry{first, notif}, []uint8{1, 1, 0}, true},
		{"send-without-notif", []conn{{zc: true, sending: true}}, []*completionEntry{plain}, []uint8{1, 0}, true},
		{"recv-and-notif", []conn{{zc: true, sending: true, notifPending: true, recv: true}}, []*completionEntry{recvEnd, notif}, []uint8{1, 1, 0}, true},
		{"plain-send", []conn{{sending: true}}, []*completionEntry{plain}, []uint8{0, 0}, true},
		{"collision", []conn{{zc: true, sending: true, notifPending: true}, {sending: true}}, []*completionEntry{plain, notif}, []uint8{1, 1, 0}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &Worker{conns: make([]*connState, fd+1), handoffLoss: &handoffLossStats{}}
			var css []*connState
			for _, c := range tc.conns {
				cs := &connState{fd: fd, generation: gen, sendIsZC: c.zc, sending: c.sending, zcNotifPending: c.notifPending, recvArmed: c.recv}
				if c.sending || c.notifPending {
					cs.kernelInflight++
				}
				if c.recv {
					cs.kernelInflight++
				}
				w.noteClosedInflight(cs)
				css = append(css, cs)
			}
			key := encodeConnOpKey(fd, gen)
			owed := func() uint8 {
				if e := w.closedOps[key]; e != nil {
					return e.zcOwed
				}
				return 0
			}
			if got := owed(); got != tc.owed[0] {
				t.Fatalf("zcOwed = %d after the close, want %d", got, tc.owed[0])
			}
			if got, want := w.closedZCOwed(css[0]), tc.owed[0] > 0; got != want {
				t.Fatalf("closedZCOwed = %v after the close, want %v", got, want)
			}
			for i, c := range tc.cqes {
				if !w.staleConnCQE(c, fd, c.UserData) {
					t.Fatalf("CQE %d was not taken as stale", i)
				}
				if got := owed(); got != tc.owed[i+1] {
					t.Fatalf("zcOwed = %d after CQE %d (ud=%#x flags=%#x), want %d", got, i, c.UserData, c.Flags, tc.owed[i+1])
				}
			}
			if _, ok := w.closedOps[key]; ok == tc.closed {
				t.Fatalf("identity still registered = %v after the last CQE, want %v", ok, !tc.closed)
			}
			for _, cs := range css {
				if w.closedZCOwed(cs) {
					t.Fatalf("closedZCOwed still true once every op was retired")
				}
			}
		})
	}
}

// TestDroppingAnIdentityWithAZCOwedIsCounted is the positive control of the
// must-stay-0 counter: dropping the accounting of a closed conn that still
// owes a SEND_ZC, as the backstop does for any entry it gives up on, is a send
// buffer released with the kernel still able to send from it, and must be
// counted (CloseZCNotifForced). Dropping one that owes only a recv must not.
func TestDroppingAnIdentityWithAZCOwedIsCounted(t *testing.T) {
	for _, tc := range []struct {
		name string
		zc   bool
		want uint64
	}{
		{"zc-owed", true, 1},
		{"recv-only", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &Worker{handoffLoss: &handoffLossStats{}}
			cs := &connState{fd: 9, generation: 5, recvArmed: true, kernelInflight: 1}
			if tc.zc {
				cs.sendIsZC, cs.zcNotifPending = true, true
				cs.kernelInflight++
			}
			w.noteClosedInflight(cs)
			w.dropClosedOps(cs)
			if got := w.handoffLoss.zcNotifForced.Load(); got != tc.want {
				t.Fatalf("CloseZCNotifForced = %d, want %d", got, tc.want)
			}
			if len(w.closedOps) != 0 {
				t.Fatalf("the identity is still registered after dropClosedOps")
			}
		})
	}
}

// zcRetainedSince reports whether the array behind buf went into zcRetained
// after its first n entries, and how many entries were added.
func zcRetainedSince(n int, buf []byte) (found bool, added int) {
	zcRetained.mu.Lock()
	defer zcRetained.mu.Unlock()
	for _, b := range zcRetained.bufs[n:] {
		if unsafe.SliceData(b) == unsafe.SliceData(buf) {
			found = true
		}
	}
	return found, len(zcRetained.bufs) - n
}

func zcRetainedLen() int {
	zcRetained.mu.Lock()
	defer zcRetained.mu.Unlock()
	return len(zcRetained.bufs)
}

// TestShutdownRetainsSendBuffersAZCMayStillRead is the shutdown half of
// celeris#812. Closing the ring ends no SEND_ZC's use of its send buffer, and
// after it no notification can say when that use ends, so worker shutdown
// must keep, for the life of the process, every send buffer a SEND_ZC may
// still read, and only those. Run on a synthetic worker right where shutdown
// runs it (retainZCSendBufsAtShutdown, before the ring closes):
//
//   - live-notif-owed: a live connection whose peer stopped reading owes its
//     notification: its sendBuf array must be retained and counted.
//   - live-notif-in-ring: the peer read everything and the notification is in
//     the completion ring, not yet read: the retention reads it first, and
//     nothing is retained.
//   - held-after-close: a closed connection the backstop holds for its
//     notification (the #812 hold): retained and counted.
func TestShutdownRetainsSendBuffersAZCMayStillRead(t *testing.T) {
	for _, tc := range []struct {
		name        string
		peerReads   bool
		closeFirst  bool
		wantRetain  bool
		wantCounted uint64
	}{
		{"live-notif-owed", false, false, true, 1},
		{"live-notif-in-ring", true, false, false, 0},
		{"held-after-close", false, true, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			w, cs, peer := newZCCloseWorker(t)
			sent := startZCSend(t, w, cs, true)
			buf := cs.sendBuf
			if tc.peerReads {
				if err := unix.SetNonblock(peer, true); err != nil {
					t.Fatal(err)
				}
				got, rb := 0, make([]byte, 64<<10)
				for end := time.Now().Add(3 * time.Second); got < int(sent) && time.Now().Before(end); {
					if n, _ := unix.Read(peer, rb); n > 0 {
						got += n
						continue
					}
					time.Sleep(time.Millisecond)
				}
				if got != int(sent) {
					t.Fatalf("the peer read %d of %d", got, sent)
				}
				for end := time.Now().Add(3 * time.Second); ; {
					if head, tail := w.ring.BeginCQ(); tail != head {
						break
					}
					if time.Now().After(end) {
						t.Fatal("no notification in the ring 3s after the peer read everything")
					}
					time.Sleep(time.Millisecond)
				}
			}
			if tc.closeFirst {
				sweepClose(t, w, cs)
				w.pendingRelease[0].releaseAtNanos = time.Now().UnixNano()
				for range 3 {
					runRingOnce(t, w, 10*time.Millisecond)
					w.cachedNow = time.Now().UnixNano()
					w.drainPendingRelease()
				}
				if len(w.pendingRelease) != 1 || !w.pendingRelease[0].zcHeld {
					t.Fatalf("the backstop did not hold the entry: %+v", w.pendingRelease)
				}
			}
			n := zcRetainedLen()
			w.retainZCSendBufsAtShutdown()
			found, added := zcRetainedSince(n, buf)
			counted := w.handoffLoss.zcBufRetained.Load()
			t.Logf("celeris812 shutdown case=%s sent=%d retained=%v added=%d shutdown_zc_buf_retained=%d kernelInflight=%d",
				tc.name, sent, found, added, counted, cs.kernelInflight)
			if found != tc.wantRetain || added != int(tc.wantCounted) || counted != tc.wantCounted {
				t.Fatalf("retained=%v added=%d ShutdownZCBufRetained=%d, want %v %d %d", found, added, counted,
					tc.wantRetain, tc.wantCounted, tc.wantCounted)
			}
		})
	}
}
