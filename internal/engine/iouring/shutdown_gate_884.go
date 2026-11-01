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
// elsewhere. The gate now does the same (an idle keep-alive conn and a conn that has
// sent nothing are closed at the gate, 250 ms into a drain, or at once by a
// worker with nothing to send; a head in the kernel is answered: by the gate's
// first pass, and by the look a worker with nothing to send makes inside the
// floor (h1UnreadInKernel) before it exits, as epoll's h1InProgress does; the
// Server.Shutdown godoc has the contract per engine and
// test/server/shutdown_conn_shapes_1208_linux_test.go pins it, celeris#1208):
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
// The gate is not what makes a response close any more: from Engine.Shutdown on,
// every HTTP/1 response written, before the gate as after it, carries
// Connection: close and ends its conn (conn.H1State.Shutdown, read where the
// response head is built; celeris#884). The CloseAfter marks below stay: they
// are the per-conn half, which the flag does not replace.
//
// A conn whose dispatch goroutine owns its parse state (asyncPromoted: a conn on
// an Async route, or, on a server with any Async route, one whose read left a
// partial request) is looked at under the goroutine's own locks, never without:
// asyncInMu says whether input is waiting for it or it is inside a handler,
// and detachMu, taken with TryLock as every worker-thread site does (celeris#593),
// is free only while the goroutine is between requests, so h1State is read under
// it. Epoll looks inside such a conn the same way (celeris#1165, #1166).

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
	if cs.gateRead && !cs.writeEnded.Load() {
		// The gate's first pass found a request that reached the kernel before
		// the gate, in its socket queue (holdUnreadAtGate) or as a recv CQE not
		// reaped yet (holdPostedRecvs): it is read and answered whatever the
		// conn looks like (the conn is marked close-after by the pass, or by
		// initProtocol). Not a detached stream the gate has ended.
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
		w.sweepUnreadAtGate()
	}
	closed := 0
	for i := len(w.liveConns) - 1; i >= 0; i-- {
		fd := w.liveConns[i]
		cs := w.conns[fd]
		if cs == nil {
			continue
		}
		if cs.closing {
			// Already closing (a response that said Connection: close is still going
			// out; its close waits for it): not read again and not served, but a
			// request that reached it behind that response is in its receive queue
			// unread when the descriptor is closed, and close(2) then sends an
			// RST that destroys the response in flight (celeris#884; epoll's
			// discardUnreadOfClosingConn). It is read off and dropped, unparsed.
			if first {
				w.discardUnreadOfClosingConn(cs)
			}
			continue
		}
		if cs.detected && engine.Protocol(cs.protocol.Load()) != engine.HTTP1 {
			continue
		}
		if !w.gateCloseable(cs) {
			continue
		}
		// A conn the first pass held (sweepUnreadAtGate, celeris#1104) holds a
		// request that is read on and answered, not closed. On the first pass
		// that is all; a later pass closes it only once its socket has nothing
		// left to deliver (a recv that has not yet been re-armed leaves the bytes
		// there, and close(2) would reset them).
		if cs.gateRead {
			if first || !ownsDescriptorNumber(cs) {
				continue
			}
			if q, err := unix.IoctlGetInt(cs.fd, unix.TIOCINQ); err != nil || q != 0 {
				continue
			}
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

// discardUnreadOfClosingConn reads what the kernel holds in the receive queue of
// a closing HTTP/1 conn and drops it without parsing it (see closeIdleH1).
// Bounded: 64 reads of 4 KiB, then the close's RST as before; EAGAIN, an EOF or
// an error ends it. Not for a fixed-file conn (its slot is no descriptor), a
// conn a dispatch goroutine owns (its state is the goroutine's), HTTP/2, or a
// detached stream. Worker thread.
func (w *Worker) discardUnreadOfClosingConn(cs *connState) {
	if !ownsDescriptorNumber(cs) || cs.asyncPromoted.Load() || !cs.detected ||
		engine.Protocol(cs.protocol.Load()) != engine.HTTP1 {
		return
	}
	if h := cs.h1State; h == nil || h.Detached.Load() {
		return
	}
	var buf [4096]byte
	for range 64 {
		if n, err := unix.Read(cs.fd, buf[:]); n <= 0 || err != nil {
			return
		}
	}
}

// sweepUnreadAtGate is the first pass's look for requests that reached the
// kernel before the gate (celeris#1104), ahead of the walk that closes. Order
// matters: the sockets are asked first (every conn the pass could close:
// holdUnreadAtGate), the completion queue second (holdPostedRecvs). On a ring
// whose task work runs at any syscall return (COOP_TASKRUN, and the base tier's
// flags 0) the ioctls of the first walk can themselves turn a later conn's
// bytes into a recv CQE; looked at in the other order, such a request is in
// neither place when each is asked. Asked this way it is in one of them: still
// in the socket, or already a CQE by the time the queue is read. Worker thread.
func (w *Worker) sweepUnreadAtGate() {
	for i := len(w.liveConns) - 1; i >= 0; i-- {
		cs := w.conns[w.liveConns[i]]
		if cs == nil || cs.closing {
			continue
		}
		if cs.detected && engine.Protocol(cs.protocol.Load()) != engine.HTTP1 {
			continue
		}
		if !w.gateCloseable(cs) {
			continue
		}
		w.holdUnreadAtGate(cs)
	}
	w.holdPostedRecvs()
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
	w.eachPostedRecv(func(cs *connState) bool { cs.gateRead = true; return false })
}

// eachPostedRecv calls f for each conn whose recv completion, with bytes, is in
// the completion queue and not reaped yet (the checks of holdPostedRecvs), and
// stops at the first f that returns true. Nothing is consumed. Worker thread.
func (w *Worker) eachPostedRecv(f func(cs *connState) bool) {
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
		if f(cs) {
			return
		}
	}
}

// holdUnreadAtGate reports whether cs, which gateCloseable has found idle,
// holds the bytes of a request in the kernel's receive queue, and if so marks
// it to be read on (cs.gateRead, gateReadsOn): the recv that is armed on it
// delivers them, as a CQE the next reap takes, and the request is answered with
// Connection: close. Where the bytes are when the pass runs depends on the
// ring's task work: on a DEFER_TASKRUN ring (the default tier on a 6.1+ kernel)
// a request that reaches a socket between two of the worker's enters waits in
// the socket's queue until the next enter posts its recv CQE, which is what
// this asks; on the base tier and under COOP_TASKRUN the recv completes when the
// bytes arrive and the request is a CQE already (holdPostedRecvs, which the
// sweep asks second). An ended detached stream is not asked (its bytes are the
// application's), and a fixed-file conn cannot be (its slot is an index into the
// ring's file table, not a descriptor: ownsDescriptorNumber; its CQE is still
// seen). A failed ioctl is not an answer: the conn is held for this pass.
// Worker thread.
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
// drainclock.Now. Worker thread.
func (w *Worker) closeIdlePaced(now int64) {
	if now < w.producersStopNext {
		return
	}
	w.producersStopNext = now + int64(h2PoolDrainPoll)
	w.closeIdleH1()
}

// h1RequestInProgress reports whether an HTTP/1 conn of the worker has a request
// in progress: part of one in its parser, the bytes of a protocol decision
// begun, or a dispatch goroutine with input waiting or inside a handler
// (promotedInProgress). With sends pending the send drain keeps the worker
// turning and the gate reads such a request on and answers it; with none, the
// worker used to shut down at once and cut the request with its conn, unanswered
// (celeris#1166, as epoll's loop with no HTTP/2 conn did). It now waits for it,
// bounded as the drain is (drainEnd), with the same gate meanwhile. A request
// still in the kernel's queue or a recv CQE not reaped is not seen here
// (h1UnreadInKernel asks for those inside the floor, the gate's first pass
// marks them at it: gateRead, counted here). Worker thread.
func (w *Worker) h1RequestInProgress() bool {
	for _, fd := range w.liveConns {
		cs := w.conns[fd]
		if cs == nil || cs.closing {
			continue
		}
		if cs.gateRead && !cs.writeEnded.Load() {
			// The gate's first pass found a request that reached the kernel
			// before it (holdUnreadAtGate, holdPostedRecvs): it is read on and
			// answered (gateReadsOn), and the worker waits for that. The conn
			// leaves this walk once it is closing.
			return true
		}
		if !cs.detected {
			if len(cs.detectAccum) > 0 {
				return true
			}
			continue
		}
		if engine.Protocol(cs.protocol.Load()) != engine.HTTP1 {
			continue
		}
		if cs.asyncPromoted.Load() {
			if w.promotedInProgress(cs) {
				return true
			}
			continue
		}
		h := cs.h1State
		if h == nil || h.Detached.Load() {
			continue
		}
		if cs.recvIntoBody || h.HasPendingData() || !h.AtRequestBoundary() {
			return true
		}
	}
	return false
}

// h1UnreadInKernel reports whether an HTTP/1 conn of the worker has the bytes of
// a request the worker has not read: in the socket's receive queue (TIOCINQ, a
// ring whose completions are task work leaves a request there until the next
// enter) or as a recv CQE with bytes that the reap has not taken. It is the
// look of the gate's first pass (sweepUnreadAtGate, celeris#1104) without its
// marks, asked by a worker with nothing to send on its first look at the
// shutdown: epoll's h1InProgress counts the same bytes, and the worker used to
// exit with them unserved (celeris#1208). Sockets first, the completion queue
// second, as the sweep does. A conn the gate would not look at is not asked:
// closing, a detached stream (its bytes are the application's), HTTP/2, and a
// fixed-file conn's socket (its slot is no descriptor; its CQE is still seen).
// Worker thread.
func (w *Worker) h1UnreadInKernel() bool {
	for _, fd := range w.liveConns {
		cs := w.conns[fd]
		if cs == nil || cs.closing || cs.writeEnded.Load() || !ownsDescriptorNumber(cs) {
			continue
		}
		if cs.detected && engine.Protocol(cs.protocol.Load()) != engine.HTTP1 {
			continue
		}
		if cs.detected && w.detachedOrBusy(cs) {
			continue
		}
		if q, err := unix.IoctlGetInt(cs.fd, unix.TIOCINQ); err != nil || q != 0 {
			return true
		}
	}
	return w.postedRecvs()
}

// detachedOrBusy reports whether h1UnreadInKernel leaves a detected conn out: a
// detached stream, or a conn whose dispatch goroutine holds detachMu (inside a
// handler: a request in progress that h1RequestInProgress sees). Worker thread.
func (w *Worker) detachedOrBusy(cs *connState) bool {
	if mu := cs.detachMu; mu != nil {
		if !mu.TryLock() {
			return true
		}
		defer mu.Unlock()
	}
	h := cs.h1State
	return h == nil || h.Detached.Load()
}

// postedRecvs reports whether the completion queue holds a recv with bytes for
// an HTTP/1 conn the gate could still read on (the conns h1UnreadInKernel
// leaves out are left out here), without marking it (holdPostedRecvs is the
// marking twin). Worker thread.
func (w *Worker) postedRecvs() bool {
	found := false
	w.eachPostedRecv(func(cs *connState) bool {
		if cs.detected && (engine.Protocol(cs.protocol.Load()) != engine.HTTP1 || w.detachedOrBusy(cs)) {
			return false
		}
		found = true
		return true
	})
	return found
}
