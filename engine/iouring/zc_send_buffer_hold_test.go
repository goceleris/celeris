//go:build linux

package iouring

import (
	"runtime"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// celeris#812: the kernel reads a SEND_ZC's send buffer (cs.sendBuf) until the
// send's notification CQE. A peer that stops reading keeps the unsent tail of
// the send queued on the socket, still referencing those pages, and the
// notification with it: for as long as the socket lives, which after a close
// is as long as the kernel keeps the orphaned socket trying to deliver that
// tail. The pendingRelease backstop released such a connState 5 s after the
// close, to connStatePool with its sendBuf (a detached one to the GC). The
// next occupant's response went into that array, and when the peer read
// again, the orphaned socket sent the tail from it: another connection's
// bytes.

// zcStallPastBackstop is how long the peer stays stalled with the backstop
// already due: about 30 loop passes, every one of which would release a
// connState the backstop does not hold.
const zcStallPastBackstop = 300 * time.Millisecond

// zcBackstopCase is one case of the backstop tests: what closes, and how.
type zcBackstopCase struct {
	name                 string
	reap, recv, detached bool
}

// zcBackstopCases are the ways a connection is closed with a SEND_ZC
// notification owed:
//
//   - notif-only: the send's first CQE was read before the close; only the
//     notification is owed.
//   - recv-and-notif: a recv was armed after the send too; the close's
//     cancel ends it, and the notification is left.
//   - send-done-after-close: the send's first CQE was still in the ring,
//     unread, at the close.
//   - detached-notif-only: notif-only on a detached connection (detachMu),
//     which the release drops for the GC instead of pooling.
var zcBackstopCases = []zcBackstopCase{
	{"notif-only", true, false, false},
	{"recv-and-notif", true, true, false},
	{"send-done-after-close", false, false, false},
	{"detached-notif-only", true, false, true},
}

// zcBackstopResult is what one case measured.
type zcBackstopResult struct {
	w                *Worker
	sent             int32
	got              int  // bytes the peer read
	eof              bool // the peer read to EOF
	corrupt, first   int  // bytes that are not what was sent, and the first one's offset (-1: none)
	heldPastBackstop bool
	releasedAfter    time.Duration // from the close to the array's release; -1: not released
	owedAtRelease    int32         // cs.kernelInflight at the release: 0 once every op retired; a closed conn's keeps its close-time value until then
}

// runZCBackstopCase builds the case with the #798 fixture: a 64 KiB SEND_ZC to
// a loopback peer with a 4 KiB receive buffer that reads nothing, closed as
// the closing-drain sweep closes it. The backstop is then made due at once
// (the entry's releaseAtNanos, set to the close: the 5 s wait itself is not
// what is tested), and the loop runs for zcStallPastBackstop with the peer
// still stalled. The moment the array the SEND_ZC was given stops being held
// (the connState leaves pendingRelease, or stays there without the array),
// the test does what the array's next owner does and writes over it (0xAA).
// Then the peer reads to EOF while the loop runs, and every byte it read is
// compared with the byte that was sent (byte(i)).
func runZCBackstopCase(t *testing.T, tc zcBackstopCase) zcBackstopResult {
	t.Helper()
	w, cs, peer := newZCCloseWorker(t)
	if tc.detached {
		cs.detachMu = new(sync.Mutex)
	}
	r := zcBackstopResult{w: w, first: -1, releasedAfter: -1, owedAtRelease: -1}
	r.sent = startZCSend(t, w, cs, tc.reap)
	pinned := cs.sendBuf[:cap(cs.sendBuf)]
	if len(cs.sendBuf) != zcPayload || pinned[1] != 1 || pinned[255] != 255 {
		t.Fatalf("sendBuf is not the payload: len=%d", len(cs.sendBuf))
	}
	if tc.recv {
		if !w.prepareRecv(cs, cs.buf) || cs.kernelInflight != 2 {
			t.Fatalf("recv not placed: kernelInflight=%d", cs.kernelInflight)
		}
		if res := runRingOnce(t, w, 10*time.Millisecond); len(res) != 0 {
			t.Fatalf("the recv completed before the close (%v): the peer sent nothing", res)
		}
	}
	sweepClose(t, w, cs)
	if len(w.pendingRelease) != 1 || w.pendingRelease[0].cs != cs || w.pendingRelease[0].detached != tc.detached {
		t.Fatalf("the close did not queue the connState for release as expected: %+v", w.pendingRelease)
	}
	// The backstop is due from here on.
	closedAt := time.Now()
	w.pendingRelease[0].releaseAtNanos = closedAt.UnixNano()

	// held: the array is still the send buffer of a connState queued for
	// release. Anything else, the connState released or a queued one no
	// longer holding the array, gives the array to a next owner.
	held := func() bool {
		for i := range w.pendingRelease {
			if w.pendingRelease[i].cs == cs && unsafe.SliceData(cs.sendBuf) == unsafe.SliceData(pinned) {
				return true
			}
		}
		return false
	}
	pass := func(d time.Duration) {
		runRingOnce(t, w, d)
		owed := cs.kernelInflight
		w.cachedNow = time.Now().UnixNano()
		w.drainPendingRelease()
		if r.releasedAfter < 0 && !held() {
			r.releasedAfter = time.Since(closedAt)
			r.owedAtRelease = owed
			// The next owner of the array writes into it.
			for i := range pinned {
				pinned[i] = 0xAA
			}
		}
	}
	for time.Since(closedAt) < zcStallPastBackstop {
		pass(10 * time.Millisecond)
	}
	r.heldPastBackstop = r.releasedAfter < 0

	if err := unix.SetNonblock(peer, true); err != nil {
		t.Fatalf("peer nonblock: %v", err)
	}
	got := make([]byte, 0, zcPayload)
	buf := make([]byte, 64<<10)
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end) && (!r.eof || r.releasedAfter < 0); {
		for !r.eof {
			n, err := unix.Read(peer, buf)
			if n > 0 {
				got = append(got, buf[:n]...)
				continue
			}
			if n == 0 && err == nil {
				r.eof = true
			}
			break
		}
		pass(5 * time.Millisecond)
	}
	r.got = len(got)
	for i, b := range got {
		if b != byte(i) {
			r.corrupt++
			if r.first < 0 {
				r.first = i
			}
		}
	}
	t.Logf("celeris812 backstop case=%s sent=%d held_past_backstop=%v released_after=%v owed_at_release=%d peer_got=%d eof=%v corrupt_bytes=%d first_corrupt=%d",
		tc.name, r.sent, r.heldPastBackstop, r.releasedAfter.Round(time.Microsecond), r.owedAtRelease, r.got, r.eof, r.corrupt, r.first)
	return r
}

