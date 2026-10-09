//go:build linux

package iouring

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/ctxkit"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/sockopts"
)

// AdoptConn implements [engine.TransplantTarget] (#383). The source engine
// (epoll) has ALREADY detached fd from its own event loop before calling this,
// so fd is owned by no engine at this instant; any pending request bytes wait in
// the kernel socket receive buffer. AdoptConn routes the fd to one worker
// (round-robin) and schedules the attach on that worker's own thread via the
// existing driver-action wakeup primitive (eventfd), where SQE submission is
// single-issuer-safe.
//
// Safe to call from any goroutine. On a returned error the caller still owns fd.
func (e *Engine) AdoptConn(fd int, carry engine.Carryover) error {
	if fd < 0 || fd >= fixedFileTableSize {
		return fmt.Errorf("celeris/iouring: adopt fd %d out of range [0,%d)", fd, fixedFileTableSize)
	}
	// e.workers is published under e.mu by Listen; read it the same way the
	// other post-Listen accessors (NumWorkers/WorkerLoop) do.
	e.mu.Lock()
	n := len(e.workers)
	if n == 0 {
		e.mu.Unlock()
		return fmt.Errorf("celeris/iouring: no workers available to adopt fd %d", fd)
	}
	w := e.workers[int(e.metrics.transplantRR.Add(1)-1)%n]
	e.mu.Unlock()

	// addAdoptAction takes the worker's own lock and wakes it, parked or not;
	// safe to call outside e.mu. It refuses once the worker has shut down, and
	// the caller then still owns fd (celeris#658).
	return w.addAdoptAction(fd, carry)
}

// closeAdoptQueue refuses every adoption still queued when the worker shuts
// down, and every AdoptConn after it. Worker.shutdown calls it first, on the
// worker thread.
//
// A queued adoption is a connection the source has ALREADY let go of:
// AdoptConn returned nil, so the source dropped it from its live gauge with no
// OnDisconnect. Worker.shutdown only walks the conn table, so an adopt still in
// the driver-action queue used to outlive the engine — descriptor open, owned
// by nobody (celeris#658). The wakeup does not make that state unreachable: a
// worker woken out of its park checks its context at the top of the next
// iteration, before drainDriverActions. So each queued adopt is finished here
// like any other adoption this worker cannot complete (refuseAdopt: close,
// count, fire the hook), and adoptClosed, set under the same lock, turns a
// later AdoptConn into an error the source can reclaim from.
//
// The other driver actions stay queued exactly as before. The only
// descriptor they can reach that the engine owns is the duplicate each
// registered driver conn holds (celeris#691), and shutdownDrivers, which
// runs next, retires every registered driver conn, closing it, and fires
// its onClose. A driver closes its own fds.
func (w *Worker) closeAdoptQueue() {
	w.driverActionMu.Lock()
	w.adoptClosed = true
	var adopts []driverAction
	kept := w.driverActionQueue[:0]
	for _, a := range w.driverActionQueue {
		if a.kind == driverActionAdopt {
			adopts = append(adopts, a)
			continue
		}
		kept = append(kept, a)
	}
	clear(w.driverActionQueue[len(kept):])
	w.driverActionQueue = kept
	if len(kept) == 0 {
		w.driverActionPending.Store(0)
	}
	w.driverActionMu.Unlock()
	for _, a := range adopts {
		w.refuseAdopt(a.adoptFD, a.adoptCarry)
	}
}

// TransplantCount returns the cumulative number of connections this engine has
// adopted from another engine via AdoptConn (#383). Zero unless the transplant
// feature flag is active.
func (e *Engine) TransplantCount() uint64 { return e.metrics.transplantCount.Load() }

var _ engine.TransplantTarget = (*Engine)(nil)

// refuseAdopt finishes a target-side adoption this worker cannot complete
// (the descriptor is outside its conn table). The source already dropped the
// conn from its live gauge with NO OnDisconnect, so a bare close here loses
// it from accepted - closed - active for good, with no hook and no counter —
// one of the silent drop points of celeris#624. Close it AND fire the hook.
//
// activeConns is deliberately untouched: the conn never entered this
// engine's gauge, and the source already decremented its own.
func (w *Worker) refuseAdopt(fd int, carry engine.Carryover) {
	if w.transplantAdoptRefused != nil {
		w.transplantAdoptRefused.Add(1)
	}
	if fd >= 0 {
		_ = unix.Shutdown(fd, unix.SHUT_WR)
		_ = unix.Close(fd)
	}
	if w.closeCount != nil {
		w.closeCount.Add(1)
	}
	if w.cfg.OnDisconnect != nil {
		w.cfg.OnDisconnect(carry.RemoteAddr)
	}
}

