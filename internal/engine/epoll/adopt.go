//go:build linux

package epoll

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/ctxkit"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/sockopts"
)

// adoptItem is one pending io_uring→epoll transplant hand-off (#383 reverse
// direction): a real, connected fd to adopt onto an epoll loop, plus the
// carried-over state. Enqueued by AdoptConn (any thread), applied on the loop
// thread by drainAdoptQueue.
type adoptItem struct {
	fd    int
	carry engine.Carryover
}

// AdoptConn implements [engine.TransplantTarget] for the epoll engine (#383
// reverse direction). The source engine (io_uring) has ALREADY detached fd from
// its own rings (cancelled the recv, released its connState) before calling
// this, handing epoll a real, connected, non-blocking socket at an HTTP/1
// request boundary. AdoptConn routes it to one loop (round-robin) and schedules
// the attach on that loop's own thread via the eventfd wakeup, where epoll_ctl +
// connState setup are safe. Safe to call from any goroutine; on a returned error
// the caller still owns fd.
func (e *Engine) AdoptConn(fd int, carry engine.Carryover) error {
	if fd < 0 {
		return fmt.Errorf("celeris/epoll: adopt invalid fd %d", fd)
	}
	n := len(e.loops)
	if n == 0 {
		return fmt.Errorf("celeris/epoll: no loops available to adopt fd %d", fd)
	}
	l := e.loops[int(e.adoptRR.Add(1)-1)%n]
	l.adoptQMu.Lock()
	if l.adoptQClosed {
		l.adoptQMu.Unlock()
		// The loop has shut down and will never drain its queue again. A nil
		// return here would hand the descriptor to nobody; an error leaves it
		// with the source, whose reclaim path takes the connection back
		// (celeris#658).
		return fmt.Errorf("celeris/epoll: loop %d has shut down, cannot adopt fd %d", l.id, fd)
	}
	l.adoptQueue = append(l.adoptQueue, adoptItem{fd: fd, carry: carry})
	l.adoptQPending.Store(1)
	// Wake the loop so the adopt is applied promptly (the loop drains the
	// eventfd counter and then drainAdoptQueue). Signalled under adoptQMu:
	// Loop.shutdown takes this lock (closeAdoptQueue) before it closes the
	// eventfd. Since celeris#655 the handle enforces that on its own —
	// Signal and Close share a lock — and the ordering is kept here because
	// it also publishes the queue entry.
	l.wakeFD.Signal()
	l.adoptQMu.Unlock()
	// The eventfd only reaches a loop that is in epoll_wait. A standby loop
	// parked in DRAINING→SUSPENDED waits on a channel instead, and used to
	// sleep through this adoption (celeris#658). Kick it — after releasing
	// adoptQMu, because wakeMu is a leaf lock.
	l.wakeIfSuspended()
	return nil
}

var _ engine.TransplantTarget = (*Engine)(nil)

// closeAdoptQueue refuses every adoption still queued when the loop shuts
// down, and every AdoptConn after it. Loop.shutdown calls it first, on the
// loop thread.
//
// A queued adoption is a connection the source has ALREADY let go of:
// AdoptConn returned nil, so the source dropped it from its live gauge with no
// OnDisconnect. Loop.shutdown only walks the conn table, so an item still in
// the queue used to outlive the engine — descriptor open, owned by nobody, in
// CLOSE_WAIT once the peer gave up (celeris#658). The wakeup in AdoptConn does
// not make that state unreachable: a loop woken out of its park checks its
// context at the top of the next iteration, before drainAdoptQueue. So each
// item is finished here like any other adoption this loop cannot complete
// (refuseAdopt: close, count, fire the hook), and marking the queue closed
// under the same lock is what turns a later AdoptConn into an error the
// source can reclaim from, instead of a queue entry nothing will ever drain.
func (l *Loop) closeAdoptQueue() {
	l.adoptQMu.Lock()
	l.adoptQClosed = true
	queued := l.adoptQueue
	l.adoptQueue = nil
	l.adoptQPending.Store(0)
	l.adoptQMu.Unlock()
	for _, it := range queued {
		l.refuseAdopt(it.fd, it.carry)
	}
}

