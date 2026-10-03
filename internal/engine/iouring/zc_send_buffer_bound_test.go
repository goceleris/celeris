//go:build linux

package iouring

import (
	"fmt"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/conn"
)

// What the celeris#812 hold costs while it lasts, and what bounds it
// (celeris#813 round 2). The notification a held send buffer waits for comes
// when the peer takes the unsent tail or the kernel ends the orphaned socket,
// and a peer that keeps reading, however slowly, keeps the socket alive: the
// hold lasts as long as the peer wants. So the hold must cost nothing per loop
// pass, keep no more than the array the kernel reads, show what it keeps, and
// stop growing on its own.

// zcHeldWorker is a Worker with n closed connections, each owing only its
// SEND_ZC's notification, taken past the release backstop, which holds them.
func zcHeldWorker(tb testing.TB, n int) *Worker {
	tb.Helper()
	w := &Worker{conns: make([]*connState, 16), handoffLoss: &handoffLossStats{}}
	for i := range n {
		cs := &connState{fd: 100 + i, generation: uint32(i + 1), sendIsZC: true, sending: true, zcNotifPending: true,
			kernelInflight: 1, sendBuf: make([]byte, 0, 64)}
		w.noteClosedInflight(cs)
		w.queuePendingReleaseFD(cs, false, -1)
	}
	w.cachedNow = time.Now().UnixNano() + 2*pendingReleaseHoldNanos
	w.drainPendingRelease()
	if got := w.handoffLoss.zcNotifHeld.Load(); got != uint64(n) {
		tb.Fatalf("held %d of %d", got, n)
	}
	return w
}

// zcNotifCQE delivers the notification of the SEND_ZC of the closed identity
// (fd, gen) as the loop does, through staleConnCQE.
func zcNotifCQE(t *testing.T, w *Worker, fd int, gen uint32) {
	t.Helper()
	c := &completionEntry{UserData: encodeUserDataGen(udSend, fd, gen), Flags: cqeFNotif}
	if !w.staleConnCQE(c, fd, c.UserData) {
		t.Fatalf("notification of (%d, %d) not taken as stale", fd, gen)
	}
}

// zcHeldGauges reads the held-now gauges.
func zcHeldGauges(w *Worker) (int64, int64) {
	return w.handoffLoss.zcHeldNow.Load(), w.handoffLoss.zcHeldBytes.Load()
}

