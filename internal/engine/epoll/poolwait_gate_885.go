//go:build linux

package epoll

import (
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
)

// The gate of the wait for HTTP/2 pool handlers (celeris#885 item 49), the
// epoll twin of io_uring's (celeris#884 item 50). A loop whose context is
// cancelled waits, as long as an HTTP/2 conn has a handler running on the
// shared worker pool, a response queued or DATA waiting for a window
// (h2PoolSettled), and the wait can last the whole budget of the shutdown. It
// used to go on reading and answering the keep-alive requests of its HTTP/1
// clients and let detached streams (Server-Sent Events, a WebSocket) write
// meanwhile: the phases of shutdown that stop both (its phase 1 and the send
// drain) run only after the wait. Measured with
// TestShutdownPoolWaitStopsServingH1AndDetached884 on epoll: 16 of 16
// requests answered and about 700 ticks written 1.5 s into the shutdown.
//
// The gate comes poolWaitGrace after the wait began, as the send drain's does,
// so a request in flight when the shutdown began is still answered. From then
// on the loop reads no more new requests from an HTTP/1 conn (refuseRecv) and
// ends the writes of every detached stream (stopDetachedProducers). A request
// already in progress on a conn the loop parses itself (headers in pieces, a
// body arriving) is read on and answered, as std's Shutdown answers an active
// conn, with Connection: close (H1State.CloseAfter, as net/http's response
// written after Shutdown began). The HTTP/2 conns the wait is for are served on:
// they have had GOAWAY since the wait began.
//
// An HTTP/1 conn that is idle, holds nothing queued and has no request in
// progress is closed, as std's Shutdown closes its idle conns: its client fails
// at once and retries elsewhere, where it would otherwise wait, with no answer,
// for as long as the wait lasts (up to the budget of the shutdown) and then be
// cut (closeIdleH1). So is a conn that was accepted and has sent nothing, and a
// detached stream the gate has ended once what was queued for it has gone out
// (its client gets a FIN and reconnects), whatever kind of route it is on. A
// conn a dispatch goroutine owns (asyncPromoted: on a server with any Async
// route every conn whose read leaves a partial request is promoted, whatever its
// route, and a conn on an Async route is) is otherwise left to the end of the
// wait, with a request in progress on it unread: the documented behaviour and
// a follow-up shared with io_uring (which has no idle close at all).

// poolWaitGrace is how long into the wait for the HTTP/2 pool handlers the
// gate stays open: io_uring's shutdownSendDrainNanos, and this engine's
// shutdownSendDrainFloor.
const poolWaitGrace = shutdownSendDrainFloor

// gateClosesPerPass caps the conns closeIdleH1 closes in one pass, so that a
// loop with thousands of idle conns does not stop serving the HTTP/2 conns the
// wait is for while it closes them all (several syscalls each): the next pass,
// 10 ms on, goes on.
const gateClosesPerPass = 64

// refusesRecv reports whether a loop past the pool wait's gate reads no more
// from cs: an HTTP/1 conn (detached or not) or one whose protocol is not
// detected yet, which io_uring's refuseRecv test mirrors, with one exception
// io_uring does not make: a request in progress is read on, and answered, as
// std's Shutdown answers the request of an active conn (headers in pieces, a
// body still arriving, the bytes of a protocol decision begun). A conn whose
// dispatch goroutine owns its parse state (asyncPromoted) is refused as before
// while it is HTTP/1: its state is not the loop's to read. Loop thread.
func refusesRecv(cs *connState) bool {
	if !cs.detected {
		return cs.detectN == 0
	}
	if cs.asyncPromoted {
		// protocol, h1State and h2State are the dispatch goroutine's, which
		// writes them in switchToH2Local under detachMu: not read here. The
		// conn is HTTP/1 until the goroutine sets h2Switched (under asyncInMu,
		// which is held for an instant, never across a handler, so waiting for
		// it cannot stall the pass), and an HTTP/1 conn it owns is refused, as
		// before; one it has switched is HTTP/2 and is served (celeris#885
		// item 28). Not refused on asyncPromoted alone: the pool wait is for
		// the HTTP/2 conns.
		return !cs.switchedToH2()
	}
	if cs.protocol != engine.HTTP1 {
		return false
	}
	return cs.h1State == nil || !cs.h1State.HasPendingData()
}

// poolWaitGate applies the gate once the loop has waited poolWaitGrace for the
// HTTP/2 pool handlers (h2PoolSettled has set h2DrainStart). The wait is a
// serving phase, so the walk over every conn, which ends the producers and
// closes the idle HTTP/1 conns, is not paid on every pass: it runs at most once
// per h2PoolDrainPollMs. Loop thread.
func (l *Loop) poolWaitGate() {
	now := time.Now()
	if now.Sub(l.h2DrainStart) <= poolWaitGrace {
		return
	}
	l.refuseRecv = true
	if n := now.UnixNano(); n >= l.producersStopNext {
		l.producersStopNext = n + int64(h2PoolDrainPollMs*time.Millisecond)
		l.stopDetachedProducers()
		l.closeIdleH1()
	}
}