// drainAdoptQueue applies pending io_uring→epoll adoptions on the loop thread.
// Called from the run loop after the event batch.
func (l *Loop) drainAdoptQueue(ctx context.Context, now int64) {
	if l.adoptQPending.Load() == 0 {
		return
	}
	l.adoptQMu.Lock()
	l.adoptQSpare, l.adoptQueue = l.adoptQueue, l.adoptQSpare[:0]
	l.adoptQPending.Store(0)
	l.adoptQMu.Unlock()
	for _, it := range l.adoptQSpare {
		l.attachAdoptedFD(ctx, it.fd, it.carry, now)
	}
	// Same guard as the detach queue: adoptItem carries a Carryover with
	// the peer address string, so stale slots pin it after the handoff.
	clear(l.adoptQSpare)
	l.adoptQSpare = l.adoptQSpare[:0]
}

// refuseAdopt finishes a target-side adoption this loop cannot complete: the
// descriptor is outside the conn table, or epoll_ctl refused it. The source
// has already dropped the conn from its live gauge WITHOUT firing
// OnDisconnect, so if this engine simply closed the descriptor — which is
// what every one of these branches used to do — the connection would vanish
// from accepted - closed - active for good, with no hook and no counter
// (celeris#624). Close it AND fire the hook, so the lifecycle ledger balances
// on the engine that actually ended the connection.
//
// activeConns is deliberately untouched: this conn never entered this
// engine's gauge, and the source already decremented its own.
func (l *Loop) refuseAdopt(fd int, carry engine.Carryover) {
	if l.transplantAdoptRefused != nil {
		l.transplantAdoptRefused.Add(1)
	}
	if fd >= 0 {
		_ = unix.Shutdown(fd, unix.SHUT_WR)
		_ = unix.Close(fd)
	}
	if l.closeCount != nil {
		l.closeCount.Add(1)
	}
	if l.cfg.OnDisconnect != nil {
		l.cfg.OnDisconnect(carry.RemoteAddr)
	}
}

// attachAdoptedFD registers a transplanted real fd onto this loop as a fresh
// HTTP/1 connection. Mirrors acceptAll's per-conn setup. Runs on the loop thread
// (epoll_ctl + connState setup are loop-owned). The fd is assumed to be at a
// clean HTTP/1 request boundary; any pending request bytes wait in the kernel
// socket receive buffer and are picked up by the EPOLLIN edge after registration.
func (l *Loop) attachAdoptedFD(ctx context.Context, fd int, carry engine.Carryover, now int64) {
	if fd < 0 {
		l.refuseAdopt(fd, carry)
		return
	}
	if fd >= connTableSize {
		l.errs.ConnTableCap.Add(1)
		l.refuseAdopt(fd, carry)
		return
	}
	if fd >= len(l.conns) {
		l.growConns(fd)
	}
	// The slot is read and, below, filled under driverMu: an async Hijack on
	// this loop clears a slot from its dispatch goroutine under this lock and
	// then closes the descriptor, whose number the kernel may reissue to the
	// connection being adopted here (celeris#668).
	l.driverMu.Lock()
	occupied := l.conns[fd] != nil
	l.driverMu.Unlock()
	if occupied {
		// Slot occupied — the source detached fd before handing it off, so this
		// should not happen; refuse rather than clobber a live conn. Do not close
		// (the slot holder may close the same descriptor later). The connection
		// is lost here with no close and no hook, so count it separately from
		// the generic error total (celeris#624).
		l.errs.TransplantAdopt.Add(1)
		if l.transplantSlotOccupied != nil {
			l.transplantSlotOccupied.Add(1)
		}
		return
	}

	_ = sockopts.ApplyFD(fd, l.sockOpts)
	if err := unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_ADD, fd, &unix.EpollEvent{
		Events: unix.EPOLLIN | unix.EPOLLET | unix.EPOLLRDHUP,
		Fd:     int32(fd),
	}); err != nil {
		l.errs.ConnRegister.Add(1)
		l.refuseAdopt(fd, carry)
		return
	}

	cs := acquireConnState(ctxkit.WithWorkerID(ctx, l.id), fd, l.resolved.BufferSize, l.async)
	cs.remoteAddr = carry.RemoteAddr
	l.driverMu.Lock()
	l.conns[fd] = cs
	l.driverMu.Unlock()
	l.addLiveConn(cs)
	l.connCount++
	if fd > l.maxFD {
		l.maxFD = fd
	}
	cs.writeFn = l.makeWriteFn(cs)
	l.activeConns.Add(1)
	// Counted on the same statement as the gauge increment, and fired with
	// no OnConnect (the source already counted this conn when it accepted
	// it), so TransplantAdopted is the exact partner of the source's
	// TransplantDetached (celeris#624).
	if l.transplantAdopted != nil {
		l.transplantAdopted.Add(1)
	}
	cs.lastActivity = now

	// #383 adopts HTTP/1 keep-alive conns only; lock the protocol and install a
	// fresh parser at the request boundary.
	cs.protocol = engine.HTTP1
	cs.detected = true
	l.initProtocol(cs)

	// Replay any carried pipelined NEXT-request bytes through the fresh parser.
	// They get exactly what drainRead would give them had this loop read them
	// off the socket itself: in sync mode ProcessH1 inline (drainRead's inline
	// process→flush), and on an AsyncHandlers loop the per-route decision a
	// fresh conn's first read gets (replayCarriedAsync). Async mode used to
	// skip them, on the strength of the in-tree source never carrying bytes
	// under AsyncHandlers; the carry is public API (engine.TransplantTarget),
	// and a carry that did arrive was dropped, its client left waiting for an
	// answer to a request no handler saw (celeris#543).
	if len(carry.Buffered) > 0 {
		if l.async {
			l.replayCarriedAsync(cs, fd, carry.Buffered)
			return
		}
		if perr := conn.ProcessH1(cs.ctx, carry.Buffered, cs.h1State, l.handler, cs.writeFn); perr != nil {
			l.endCarriedReplay(cs, fd, perr)
			return
		}
		l.flushCarriedReplay(cs, fd)
	}
}

