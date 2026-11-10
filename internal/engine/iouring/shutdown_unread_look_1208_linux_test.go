//go:build linux

package iouring

import (
	"testing"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
)

// celeris#1208: a worker with nothing to send, on its first look at the shutdown,
// asks the kernel for a request it has not read before it exits
// (sendDrainDone, h1UnreadInKernel), inside the 250 ms floor only, and only at
// the conns that had one when its drain began (markUnreadAtShutdown, celeris#1229).
// These tests pin
// the parts that the end-to-end cells (test/server, B-hold-other and
// B-accept-hold) cannot: the completion-queue half of the look, which on a kernel
// whose completions are task work (DEFER_TASKRUN, COOP_TASKRUN) is never the one
// that finds the request (its bytes are still in the socket), and only a base-tier
// ring, or any ring that posts at once, reaches; the count of a conn the gate has
// marked; and the floor. They arm the recv themselves, so they do not depend on
// the ring's tier, and the base-tier CI job (which runs this whole package) runs
// them as well.

const req1208 = "GET /x HTTP/1.1\r\nHost: x\r\n\r\n"

// done1229 is sendDrainDone's first result.
func done1229(w *Worker, now int64) bool {
	done, _ := w.sendDrainDone(now)
	return done
}

func inq1208(t *testing.T, fd int) int {
	t.Helper()
	q, err := unix.IoctlGetInt(fd, unix.TIOCINQ)
	if err != nil {
		t.Fatalf("TIOCINQ: %v", err)
	}
	return q
}

// look1229 is the worker's look as it makes it: the snapshot of the first pass
// of the drain (markUnreadAtShutdown), then the question (h1UnreadInKernel). The
// subtests of TestGateH1UnreadInKernel1208 set their conns up first, so the
// snapshot sees them as the first pass sees the conns the worker has.
func look1229(w *Worker) bool {
	w.markUnreadAtShutdown()
	return w.h1UnreadInKernel()
}

func TestGateH1UnreadInKernel1208(t *testing.T) {
	t.Run("an idle conn holds nothing", func(t *testing.T) {
		w, _, _ := gateRig884(t, 2)
		if look1229(w) {
			t.Errorf("h1UnreadInKernel: true for conns with nothing in the kernel")
		}
	})
	t.Run("bytes in the socket queue", func(t *testing.T) {
		w, _, peers := gateRig884(t, 2)
		if _, err := unix.Write(peers[1], []byte(req1208)); err != nil {
			t.Fatal(err)
		}
		if !look1229(w) {
			t.Errorf("h1UnreadInKernel: false with a request in a socket's queue")
		}
	})
	t.Run("a recv CQE with bytes, unreaped, the socket queue empty (the CQ half)", func(t *testing.T) {
		w, css, peers := gateRig884(t, 2)
		armPostedRecv1104(t, w, css[0], peers[0], req1208)
		if q := inq1208(t, css[0].fd); q != 0 {
			t.Fatalf("premise: %d bytes still in the socket's queue after the recv completed", q)
		}
		if !look1229(w) {
			t.Errorf("h1UnreadInKernel: false with the request a posted recv CQE (the base tier's shape)")
		}
		if css[0].gateRead {
			t.Errorf("the look marked the conn (gateRead): it must not, the gate's pass marks")
		}
		if !css[0].shutdownUnread {
			t.Errorf("the snapshot did not mark the conn whose request is a posted recv CQE")
		}
	})
	t.Run("a recv CQE that is an EOF", func(t *testing.T) {
		w, css, peers := gateRig884(t, 1)
		armPostedRecv1104(t, w, css[0], peers[0], "")
		if look1229(w) {
			t.Errorf("h1UnreadInKernel: true for a posted recv CQE with no bytes (a FIN is not a request)")
		}
	})
	t.Run("a recv CQE of another generation", func(t *testing.T) {
		w, css, peers := gateRig884(t, 1)
		armPostedRecv1104(t, w, css[0], peers[0], req1208)
		css[0].generation++
		if look1229(w) {
			t.Errorf("h1UnreadInKernel: true on the strength of a CQE of another generation")
		}
	})
	t.Run("a recv CQE of an HTTP/2 conn", func(t *testing.T) {
		w, css, peers := gateRig884(t, 1)
		armPostedRecv1104(t, w, css[0], peers[0], req1208)
		css[0].protocol.Store(int32(engine.H2C))
		if look1229(w) {
			t.Errorf("h1UnreadInKernel: true for an HTTP/2 conn's recv CQE")
		}
	})
	for _, tc := range []struct {
		name string
		mark func(cs *connState)
	}{
		{"a closing conn", func(cs *connState) { cs.closing = true }},
		{"an ended detached stream", func(cs *connState) { cs.writeEnded.Store(true) }},
		{"an HTTP/2 conn", func(cs *connState) { cs.protocol.Store(int32(engine.H2C)) }},
		{"a running detached stream", func(cs *connState) { cs.h1State.Detached.Store(true) }},
	} {
		t.Run(tc.name+" is not asked", func(t *testing.T) {
			w, css, peers := gateRig884(t, 1)
			if _, err := unix.Write(peers[0], []byte(req1208)); err != nil {
				t.Fatal(err)
			}
			tc.mark(css[0])
			if look1229(w) {
				t.Errorf("h1UnreadInKernel: true for %s with bytes in its socket", tc.name)
			}
		})
	}
}