// closeIdleH1 closes the HTTP/1 conns the gate has stopped reading that hold
// nothing: no response queued in the engine or the kernel's send queue (a close
// with bytes unsent would reset them away), no request in progress, not a
// detached stream still running (one the gate has ended is closed once it has
// flushed, on a sync route or an Async one), no dispatch goroutine otherwise
// (see the gate's comment), and a conn with no byte sent (not detected,
// detectN 0). It also marks every conn it meets close-after (H1State.CloseAfter),
// so that a request it still reads (one in progress) is answered with
// Connection: close. A conn whose response is
// still going out is met again by the next pass. closeConn removes the conn
// from liveConns by swapping it with the last entry, so the walk goes from the
// top down, as checkTimeouts does. Loop thread.
func (l *Loop) closeIdleH1() {
	closed := 0
	for i := len(l.liveConns) - 1; i >= 0; i-- {
		cs := l.liveConns[i]
		if cs.hijacked.Load() {
			continue
		}
		// A conn a dispatch goroutine owns is closed here only when the gate has
		// ended its detached stream (an SSE or WebSocket handler on an Async
		// route detaches on that goroutine): the goroutine is out of its handler,
		// since detachClosed is set only under a detachMu the handler held, its
		// writes no-op, and closeConn takes the lock as shutdown's own teardown
		// does. The state is read under the lock.
		ended := false
		if cs.asyncPromoted {
			mu := cs.detachMu
			if mu == nil || !mu.TryLock() {
				continue
			}
			ended = cs.detachClosed && cs.protocol == engine.HTTP1 && cs.h1State != nil && cs.h1State.Detached.Load()
			mu.Unlock()
			if !ended {
				continue
			}
		}
		if cs.detected {
			if cs.protocol != engine.HTTP1 {
				continue
			}
			if h := cs.h1State; h != nil && !ended {
				// A request read after the gate is answered close-after (net/http
				// does the same for a response written after Shutdown began).
				h.CloseAfter = true
				if h.HasPendingData() || (h.Detached.Load() && !cs.detachClosed) {
					continue
				}
			}
		} else if cs.detectN != 0 {
			continue
		}
		if closed >= gateClosesPerPass || csWritePending(cs) || cs.dirty || cs.epollOut {
			continue
		}
		if q, err := unix.IoctlGetInt(cs.fd, unix.TIOCOUTQ); err != nil || q != 0 {
			continue
		}
		l.closeConn(cs.fd)
		closed++
	}
}

// stopDetachedProducers is the detached half of shutdown's phase 1, run early
// by the pool wait's gate: a detached conn (Server-Sent Events, a WebSocket)
// has a goroutine of its own that keeps writing, and with the wait lasting as
// long as the budget a stream that never ends would write for all of it. The
// stream is told as shutdown tells it: asyncClosed, which the write-error
// query reads first, then, under detachMu, the parked read-end error, then
// detachClosed (the guarded write hook no-ops from there) and OnDetachClose.
// Only conns that have detached (h1State.Detached) and whose dispatch
// goroutine is not inside the handler: the goroutine holds detachMu across
// the handler, so the TryLock fails for a running async handler, which is
// answered, up to the budget, as before. TryLock, never Lock, so a goroutine
// parked inside a write cannot wedge the loop; a conn whose lock is busy is
// tried again on the next pass of the pacing. A conn whose async Detach is
// not yet finalised (asyncDetachPending) is left for that pass.
//
// What the producer's guarded write hook left queued is flushed, not
// stranded: the inline write may have taken only part of the bytes, and
// drainDetachQueue skips a detachClosed conn, so a conn with bytes queued and
// neither EPOLLOUT armed nor on the dirty list is put on the list here. The
// rest is shutdown's own, which every step repeats harmlessly (the parked
// error and OnDetachClose are consumed once). Loop thread.
func (l *Loop) stopDetachedProducers() {
	for i := len(l.liveConns) - 1; i >= 0; i-- {
		cs := l.liveConns[i]
		mu := cs.detachMu
		if mu == nil || cs.hijacked.Load() {
			continue
		}
		// h1State is read before the lock only on a conn the loop owns: on a
		// promoted conn the dispatch goroutine sets it to nil in the h2c switch,
		// under detachMu, so it is read under the lock (celeris#885 item 28).
		promoted := cs.asyncPromoted
		if !promoted && (cs.h1State == nil || !cs.h1State.Detached.Load()) {
			continue
		}
		if !mu.TryLock() {
			continue
		}
		if promoted && (cs.h1State == nil || !cs.h1State.Detached.Load()) {
			mu.Unlock()
			continue
		}
		// Re-tested under the lock, as closeConn's ownership check is: an async
		// Hijack takes the conn and closes its descriptor under this lock.
		if cs.hijacked.Load() || (!cs.detachClosed && cs.asyncDetachPending) {
			mu.Unlock()
			continue
		}
		if !cs.detachClosed {
			cs.asyncClosed.Store(true)
			if cs.asyncCond.L != nil {
				cs.asyncInMu.Lock()
				cs.asyncCond.Broadcast()
				cs.asyncInMu.Unlock()
			}
			if err := cs.closeErr; err != nil {
				cs.closeErr = nil
				if cs.h1State.OnError != nil {
					cs.h1State.OnError(err)
				}
			}
			cs.detachClosed = true
			if cs.h1State.WSReady.Load() && cs.h1State.OnDetachClose != nil {
				cs.h1State.OnDetachClose()
				cs.h1State.OnDetachClose = nil
			}
		}
		// What is queued must still reach the peer, and nothing but this pass
		// will see to it once detachClosed is set: drainDetachQueue skips such
		// a conn, so a remainder the producer's write queued for the loop, or a
		// conn a relink gave up (its hand-back is skipped the same way) and
		// that EPOLLOUT no longer watches, is stranded. Met again on every
		// pass, for a conn that is given up after this one.
		if csWritePending(cs) && !cs.epollOut && !cs.dirty {
			l.markDirty(cs)
		}
		mu.Unlock()
	}
}