// replayCarriedAsync replays a transplant's carried bytes on an AsyncHandlers
// loop the way drainRead treats the first bytes a fresh async HTTP/1 conn
// reads (celeris#543): ProcessH1 runs inline in InlineMode, so sync routes are
// served here and ProcessH1 stops at the first async route (ErrAsyncDispatch);
// the conn is then promoted and that request, with whatever follows it, goes
// to the conn's dispatch goroutine. Each step mirrors drainRead's. Loop
// thread; the conn is fresh from attachAdoptedFD, so no dispatch goroutine
// exists yet and the loop owns cs.h1State until the promotion hands it over.
// The conn's EPOLLIN is already registered, so nothing is left to arm.
func (l *Loop) replayCarriedAsync(cs *connState, fd int, data []byte) {
	cs.h1State.InlineMode = true
	perr := conn.ProcessH1(cs.ctx, data, cs.h1State, l.handler, cs.writeFn)
	// A handler that hijacked inline has had cs released inside the Hijack
	// call, as in drainRead, so cs is not touched again after ErrHijacked.
	if errors.Is(perr, conn.ErrHijacked) {
		return
	}
	cs.h1State.InlineMode = false
	if errors.Is(perr, conn.ErrAsyncDispatch) {
		// Any response already written inline (a sync request ahead of the
		// async one) stays in cs.writeBuf and the dispatch path flushes it in
		// order, as in drainRead.
		cs.asyncPromoted = true
		l.asyncPromoted.Add(1)
		stashed := cs.h1State.TakeBufferedBytes()
		cs.asyncInMu.Lock()
		cs.asyncInBuf = append(cs.asyncInBuf, stashed...)
		starting := !cs.asyncRun
		if starting {
			cs.asyncRun = true
		}
		cs.asyncInMu.Unlock()
		if starting {
			l.asyncWG.Add(1)
			go l.runAsyncHandler(cs)
		} else {
			cs.asyncCond.Signal()
		}
		return
	}
	if perr != nil {
		// Nothing below may touch cs.h1State: an h2c upgrade has replaced it.
		l.endCarriedReplay(cs, fd, perr)
		return
	}
	// Inline served the bytes, but if they ended inside a request its
	// continuation must run on the dispatch goroutine, as in drainRead.
	if cs.h1State.HasPendingData() {
		cs.asyncPromoted = true
		l.asyncPromoted.Add(1)
	}
	l.flushCarriedReplay(cs, fd)
}