// TestZCHoldIsOffTheReleaseWalk: the loop walks pendingRelease on every pass
// while it is not empty, and in round 1 a held entry stayed on it, one
// closedOps lookup a pass each, for the whole hold: 94 us a pass at 10,000
// held (the round-2 review's BenchmarkRVBDrainPass). Past the backstop a held
// send buffer must leave the walk at once and wait in zcHolds, where only a
// CQE of its own identity reaches it: the notification releases that hold and
// no other.
func TestZCHoldIsOffTheReleaseWalk(t *testing.T) {
	const n = 3
	w := &Worker{conns: make([]*connState, 16), handoffLoss: &handoffLossStats{}}
	bufs := make([][]byte, n)
	for i := range n {
		bufs[i] = make([]byte, 5000, 8192)
		cs := &connState{fd: 3 + i, generation: uint32(40 + i), sendIsZC: true, sending: true, zcNotifPending: true,
			kernelInflight: 1, sendBuf: bufs[i]}
		w.noteClosedInflight(cs)
		w.queuePendingReleaseFD(cs, false, -1)
	}
	w.cachedNow = w.pendingRelease[n-1].releaseAtNanos + 1
	for pass := range 4 {
		if len(w.pendingRelease) > 0 {
			w.drainPendingRelease()
		}
		if len(w.pendingRelease) != 0 {
			t.Fatalf("pass %d past the backstop: %d held entries are still on the release walk, which every loop pass makes",
				pass, len(w.pendingRelease))
		}
	}
	for i := range n {
		hs := w.zcHolds[encodeConnOpKey(3+i, uint32(40+i))]
		if len(hs) != 1 || unsafe.SliceData(hs[0].sendBuf) != unsafe.SliceData(bufs[i]) || cap(hs[0].sendBuf) != cap(bufs[i]) {
			t.Fatalf("conn %d: its send buffer is not held: %+v", i, hs)
		}
	}
	if held := w.handoffLoss.zcNotifHeld.Load(); held != n {
		t.Fatalf("CloseZCNotifHeld = %d, want %d: one per hold, however many passes", held, n)
	}
	if now, b := zcHeldGauges(w); now != n || b != n*8192 || w.zcHoldBytes != n*8192 || w.zcHoldCount != n {
		t.Fatalf("held-now gauges %d buffers, %d bytes (worker %d, %d), want %d and %d", now, b, w.zcHoldCount, w.zcHoldBytes, n, n*8192)
	}
	// One notification releases its own hold, and only that one.
	zcNotifCQE(t, w, 4, 41)
	if len(w.zcHolds) != n-1 || len(w.zcHolds[encodeConnOpKey(4, 41)]) != 0 {
		t.Fatalf("after conn 1's notification: holds %+v, want conns 0 and 2 only", w.zcHolds)
	}
	if now, b := zcHeldGauges(w); now != n-1 || b != (n-1)*8192 {
		t.Fatalf("held-now gauges %d buffers, %d bytes after one release, want %d and %d", now, b, n-1, (n-1)*8192)
	}
	zcNotifCQE(t, w, 3, 40)
	zcNotifCQE(t, w, 5, 42)
	if len(w.zcHolds) != 0 || len(w.pendingRelease) != 0 || len(w.closedOps) != 0 || w.zcHoldBytes != 0 || w.zcHoldCount != 0 {
		t.Fatalf("after every notification: holds=%d walk=%d identities=%d worker bytes=%d count=%d, want all 0",
			len(w.zcHolds), len(w.pendingRelease), len(w.closedOps), w.zcHoldBytes, w.zcHoldCount)
	}
	if now, b := zcHeldGauges(w); now != 0 || b != 0 {
		t.Fatalf("held-now gauges %d buffers, %d bytes after every release, want 0 and 0", now, b)
	}
	if forced := w.handoffLoss.zcNotifForced.Load(); forced != 0 {
		t.Fatalf("CloseZCNotifForced = %d, want 0", forced)
	}
}