// The gate's first pass marks a conn it finds with a request in the kernel
// (gateRead): the worker must wait for that request to be read and answered, not
// exit between the mark and the read.
func TestGateH1RequestInProgressCountsAMarkedConn1208(t *testing.T) {
	w, css, peers := gateRig884(t, 1)
	if w.h1RequestInProgress() {
		t.Fatal("premise: an idle conn is a request in progress")
	}
	if _, err := unix.Write(peers[0], []byte(req1208)); err != nil {
		t.Fatal(err)
	}
	w.refuseRecv = true
	w.closeIdleH1() // the first pass marks the conn
	if !css[0].gateRead {
		t.Fatal("premise: the first pass did not mark the conn")
	}
	if !w.h1RequestInProgress() {
		t.Errorf("h1RequestInProgress: false for a conn the gate marked to be read on: the worker would exit with its request unanswered")
	}
	css[0].writeEnded.Store(true)
	if w.h1RequestInProgress() {
		t.Errorf("h1RequestInProgress: true for a marked conn whose detached stream was ended")
	}
	css[0].writeEnded.Store(false)
	css[0].closing = true
	if w.h1RequestInProgress() {
		t.Errorf("h1RequestInProgress: true for a marked conn that is closing")
	}
}

// The look is inside the floor only: with a request the worker cannot read in
// the kernel (it has stopped reading past the floor), asking on would hold the
// worker for the whole budget. It is also about the requests that were in the
// kernel when the drain began: a request that arrives after the snapshot, on a
// conn that had nothing then, does not hold the worker (celeris#1229).
func TestGateSendDrainDoneAsksTheKernelInsideTheFloorOnly1208(t *testing.T) {
	w, css, peers := gateRig884(t, 2)
	const start = int64(5_000_000_000)
	w.shutdownDrainStart = start
	if !done1229(w, start+1) {
		t.Fatal("control: a worker with nothing to send and nothing in the kernel is not done")
	}
	if _, err := unix.Write(peers[0], []byte(req1208)); err != nil {
		t.Fatal(err)
	}
	if !w.markUnreadAtShutdown() {
		t.Fatal("premise: the snapshot did not mark the conn whose request was in the kernel")
	}
	if done, held := w.sendDrainDone(start + 1); done || !held {
		t.Errorf("sendDrainDone at the start of the drain with a request in the kernel = done %v, held %v, want false, true (the caller stops accepting)", done, held)
	}
	if done1229(w, start+shutdownSendDrainNanos) {
		t.Errorf("sendDrainDone: true at the floor itself (not past it) with a request in the kernel")
	}
	if done, held := w.sendDrainDone(start + shutdownSendDrainNanos + 1); !done || held {
		t.Errorf("sendDrainDone past the floor = done %v, held %v, want true, false: the look would keep the worker beyond it", done, held)
	}
	// A response going out is the other reason to serve on, and then the worker
	// keeps accepting for the floor as it always did: not held by the look.
	css[0].sending = true
	if done, held := w.sendDrainDone(start + 1); done || held {
		t.Errorf("sendDrainDone with a send pending = done %v, held %v, want false, false: only a worker with nothing to send stops accepting", done, held)
	}
	css[0].sending = false

	// A request that reaches the other conn after the snapshot is not asked
	// about, whether the marked one is read or not.
	if _, err := unix.Write(peers[1], []byte(req1208)); err != nil {
		t.Fatal(err)
	}
	if done1229(w, start+1) {
		t.Errorf("sendDrainDone: true while the marked conn's request is unread")
	}
	buf := make([]byte, 256)
	if _, err := unix.Read(css[0].fd, buf); err != nil {
		t.Fatal(err)
	}
	if q := inq1208(t, css[1].fd); q == 0 {
		t.Fatal("premise: the request of the conn that arrived after the snapshot is not in the kernel")
	}
	if !done1229(w, start+1) {
		t.Errorf("sendDrainDone: false once the marked conn was read, on the strength of a conn that arrived after the snapshot")
	}
}