// attachAdoptedFD runs on the worker thread (via drainDriverActions) and turns a
// real, already-connected fd into a full HTTP/1 connection on this worker. It
// mirrors the non-fixed-file path of onAcceptedFD. The fd is assumed to be at a
// clean HTTP/1 request boundary (the source engine only hands off conns with no
// pipelined leftover and a fully-flushed response), so no protocol state needs
// to transfer: a fresh H1State is installed and the next request — already in
// the kernel socket receive buffer — is served by the multishot recv armed here.
func (w *Worker) attachAdoptedFD(newFD int, carry engine.Carryover) {
	if newFD < 0 || newFD >= len(w.conns) {
		w.errs.ConnTableCap.Add(1)
		w.refuseAdopt(newFD, carry)
		return
	}
	if w.conns[newFD] != nil {
		// Slot already occupied — impossible in practice (the source detached
		// the fd before handing it off, and io_uring nils w.conns[fd] at close,
		// so a reused fd finds an empty slot). Refuse rather than clobber the
		// slot holder; do NOT close the fd here, since the slot holder may close
		// the same descriptor number later and a double-close could hit an
		// unrelated reused fd. Counts as an error; never observed in testing.
		//
		// "Never observed" was only ever true of the generic ErrorCount it
		// bumps, which every other error path shares. The connection is
		// lost here with no close and no hook, so it gets its own counter
		// (celeris#624) and the claim becomes checkable.
		w.errs.TransplantAdopt.Add(1)
		if w.transplantSlotOccupied != nil {
			w.transplantSlotOccupied.Add(1)
		}
		return
	}

	// Re-apply socket options on the worker's own thread (the fd is a real,
	// non-fixed-file socket). Harmless if epoll already set them.
	_ = sockopts.ApplyFD(newFD, w.sockOpts)

	bufSize := w.resolved.BufferSize
	if w.bufRing != nil {
		bufSize = 0
	}
	ctx := w.runCtx
	if ctx == nil {
		ctx = context.Background()
	}
	cs := acquireConnState(ctxkit.WithWorkerID(ctx, w.id), newFD, bufSize, w.async)
	cs.fixedFile = false
	cs.remoteAddr = carry.RemoteAddr

	w.conns[newFD] = cs
	w.connCount++
	w.addLiveConn(cs)
	if newFD > w.maxFD {
		w.maxFD = newFD
	}
	cs.writeFn = w.makeWriteFn(cs)
	w.activeConns.Add(1)
	if w.transplantCount != nil {
		w.transplantCount.Add(1)
	}
	// A fresh clock, not w.cachedNow: an adoption is drained after the CQE
	// batch, which is what reads cachedNow, and in an iteration that carried
	// none cachedNow is as old as the wait before it, up to 1 s on a draining
	// worker, or the whole park (celeris#713). checkTimeouts reads a stamp's
	// age as idle time. The adoption's eventfd wake is normally a CQE of the
	// same iteration; this covers one drained without it. Adoptions come one
	// per handed-off connection, so the vDSO call is off the request path.
	cs.lastActivity = time.Now().UnixNano()

	// #383 adopts HTTP/1 keep-alive conns only; lock the protocol and install a
	// fresh parser at the request boundary.
	cs.protocol.Store(int32(engine.HTTP1))
	cs.detected = true
	w.initProtocol(cs)

	// Replay any carried pipelined NEXT-request bytes through the fresh parser
	// before arming the steady recv — mirrors handleRecv's process→flush. The
	// source guaranteed AtRequestBoundary, so these are whole/partial next
	// requests, never a mid-request continuation.
	//
	// They get exactly what handleRecv would give them had this worker read
	// them off the socket itself. In sync mode that is ProcessH1 inline. On an
	// AsyncHandlers worker it is the per-route decision a fresh conn's first
	// read gets (replayCarriedAsync), never a plain inline ProcessH1: that ran
	// an async route's handler on this worker thread, stalling every other
	// conn the worker owns for as long as the handler ran (celeris#543).
	if len(carry.Buffered) > 0 {
		if w.async {
			if !w.replayCarriedAsync(cs, newFD, carry.Buffered) {
				return // promoted (its recv is armed), hijacked, or closed
			}
		} else {
			if perr := conn.ProcessH1(cs.ctx, carry.Buffered, cs.h1State, w.handler, cs.writeFn); perr != nil {
				if !w.endCarriedReplay(cs, newFD, perr) {
					return // hijacked (handed off) or closed on error — nothing more to arm
				}
				// Switched to HTTP/2 (celeris#908): the 101 is sent, arm its recv below.
			} else if w.flushSend(cs) {
				w.markDirty(cs)
			}
		}
	}

	if !w.prepareRecv(cs, cs.buf) {
		cs.needsRecv = true
		w.markDirty(cs)
	}
}

