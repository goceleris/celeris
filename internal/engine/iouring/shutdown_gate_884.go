//go:build linux

package iouring

import (
	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
)

// The conns the shutdown gate stops reading (celeris#884 item 73), the
// io_uring twin of epoll's closeIdleH1 and refusesRecv (celeris#885 item 49,
// poolwait_gate_885.go). Past the first shutdownSendDrainNanos of the send
// drain, or of the wait for the HTTP/2 pool handlers before it, an HTTP/1 conn
// is read no more (refuseRecv): the drain, which lasts as long as the budget,
// would never find its queues empty under steady keep-alive traffic. That left
// the conns the gate had stopped reading open with no answer until the end of
// the wait (up to the budget of the shutdown): an idle keep-alive client, a
// conn that was accepted and has sent nothing, a detached stream the gate had
// ended, and a request whose bytes were still arriving at the gate (an upload,
// headers in pieces), which was dropped, never answered, and cut at the end.
// std's Shutdown closes the idle conns at once and answers the request of an
// active conn, so a client of either fails or finishes at once and retries
// elsewhere. The gate now does the same:
//
//   - a conn that is idle, holds nothing queued (user space or the kernel's
//     send queue) and has no request in progress is closed (closeIdleH1): an
//     HTTP/1 keep-alive conn, one that has sent no byte, and a detached stream
//     the gate has ended once what was queued for it has gone out (its client
//     gets a FIN and reconnects), whatever kind of route it is on;
//   - a request in progress is read on and answered (gateReadsOn): headers in
//     pieces, a fixed-length or chunked body still arriving, the bytes of a
//     protocol decision begun. The response carries Connection: close
//     (H1State.CloseAfter, as net/http's response written after Shutdown began
//     does) and the conn is closed once it has gone out;
//   - the HTTP/2 conns the wait is for are served on: they have had GOAWAY since
//     the wait began.
//
// A conn whose dispatch goroutine owns its parse state (asyncPromoted: a conn on
// an Async route, or, on a server with any Async route, one whose read left a
// partial request) is looked at under the goroutine's own locks, never without:
// asyncInMu says whether input is waiting for it or it is inside a handler,
// and detachMu, taken with TryLock as every worker-thread site does (celeris#593),
// is free only while the goroutine is between requests, so h1State is read under
// it. Epoll cannot yet look inside such a conn (its follow-up F2); this engine
// can.

// gateClosesPerPass caps the conns closeIdleH1 closes in one pass, so that a
// worker with thousands of idle conns does not stop serving the conns the gate
// is waiting for while it closes them all (a cancel SQE and a few syscalls
// each): the next pass, h2PoolDrainPoll on, goes on.
const gateClosesPerPass = 64

// gateReadsOn reports whether handleRecv, past the gate, reads cs's bytes on
// instead of dropping them: a request in progress (see the list above). It is
// called only when refuseRecv is set and the conn is HTTP/1 or undecided, and
// it marks the conn close-after when it says yes. Worker thread.
func (w *Worker) gateReadsOn(cs *connState) bool {
	if cs.gateRead {
		// The gate's first pass found the bytes of a request in the kernel's
		// receive queue (holdUnreadAtGate): they reached it before the gate,
		// and are read and answered whatever the conn looks like (the conn is
		// marked close-after by the pass, or by initProtocol).
		return true
	}
	if !cs.detected {
		// The bytes of a protocol decision begun. Nothing yet (no earlier
		// bytes): a request that begins after the gate.
		return len(cs.detectAccum) > 0
	}
	// Before any read of h1State: on a promoted conn the dispatch goroutine owns
	// the field (switchToH2Local sets it to nil during an h2c upgrade, celeris#722),
	// so it is read only under detachMu (promotedInProgress).
	if cs.asyncPromoted.Load() {
		return w.promotedInProgress(cs)
	}
	h := cs.h1State
	if h == nil || h.Detached.Load() {
		return false // a detached stream is read no more: its writes were ended with the gate
	}
	// A conn the worker parses itself: its h1State is the worker's to read.
	if cs.recvIntoBody || h.HasPendingData() || !h.AtRequestBoundary() {
		h.CloseAfter = true
		return true
	}
	return false
}