// TestZCHoldKeepsOnlyTheSendBuffer: what the kernel may still read of a closed
// connection that owes only a SEND_ZC is its send buffer's array, and that is
// all the hold may keep; round 1 kept the whole connState (23.9 KB a held
// connection in the round-2 review's load probe, for a 7 KB response). Its
// connState is released at the hold: to the pool without the array (the pool's
// next owner would write into it), or, detached, to the GC untouched (a
// dispatch goroutine may still read it). While another op is owed too (a recv
// the close's cancel has not ended yet, which may still write cs.buf), the
// whole entry is held, and the array alone from that op's CQE on.
func TestZCHoldKeepsOnlyTheSendBuffer(t *testing.T) {
	for _, tc := range []struct {
		name           string
		detached, recv bool
	}{
		{"notif-only", false, false},
		{"detached-notif-only", true, false},
		{"recv-and-notif", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const fd, gen = 9, 31
			key := encodeConnOpKey(fd, gen)
			w := &Worker{conns: make([]*connState, 16), handoffLoss: &handoffLossStats{}}
			arr := make([]byte, 6000, 8192)
			h1 := conn.NewH1State()
			cs := &connState{fd: fd, generation: gen, sendIsZC: true, sending: true, zcNotifPending: true, kernelInflight: 1,
				sendBuf: arr, buf: make([]byte, 4096), writeBuf: make([]byte, 0, 4096), h1State: h1}
			if tc.recv {
				cs.recvArmed = true
				cs.kernelInflight++
			}
			if tc.detached {
				cs.detachMu = new(sync.Mutex)
			}
			w.noteClosedInflight(cs)
			w.queuePendingReleaseFD(cs, tc.detached, -1)
			w.cachedNow = w.pendingRelease[0].releaseAtNanos + 1
			w.drainPendingRelease()
			heldArray := func(when string) zcHold {
				t.Helper()
				hs := w.zcHolds[key]
				if len(w.pendingRelease) != 0 || len(hs) != 1 || unsafe.SliceData(hs[0].sendBuf) != unsafe.SliceData(arr) || cap(hs[0].sendBuf) != cap(arr) {
					t.Fatalf("%s: the send buffer's array is not held off the release walk: walk=%+v holds=%+v", when, w.pendingRelease, hs)
				}
				return hs[0]
			}
			h := heldArray("past the backstop")
			if tc.recv {
				if h.entry.cs != cs || cs.h1State != h1 || w.closedOps[key].conns[0] != cs {
					t.Fatalf("a recv is owed with the notification, and may still write cs.buf: the whole entry must be held")
				}
				c := &completionEntry{UserData: encodeUserDataGen(udRecv, fd, gen), Res: -int32(unix.ECANCELED)}
				if !w.staleConnCQE(c, fd, c.UserData) {
					t.Fatal("recv CQE not taken as stale")
				}
				// The recv's end hands the entry back to the walk, which holds the array alone this time.
				if len(w.pendingRelease) != 1 || len(w.zcHolds[key]) != 0 {
					t.Fatalf("the recv's CQE did not hand the held entry back to the walk: walk=%+v holds=%+v", w.pendingRelease, w.zcHolds)
				}
				w.drainPendingRelease()
				h = heldArray("after the recv's CQE")
			}
			if h.entry.cs != nil {
				t.Fatal("the hold keeps the whole connState although only the SEND_ZC is owed, which reads nothing of it but the array")
			}
			if e := w.closedOps[key]; e == nil || len(e.conns) != 1 || e.conns[0] != nil {
				t.Fatalf("the identity still refers to the released connState: %+v", e)
			}
			if tc.detached {
				if cs.h1State != h1 || cs.fd != fd {
					t.Fatal("a detached connState was reset: it goes to the GC as it is, since a dispatch goroutine may still read it")
				}
			} else if cs.h1State != nil || unsafe.SliceData(cs.sendBuf) == unsafe.SliceData(arr) {
				t.Fatalf("the connState was not released to the pool without its array: h1State=%v sendBuf shares the array=%v",
					cs.h1State != nil, unsafe.SliceData(cs.sendBuf) == unsafe.SliceData(arr))
			}
			if now, b := zcHeldGauges(w); now != 1 || b != int64(cap(arr)) {
				t.Fatalf("held-now gauges %d buffers, %d bytes, want 1 and %d", now, b, cap(arr))
			}
			if held := w.handoffLoss.zcNotifHeld.Load(); held != 1 {
				t.Fatalf("CloseZCNotifHeld = %d, want 1: one hold, whatever it keeps", held)
			}
			zcNotifCQE(t, w, fd, gen)
			if len(w.zcHolds) != 0 || len(w.pendingRelease) != 0 || len(w.closedOps) != 0 {
				t.Fatalf("the notification did not end the hold: holds=%+v walk=%+v identities=%d", w.zcHolds, w.pendingRelease, len(w.closedOps))
			}
			if now, b := zcHeldGauges(w); now != 0 || b != 0 || w.handoffLoss.zcNotifForced.Load() != 0 {
				t.Fatalf("after the notification: held-now %d buffers, %d bytes, CloseZCNotifForced %d; want 0 0 0",
					now, b, w.handoffLoss.zcNotifForced.Load())
			}
		})
	}
}

