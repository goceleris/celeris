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
// pass, before the pass reaps new completions: a request whose recv CQE was
// posted after the last reap meets a conn that looks idle and is closed with
// that request unread, the race std's own idle close has (microseconds wide;
// not measured). Worker thread.
func (w *Worker) closeIdleH1() {
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