// TestBackstopHoldsASendBufferAZCNotificationStillReads is celeris#812: in
// every case of zcBackstopCases the backstop must hold the connState, and so
// its send buffer, while the notification is owed, however long past the
// backstop, and release it once the notification arrives. A release before
// the notification puts the 0xAA on the wire: the unsent tail, some 61 KB, in
// every case. Held for good, the connState would be a leak.
func TestBackstopHoldsASendBufferAZCNotificationStillReads(t *testing.T) {
	for _, tc := range zcBackstopCases {
		t.Run(tc.name, func(t *testing.T) {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			r := runZCBackstopCase(t, tc)
			if r.corrupt != 0 {
				t.Errorf("CORRUPT: %d of the %d bytes the peer read are not what was sent (first at offset %d): "+
					"the connState, and the send buffer the kernel was still sending from, left pendingRelease "+
					"%v after the close with %d op(s) owed", r.corrupt, r.got, r.first, r.releasedAfter, r.owedAtRelease)
			}
			if !r.heldPastBackstop {
				t.Errorf("the backstop released the connState %v after the close with %d op(s) owed: a SEND_ZC "+
					"notification was still owed, so the kernel could still read its send buffer", r.releasedAfter, r.owedAtRelease)
			}
			if r.got != int(r.sent) || !r.eof {
				t.Errorf("the peer read %d bytes (EOF %v), want the %d the send completed with and then EOF", r.got, r.eof, r.sent)
			}
			if r.releasedAfter < 0 || r.owedAtRelease != 0 {
				t.Errorf("the connState was not released at the notification: released_after=%v owed_at_release=%d "+
					"pendingRelease=%d", r.releasedAfter, r.owedAtRelease, len(r.w.pendingRelease))
			}
		})
	}
}