// TestZCHoldCoversACollidingSibling: two connections closed under one (fd,
// generation) identity, which needs 2^32 accepts during one hold, are
// indistinguishable to the accounting. One owes a SEND_ZC's notification, the
// other a recv and no SEND_ZC. Round 1 decided the hold on each connection's
// own send (cs.sendIsZC): the sibling reached the backstop's release, dropped
// the shared identity (CloseZCNotifForced), and the next pass found no identity
// for the SEND_ZC's connection and pooled it with the array the kernel still
// sent from. The hold is decided on the identity: while it owes a SEND_ZC,
// nothing under it is released, and the notification releases both.
func TestZCHoldCoversACollidingSibling(t *testing.T) {
	const fd, gen = 6, 77
	key := encodeConnOpKey(fd, gen)
	w := &Worker{conns: make([]*connState, 16), handoffLoss: &handoffLossStats{}}
	arr := make([]byte, 6000, 8192)
	sibling := &connState{fd: fd, generation: gen, recvArmed: true, kernelInflight: 1, sendBuf: make([]byte, 0, 4096)}
	zc := &connState{fd: fd, generation: gen, sendIsZC: true, sending: true, zcNotifPending: true, kernelInflight: 1, sendBuf: arr}
	w.noteClosedInflight(sibling)
	w.queuePendingReleaseFD(sibling, false, -1)
	w.noteClosedInflight(zc)
	w.queuePendingReleaseFD(zc, false, -1)
	w.cachedNow = w.pendingRelease[1].releaseAtNanos + 1
	held := func(when string) {
		t.Helper()
		if forced := w.handoffLoss.zcNotifForced.Load(); forced != 0 {
			t.Fatalf("%s: CloseZCNotifForced = %d: the identity was dropped with its SEND_ZC owed", when, forced)
		}
		for _, h := range w.zcHolds[key] {
			if unsafe.SliceData(h.sendBuf) == unsafe.SliceData(arr) {
				return
			}
		}
		t.Fatalf("%s: the array the SEND_ZC still reads is not held: walk=%+v holds=%+v", when, w.pendingRelease, w.zcHolds[key])
	}
	for range 3 {
		if len(w.pendingRelease) > 0 {
			w.drainPendingRelease()
		}
		held("past the backstop")
	}
	c := &completionEntry{UserData: encodeUserDataGen(udRecv, fd, gen), Res: -int32(unix.ECANCELED)}
	if !w.staleConnCQE(c, fd, c.UserData) {
		t.Fatal("recv CQE not taken as stale")
	}
	for range 3 {
		if len(w.pendingRelease) > 0 {
			w.drainPendingRelease()
		}
		held("after the sibling's recv ended")
	}
	zcNotifCQE(t, w, fd, gen)
	if len(w.pendingRelease) > 0 {
		w.drainPendingRelease()
	}
	if len(w.zcHolds) != 0 || len(w.pendingRelease) != 0 || len(w.closedOps) != 0 {
		t.Fatalf("the notification did not release both: holds=%+v walk=%+v identities=%d", w.zcHolds, w.pendingRelease, len(w.closedOps))
	}
	if now, b := zcHeldGauges(w); now != 0 || b != 0 || w.handoffLoss.zcNotifForced.Load() != 0 {
		t.Fatalf("after the notification: held-now %d buffers, %d bytes, CloseZCNotifForced %d; want 0 0 0",
			now, b, w.handoffLoss.zcNotifForced.Load())
	}
}