// endCarriedReplay finishes a replay whose ProcessH1 returned a non-nil
// verdict other than ErrHijacked and ErrAsyncDispatch, doing what drainRead
// does with the same verdict from a recv (celeris#908). Before, every such
// verdict closed the conn at once: closeConn does not send what is queued, so
// the 400 or 413 ProcessH1 had written (or the response to a request that
// said Connection: close) was dropped, and so was the 101 of an h2c upgrade,
// whose conn was closed instead of switched.
//
//   - ErrHijacked: the handler took the conn and cs is released; nothing to do.
//   - ErrUpgradeH2C: switch the conn to HTTP/2 and send the 101 with the
//     server preface and the answer to the upgrade request, as drainRead does
//     (its block is the model for the flush below; loop.go is not edited here).
//   - anything else (a parse error, errConnectionClose): flush what ProcessH1
//     queued, tell the detached middleware (OnError) and close once the
//     response has reached the kernel (closeWhenFlushed, celeris#761).
//
// Loop thread, on a conn fresh from attachAdoptedFD: no dispatch goroutine
// exists, so the only other holder of cs.detachMu is a guarded writeFn in one
// write. The lock is released before the close, which takes it again.
func (l *Loop) endCarriedReplay(cs *connState, fd int, perr error) {
	if errors.Is(perr, conn.ErrHijacked) {
		return
	}
	if errors.Is(perr, conn.ErrUpgradeH2C) {
		if err := l.switchToH2(cs, cs.writeFn); err != nil {
			l.closeConn(fd)
			return
		}
		if cs.writePos < len(cs.writeBuf) {
			if fErr := l.flushWrites(cs, true); fErr != nil {
				l.closeConn(fd)
				return
			}
			if cs.writePos >= len(cs.writeBuf) {
				cs.pendingBytes = 0
				if cs.dirty {
					l.removeDirty(cs)
				}
			} else {
				// The 101 and the preface did not all fit: the send buffer is
				// full. A newly-H2 conn is not detached, so arm EPOLLOUT
				// rather than the busy-polling dirty list.
				cs.pendingBytes = len(cs.writeBuf) - cs.writePos
				l.armEpollOut(cs)
			}
		}
		return
	}
	if mu := cs.detachMu; mu != nil {
		mu.Lock()
	}
	_ = l.flushWrites(cs, true)
	cs.pendingBytes = 0
	if cs.h1State != nil && cs.h1State.OnError != nil {
		cs.h1State.OnError(perr)
	}
	if mu := cs.detachMu; mu != nil {
		mu.Unlock()
	}
	l.closeWhenFlushed(cs)
}

// flushCarriedReplay sends the responses a replay's ProcessH1 queued, as
// drainRead's inline flush does after a recv (celeris#908). The replay used
// to flush once and stop: a response larger than the socket buffers left its
// rest in cs.writeBuf with no EPOLLOUT armed and the dirty list untouched
// (EPOLLIN is edge-triggered, and nothing reads from the socket again until
// the client sends), so the client got a truncated body and a stalled conn.
// It also left pendingBytes at the sum of the replayed responses, which the
// write hooks read as a backlog, and ignored a refused write.
//
// Loop thread, on a conn fresh from attachAdoptedFD. cs.detachMu is released
// before any close, which takes it again.
func (l *Loop) flushCarriedReplay(cs *connState, fd int) {
	mu := cs.detachMu
	if mu != nil {
		mu.Lock()
	}
	if csWritePending(cs) {
		if fErr := l.flushWrites(cs, true); fErr != nil {
			if cs.h1State != nil && cs.h1State.OnError != nil {
				cs.h1State.OnError(fErr)
			}
			if mu != nil {
				mu.Unlock()
			}
			if cs.dirty {
				l.removeDirty(cs)
			}
			l.closeConn(fd)
			return
		}
		if !csWritePending(cs) {
			cs.pendingBytes = 0
			if cs.dirty {
				l.removeDirty(cs)
			}
			l.disarmEpollOut(cs)
		} else {
			// The kernel send buffer is full: sync pendingBytes and arm
			// EPOLLOUT, which handleWritable carries on from (a pending
			// sendfile too). A truly detached conn keeps the dirty list.
			cs.pendingBytes = csPendingBytes(cs)
			if cs.h1State != nil && cs.h1State.Detached.Load() {
				l.markDirty(cs)
			} else {
				l.armEpollOut(cs)
			}
		}
	}
	refused := cs.writeRefused
	if mu != nil {
		mu.Unlock()
	}
	if refused {
		l.closeWhenFlushed(cs)
	}
}