// replayCarriedAsync replays a transplant's carried bytes on an AsyncHandlers
// worker the way handleRecv treats the first bytes a fresh async HTTP/1 conn
// receives (celeris#543): ProcessH1 runs inline in InlineMode, so sync routes
// are served here and ProcessH1 stops at the first async route
// (ErrAsyncDispatch); the conn is then promoted and that request, with
// whatever follows it, goes to the conn's dispatch goroutine. Each step
// mirrors the promotion block of handleRecv. Worker thread; the conn is fresh
// from attachAdoptedFD, so no dispatch goroutine exists yet and the worker
// owns cs.h1State until the promotion hands it over.
//
// It reports whether the caller should arm the conn's recv: false when the
// promotion has armed it (promoteConnToAsync arms the first recv for a
// completion without IORING_CQE_F_MORE, and none is armed yet), and when the
// conn was hijacked or closed.
func (w *Worker) replayCarriedAsync(cs *connState, fd int, data []byte) bool {
	cs.h1State.InlineMode = true
	perr := conn.ProcessH1(cs.ctx, data, cs.h1State, w.handler, cs.writeFn)
	if cs.h1State != nil {
		cs.h1State.InlineMode = false
	}
	if errors.Is(perr, conn.ErrAsyncDispatch) {
		// The route that forced the promotion, cloned out of the parser
		// buffer, for the celeris#364 revert (see handleRecv, celeris#631).
		if w.bufRing == nil {
			method, path := cs.h1State.CurrentRoute()
			cs.promotedMethod, cs.promotedPath = strings.Clone(method), strings.Clone(path)
		}
		cs.asyncPromoted.Store(true)
		w.asyncPromoted.Add(1)
		stashed := cs.h1State.TakeBufferedBytes()
		// A sync request served inline before the async one ships now, ahead
		// of the dispatch goroutine's responses (#300 L1).
		if len(cs.writeBuf) > 0 && !cs.sending && !cs.zcNotifPending {
			_ = w.flushSend(cs)
		}
		var first completionEntry // no IORING_CQE_F_MORE: no recv is armed yet
		w.promoteConnToAsync(cs, fd, stashed, &first)
		return false
	}
	if perr != nil {
		// Nothing below may touch cs.h1State: an h2c upgrade has replaced it.
		return w.endCarriedReplay(cs, fd, perr)
	}
	// Inline served the bytes, but if they ended inside a request (buffered
	// headers, a chunked body) its continuation must run on the dispatch
	// goroutine, as in handleRecv.
	if cs.h1State.HasPendingDispatchState() {
		cs.asyncPromoted.Store(true)
		w.asyncPromoted.Add(1)
	}
	if w.flushSend(cs) {
		w.markDirty(cs)
	}
	return true
}

// endCarriedReplay finishes a replay whose ProcessH1 returned a non-nil
// verdict other than ErrAsyncDispatch, doing what handleRecv does with the
// same verdict from a recv (celeris#908). It reports whether the caller
// should arm the conn's recv: true only for an h2c upgrade, whose conn goes on
// as HTTP/2.
//
//   - ErrHijacked: the handler took the conn; nothing to do.
//   - ErrUpgradeH2C: the 101 and the preface are queued, and the conn was
//     closed instead of switched. Switch it and send them. The send is a plain
//     one, unlinked: the caller arms the one recv, and a SEND linked to a
//     recv of its own would put a second recv on the fd (see handleRecv).
//   - anything else (a parse error, errConnectionClose): flush what ProcessH1
//     queued and tell the detached middleware (OnError) under detachMu, then
//     close, which defers behind the send until the response has gone out.
//
// Worker thread, on a conn fresh from attachAdoptedFD: no dispatch goroutine
// exists, so the only other holder of cs.detachMu is a guarded writeFn in one
// write. The lock is released before the close, which takes it again.
func (w *Worker) endCarriedReplay(cs *connState, fd int, perr error) bool {
	if errors.Is(perr, conn.ErrHijacked) {
		return false
	}
	if errors.Is(perr, conn.ErrUpgradeH2C) {
		if err := w.switchToH2(cs); err != nil {
			w.closeConn(fd)
			return false
		}
		if mu := cs.detachMu; mu != nil {
			mu.Lock()
		}
		cs.recvLinked = false
		if w.flushSend(cs) {
			w.markDirty(cs)
		}
		if mu := cs.detachMu; mu != nil {
			mu.Unlock()
		}
		return true
	}
	if mu := cs.detachMu; mu != nil {
		mu.Lock()
		_ = w.flushSend(cs)
		if cs.h1State != nil && cs.h1State.OnError != nil {
			cs.h1State.OnError(perr)
		}
		mu.Unlock()
	} else {
		_ = w.flushSend(cs)
	}
	w.closeConn(fd)
	return false
}