// TestSendZCStopsWhileHeldBuffersReachTheCap: what bounds the hold. A held
// array lasts as long as the peer keeps its orphaned socket alive, and no
// descriptor or connection slot counts it, so a worker whose held arrays reach
// zcHoldBytesMax arms no new SEND_ZC: its sends copy, and a copied send's
// unsent tail is the kernel's own memory, which its orphan and socket-memory
// limits bound. The held bytes can then grow only by the SEND_ZCs already in
// flight on live connections. SEND_ZC comes back as the holds end.
func TestSendZCStopsWhileHeldBuffersReachTheCap(t *testing.T) {
	opcode := func(w *Worker) (byte, bool) {
		var sqe [sqeSize]byte
		cs := &connState{fd: 7, sendBuf: make([]byte, sendZCMinBytes)}
		w.prepSendSQE(unsafe.Pointer(&sqe[0]), cs, false)
		return sqe[0], cs.sendIsZC
	}
	w := &Worker{sendZC: true, conns: make([]*connState, 16), handoffLoss: &handoffLossStats{}}
	holdOne := func(i int) {
		cs := &connState{fd: 3 + i, generation: uint32(50 + i), sendIsZC: true, sending: true, zcNotifPending: true, kernelInflight: 1,
			sendBuf: make([]byte, sendZCMinBytes, zcHoldBytesMax/2)}
		w.noteClosedInflight(cs)
		w.queuePendingReleaseFD(cs, false, -1)
		w.cachedNow = w.pendingRelease[len(w.pendingRelease)-1].releaseAtNanos + 1
		w.drainPendingRelease()
	}
	holdOne(0)
	if op, zc := opcode(w); op != opSENDZC || !zc {
		t.Fatalf("with %d bytes held, below the cap of %d, an unlinked %d-byte send was armed as op %d, want SEND_ZC",
			w.zcHoldBytes, zcHoldBytesMax, sendZCMinBytes, op)
	}
	holdOne(1)
	if w.zcHoldBytes != zcHoldBytesMax {
		t.Fatalf("the worker counts %d bytes held, want %d (two arrays of %d)", w.zcHoldBytes, zcHoldBytesMax, zcHoldBytesMax/2)
	}
	if op, zc := opcode(w); op != opSEND || zc {
		t.Fatalf("with %d bytes held, at the cap, an unlinked %d-byte send was armed as op %d (sendIsZC %v), want a plain SEND",
			w.zcHoldBytes, sendZCMinBytes, op, zc)
	}
	zcNotifCQE(t, w, 3, 50)
	if op, zc := opcode(w); op != opSENDZC || !zc {
		t.Fatalf("with %d bytes held after one release, an unlinked send was armed as op %d, want SEND_ZC again", w.zcHoldBytes, op)
	}
}

// BenchmarkZCHeldLoopPass is what the release walk costs one loop pass with n
// send buffers held past the backstop: the loop walks pendingRelease only while
// it is not empty, and a held one is not on it. Round 1 walked every held
// entry every pass.
func BenchmarkZCHeldLoopPass(b *testing.B) {
	for _, n := range []int{100, 1000, 10000, 100000} {
		b.Run(fmt.Sprintf("held=%d", n), func(b *testing.B) {
			w := zcHeldWorker(b, n)
			b.ResetTimer()
			for range b.N {
				if len(w.pendingRelease) > 0 {
					w.drainPendingRelease()
				}
			}
			b.StopTimer()
			if got := w.handoffLoss.zcNotifHeld.Load(); got != uint64(n) || w.handoffLoss.zcNotifForced.Load() != 0 {
				b.Fatalf("held %d of %d, forced %d", got, n, w.handoffLoss.zcNotifForced.Load())
			}
		})
	}
}

// BenchmarkClosedIdentityRetireWithZCHolds is what n held send buffers add to
// the CQE path: a closed connection's recv, cancelled at its close, retiring
// its identity through staleConnCQE, as every server-side close with a recv
// armed does. The hold adds one lookup there while any hold exists.
func BenchmarkClosedIdentityRetireWithZCHolds(b *testing.B) {
	for _, n := range []int{0, 10000} {
		b.Run(fmt.Sprintf("held=%d", n), func(b *testing.B) {
			w := zcHeldWorker(b, n)
			const fd, gen = 2, 1 << 31
			cs := &connState{fd: fd, generation: gen, recvArmed: true}
			c := &completionEntry{UserData: encodeUserDataGen(udRecv, fd, gen), Res: -int32(unix.ECANCELED)}
			b.ResetTimer()
			for range b.N {
				cs.kernelInflight = 1
				w.noteClosedInflight(cs)
				w.staleConnCQE(c, fd, c.UserData)
			}
			b.StopTimer()
			if _, ok := w.closedOps[encodeConnOpKey(fd, gen)]; ok {
				b.Fatal("the identity was not retired")
			}
		})
	}
}