// promotedInProgress is gateReadsOn for a conn whose dispatch goroutine owns the
// parse state: true while input is waiting for the goroutine, it is inside a
// handler (or a guarded write holds detachMu), or its parser holds part of a
// request. It marks an idle conn close-after too: the worker does not know
// that no request will follow before the pass closes the conn. Worker thread.
func (w *Worker) promotedInProgress(cs *connState) bool {
	// A detached stream whose writes were ended (by the gate or a close) is read
	// no more; writeEnded is an atomic, so this holds even while a guarded write
	// has detachMu.
	if cs.writeEnded.Load() {
		return false
	}
	if dispatchHasWork(cs) {
		return true
	}
	mu := cs.detachMu
	if mu == nil || !mu.TryLock() {
		return true
	}
	defer mu.Unlock()
	h := cs.h1State
	if h == nil || h.Detached.Load() {
		return false
	}
	h.CloseAfter = true
	return h.HasPendingData() || !h.AtRequestBoundary()
}

// dispatchHasWork reports whether a conn's dispatch goroutine has a request to
// serve or is serving one: input waiting in asyncInBuf, or the goroutine alive
// and out of its park loop (dispatchBusy's test, which also covers the window
// after it took the input and before it holds detachMu), all read under
// asyncInMu. A goroutine that has released detachMu for good at a Detach is not
// counted: such a conn is a stream, judged by detachClosed. Worker thread.
func dispatchHasWork(cs *connState) bool {
	cs.asyncInMu.Lock()
	busy := len(cs.asyncInBuf) > 0 || (cs.asyncRun && !cs.asyncParked && !cs.asyncDetachUnlocked)
	cs.asyncInMu.Unlock()
	return busy
}

// closeIdleH1 is the gate's pass over the live conns: every HTTP/1 conn it can
// look at is marked close-after (H1State.CloseAfter, so that a request it still
// reads, one in progress, is answered with Connection: close), and the ones that
// hold nothing are closed (gateCloseable). A conn whose response is still going
// out, or whose lock is busy, is met again by the next pass. closeConn removes
// a conn from liveConns by swapping it with the last entry, so the walk goes
// from the top down, as checkTimeouts does. The pass runs at the top of the loop
// pass, before the pass reaps new completions, so a request that reached the
// kernel since the last reap meets a conn that looks idle. The first pass looks
// for it in both places it can be (celeris#1104): as a recv CQE posted and not
// reaped (holdPostedRecvs: the base tier, and any ring that posts at once), and
// as bytes in the socket's queue (holdUnreadAtGate: where a ring's completions
// are task work, COOP_TASKRUN or DEFER_TASKRUN, the request waits in the socket
// until the next enter). The conn is held and answered instead of closed. What
// is left: a fixed-file conn cannot be asked for its socket queue (its CQE is
// still seen), and a CQE the kernel posts after the look, which is the race
// std's own idle close has, between the question and close(2).
// Worker thread.
func (w *Worker) closeIdleH1() {
	first := !w.gateSwept
	w.gateSwept = true
	if first {
		w.holdPostedRecvs()
	}
	closed := 0
	for i := len(w.liveConns) - 1; i >= 0; i-- {
		fd := w.liveConns[i]
		cs := w.conns[fd]
		if cs == nil || cs.closing {
			continue
		}
		if cs.detected && engine.Protocol(cs.protocol.Load()) != engine.HTTP1 {
			continue
		}
		if !w.gateCloseable(cs) {
			continue
		}
		// The first pass asks the kernel's receive queue (celeris#1104): bytes
		// there reached the kernel before the gate, and are a request in
		// progress, read on and answered instead of the conn being closed. Ahead
		// of the cap and the send-side tests, so that a conn those leave for the
		// next pass is not met unasked.
		if first && (cs.gateRead || w.holdUnreadAtGate(cs)) {
			continue
		}
		// Past the cap the conn was still looked at, so it is marked close-after
		// (gateCloseable); it is closed by a later pass.
		if closed >= gateClosesPerPass {
			continue
		}
		// The kernel's send queue: a conn whose response left user space but
		// has not all been acknowledged by the peer is not idle yet. close(2)
		// keeps the send queue and sends the FIN after it, so nothing is lost by
		// closing early unless the peer sends after the close (the unread
		// bytes turn the close into an RST, and the peer discards what it had
		// not read) or an SO_LINGER{1,0} path is in play; waiting for an empty
		// queue (SIOCOUTQ counts unacknowledged bytes) keeps that window shut,
		// and is what epoll does. A fixed-file conn's slot is an index into the
		// ring's file table, not a descriptor, so the ioctl cannot ask about it:
		// such a conn is judged by its user-space queues alone
		// (ownsDescriptorNumber).
		if ownsDescriptorNumber(cs) {
			if q, err := unix.IoctlGetInt(cs.fd, unix.TIOCOUTQ); err != nil || q != 0 {
				continue
			}
		}
		w.closeConn(fd)
		closed++
	}
}