// The snapshot marks the conns that have a request in the kernel at that
// moment, and nothing a later pass sees: not a request that arrives after it,
// and not a conn that is not asked about at the check (a conn marked that
// closed since).
func TestGateMarkUnreadAtShutdownIsASnapshot1229(t *testing.T) {
	t.Run("nothing in the kernel marks nothing, and the look is then false whatever arrives", func(t *testing.T) {
		w, css, peers := gateRig884(t, 2)
		if w.markUnreadAtShutdown() {
			t.Fatal("markUnreadAtShutdown: true for conns with nothing in the kernel")
		}
		for _, p := range peers {
			if _, err := unix.Write(p, []byte(req1208)); err != nil {
				t.Fatal(err)
			}
		}
		if w.h1UnreadInKernel() {
			t.Errorf("h1UnreadInKernel: true for requests that arrived after the snapshot")
		}
		for i, cs := range css {
			if cs.shutdownUnread {
				t.Errorf("conn %d is marked by a request that arrived after the snapshot", i)
			}
		}
	})
	t.Run("a marked conn that closed is not asked", func(t *testing.T) {
		w, css, peers := gateRig884(t, 1)
		if _, err := unix.Write(peers[0], []byte(req1208)); err != nil {
			t.Fatal(err)
		}
		if !look1229(w) {
			t.Fatal("premise: the look did not see the request in the kernel")
		}
		css[0].closing = true
		if w.h1UnreadInKernel() {
			t.Errorf("h1UnreadInKernel: true for a marked conn that is closing")
		}
	})
	t.Run("a second snapshot replaces the marks", func(t *testing.T) {
		w, css, peers := gateRig884(t, 1)
		if _, err := unix.Write(peers[0], []byte(req1208)); err != nil {
			t.Fatal(err)
		}
		if !w.markUnreadAtShutdown() || !css[0].shutdownUnread {
			t.Fatal("premise: the snapshot did not mark the conn")
		}
		if _, err := unix.Read(css[0].fd, make([]byte, 256)); err != nil {
			t.Fatal(err)
		}
		if w.markUnreadAtShutdown() || css[0].shutdownUnread {
			t.Errorf("a second snapshot with the request read still marks the conn")
		}
	})
}

// A worker with a response going out never takes the worker-with-nothing-to-send
// path (sendDrainDone, held, which stops accepting): every state a response can
// be in counts, including the ones the SEND_ZC and fallback paths pass through
// (celeris#1229, after a Coverage run in which a held-back conn of a worker with
// 16 stalled responses was reset). The unreachable-in-practice states are listed
// too, so that the predicate does not depend on the send buffer being kept.
func TestGateHasPendingSendsCountsEveryStateOfAResponse1229(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(cs *connState)
	}{
		{"a SEND in flight", func(cs *connState) { cs.sending = true }},
		{"a queued resend (the SQ ring was full)", func(cs *connState) { cs.sendBuf = []byte("x") }},
		{"bytes written, not flushed", func(cs *connState) { cs.writeBuf = []byte("x") }},
		{"a ZC first completion that failed: sending cleared, the notification outstanding, the bytes kept", func(cs *connState) {
			cs.zcNotifPending, cs.sendBuf = true, []byte("x")
		}},
		{"a ZC notification outstanding alone", func(cs *connState) { cs.zcNotifPending = true }},
		{"a send completion held for the dispatch goroutine", func(cs *connState) {
			cs.heldSends = append(cs.heldSends, completionEntry{})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, css, _ := gateRig884(t, 2)
			if w.hasPendingSends() {
				t.Fatal("control: idle conns have a send pending")
			}
			tc.set(css[1])
			if !w.hasPendingSends() {
				t.Errorf("hasPendingSends: false with %s", tc.name)
			}
			w.shutdownDrainStart = 5_000_000_000
			if done, held := w.sendDrainDone(w.shutdownDrainStart + 1); done || held {
				t.Errorf("sendDrainDone with %s = done %v, held %v, want false, false (a worker with a response going out does not stop accepting)", tc.name, done, held)
			}
		})
	}
}