// holdPostedRecvs marks every conn whose recv completion is in the completion
// queue and not reaped yet, with bytes in it, to be read on (cs.gateRead): the
// first pass runs at the top of the worker iteration, ahead of the reap, and a
// request that the kernel delivered since the last reap is a CQE, not bytes in
// the socket's queue, so holdUnreadAtGate's question answers 0 for it (the
// base tier, whose recv completes at once: CI measured socket-queue-nonempty=0,
// cq-unreaped=every live conn, and 18 conns closed with their request delivered).
// The CQEs are looked at in place, nothing is consumed: the reap that follows
// takes them, and handleRecv, with refuseRecv up, reads the marked conns on and
// answers them close-after. Only a recv with bytes (Res > 0) is a request; an
// EOF or an error is not. A CQE of a conn that has closed since, or whose number
// another conn has taken, is skipped by the generation, as the dispatch does
// (staleConnCQE, without its side effects: nothing is recycled here). An ended
// detached stream is not marked. The range is the worker's whole unreaped queue,
// walked once, on the first pass only. A CQE the kernel posts after the look is
// the race any close has. Worker thread.
func (w *Worker) holdPostedRecvs() {
	head, tail := w.ring.BeginCQ()
	for ; head != tail; head++ {
		e := w.ring.cqeAt(head)
		ud := e.UserData
		if ud&udMask != udRecv || e.Res <= 0 {
			continue
		}
		fd := int(ud & fdMask)
		if fd >= len(w.conns) {
			continue
		}
		cs := w.conns[fd]
		if cs == nil || cs.closing || cs.generation != decodeGen(ud) || cs.writeEnded.Load() {
			continue
		}
		cs.gateRead = true
	}
}

// holdUnreadAtGate reports whether cs, which gateCloseable has found idle,
// holds the bytes of a request in the kernel's receive queue, and if so marks
// it to be read on (cs.gateRead, gateReadsOn): the recv that is armed on it
// delivers them, as a CQE the next reap takes, and the request is answered with
// Connection: close. On a ring whose completions are task work (COOP_TASKRUN,
// DEFER_TASKRUN) a request that reaches a socket between two of the worker's
// enters waits there, in the socket's queue, until the next enter posts its
// recv CQE: the pass, at the top of the iteration, is that window. An ended
// detached stream is not asked (its bytes are the application's), and a
// fixed-file conn cannot be (its slot is an index into the ring's file table,
// not a descriptor: ownsDescriptorNumber): such a conn is judged by what the
// worker holds for it alone. A failed ioctl is not an answer: the conn is left
// to the next pass. Worker thread.
func (w *Worker) holdUnreadAtGate(cs *connState) bool {
	if cs.writeEnded.Load() || !ownsDescriptorNumber(cs) {
		return false
	}
	q, err := unix.IoctlGetInt(cs.fd, unix.TIOCINQ)
	if err != nil {
		return true
	}
	if q == 0 {
		return false
	}
	cs.gateRead = true
	return true
}

// gateCloseable reports whether the gate may close cs: it is HTTP/1 (or not
// decided, with no byte), has no request in progress, holds nothing queued and,
// if it is a detached stream, the gate has ended it (a stream that still runs
// is ended by stopDetachedProducers first). It marks every non-detached HTTP/1
// conn it can look at close-after. Worker thread.
func (w *Worker) gateCloseable(cs *connState) bool {
	if !cs.detected {
		// No byte (an undecided conn holding some is a request begun) and no
		// dispatch goroutine yet: the first bytes decide that.
		return len(cs.detectAccum) == 0 && !connSendPending(cs) && !cs.zcNotifPending
	}
	if cs.asyncPromoted.Load() && dispatchHasWork(cs) {
		return false
	}
	if mu := cs.detachMu; mu != nil {
		if !mu.TryLock() {
			return false // inside a handler or a guarded write
		}
		defer mu.Unlock()
	}
	h := cs.h1State
	if h == nil {
		return false
	}
	if h.Detached.Load() {
		if !cs.detachClosed {
			return false // the stream still runs
		}
	} else {
		h.CloseAfter = true
		if cs.recvIntoBody || h.HasPendingData() || !h.AtRequestBoundary() {
			return false
		}
	}
	return !connSendPending(cs) && !cs.zcNotifPending && len(cs.heldSends) == 0
}

// closeIdlePaced runs closeIdleH1 at most once per h2PoolDrainPoll, for the
// send drain's gate, whose own walk over the detached conns is not paced (the
// drain is not a serving phase). It shares producersStopNext with the pool
// wait's gate, which never runs in the same loop pass. now is the pass's
// UnixNano. Worker thread.
func (w *Worker) closeIdlePaced(now int64) {
	if now < w.producersStopNext {
		return
	}
	w.producersStopNext = now + int64(h2PoolDrainPoll)
	w.closeIdleH1()
}
