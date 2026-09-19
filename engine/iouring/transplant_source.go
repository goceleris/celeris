//go:build linux

package iouring

import (
	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
)

// transplantTargetHolder publishes the io_uring→epoll transplant target on each
// worker while a drain is in progress (#383 reverse direction).
type transplantTargetHolder struct {
	target engine.TransplantTarget
}

// StartTransplant begins draining this engine's idle H1 keep-alive connections to
// target (an epoll engine) as each reaches a clean request boundary (#383 reverse
// direction). The adaptive engine calls this after reverting io_uring→epoll so the
// established conns migrate back to epoll instead of being stranded on the
// now-standby io_uring. Safe to call from any goroutine.
func (e *Engine) StartTransplant(target engine.TransplantTarget) {
	if target == nil {
		return
	}
	h := &transplantTargetHolder{target: target}
	e.mu.Lock()
	workers := e.workers
	e.mu.Unlock()
	for _, w := range workers {
		w.transplant.Store(h)
	}
}

// StopTransplant halts any in-progress drain (#383 reverse).
func (e *Engine) StopTransplant() {
	e.mu.Lock()
	workers := e.workers
	e.mu.Unlock()
	for _, w := range workers {
		w.transplant.Store(nil)
	}
}

// tryTransplant detaches an idle H1 conn from this worker and hands it to the
// epoll target when it is at a clean, fully-flushed request boundary with
// nothing in flight on its descriptor (celeris#657). Runs on the worker thread
// (after handleRecv/handleSend, and when a reap lands). A conn whose recv is
// armed is not moved: its recv is reaped first (fd_lifetime.go). Otherwise it
// mirrors hijackConn's detach but hands a freshly dup'd non-blocking fd to
// epoll instead of wrapping it in a net.Conn (handOff). No-op for ineligible
// conns; retried at their next boundary.
//
// This worker-side path handles SYNC conns (and async conns that never promoted,
// asyncRun==false — they run inline on the worker, so the worker owns their state).
// A PROMOTED async conn is owned by its per-conn dispatch goroutine; the worker
// must NOT seize it (its fields can change underneath, and waking it to quiesce
// races releaseConnState). Such conns transplant THEMSELVES from runAsyncHandler at
// their park boundary (see asyncTransplantEligible / finishAsyncTransplant), so the
// worker simply skips them here.
func (w *Worker) tryTransplant(fd int) {
	h := w.transplant.Load()
	if h == nil {
		return
	}
	if fd < 0 || fd >= len(w.conns) {
		return
	}
	cs := w.conns[fd]
	if cs == nil {
		return
	}
	// Fixed-file conns hold a table index, not a real fd — can't dup/hand to epoll.
	if cs.fixedFile {
		return
	}
	// Promoted async conn — owned by its dispatch goroutine, which self-transplants
	// at its own park boundary. Skip here to avoid racing it.
	//
	// One owner per hand-off (celeris#657, A6): a goroutine that has parked
	// and claimed its own hand-off (transplantPending) has exited too, so
	// asyncRun alone reads as "not running" and this path used to move the
	// conn as if it were sync — then drainDetachQueue's finishAsyncTransplant
	// moved it again, dup'ing a descriptor number the first move had closed
	// and the process had reused (measured: one identity moved twice, 20 us
	// apart, in 1 of 167 runs, the run with the only negative gauge). The
	// goroutine sets the claim and clears asyncRun under asyncInMu, so both
	// are read under it here.
	if w.async {
		cs.asyncInMu.Lock()
		running := cs.asyncRun
		claimed := cs.transplantPending.Load()
		cs.asyncInMu.Unlock()
		if running {
			return
		}
		if claimed {
			w.handoffLoss.noteDoubleClaim()
			return
		}
	}

	// Eligibility. Only a plain HTTP/1 keep-alive at a clean, fully-flushed boundary
	// is movable. Fields are worker-thread-only here (no dispatch goroutine), so the
	// reads are race-free.
	if engine.Protocol(cs.protocol.Load()) != engine.HTTP1 || !cs.detected || cs.h1State == nil {
		return
	}
	if cs.h1State.Detached.Load() || cs.h2State != nil || cs.asyncH2Promoted.Load() {
		return
	}
	// Response fully flushed (no pending send) + clean boundary + nothing buffered.
	if cs.sending || len(cs.sendBuf) > 0 || len(cs.writeBuf) > 0 {
		return
	}
	if !cs.h1State.AtRequestBoundary() || cs.h1State.HasPendingData() {
		return
	}
	// The fd-lifetime rule (celeris#657, R0): nothing that can still resolve
	// this descriptor may be in flight when it moves. The usual case is the
	// recv armed after the last response (its linked RECV, or the idle
	// conn's standalone one): reap it and hand off at its -ECANCELED.
	if cs.recvArmed || cs.kernelInflight != 0 || cs.zcNotifPending {
		if onlyRecvInFlight(cs) {
			w.startReap(cs)
		}
		return
	}

	w.handOff(cs, fd, h, false)
}

// handOff is the commit point both hand-off sites share (tryTransplant for a
// sync conn, finishAsyncTransplant for a promoted async one), reached only
// after the caller's gates have passed. It dups the fd for epoll, detaches the
// conn from io_uring the way hijackConn does (drop it from the live set and the
// conn table, cancel what is still armed, defer the connState release to the
// terminal CQEs), closes the ORIGINAL fd (the dup keeps the socket alive for
// epoll) and hands the dup over. A failed dup leaves the conn untouched and
// reports false; the hand-off is retried at the conn's next boundary.
//
// detachedRelease picks the release: the async site's dispatch goroutine may
// still be in its deferred recover()/Done() block referencing cs, so that site
// holds cs alive with no pool recycle until the kernel ops drain (mirrors
// finishCloseDetached).
func (w *Worker) handOff(cs *connState, fd int, h *transplantTargetHolder, detachedRelease bool) bool {
	newFD, err := unix.Dup(fd)
	if err != nil {
		return false
	}
	if serr := unix.SetNonblock(newFD, true); serr != nil {
		_ = unix.Close(newFD)
		return false
	}
	carry := engine.Carryover{RemoteAddr: cs.remoteAddr}

	// Unlink from the dirty list (celeris#527): the original fd is closed
	// below and its number may be re-accepted, so a dirty-loop retry would
	// call prepareRecv on a stranger's socket. For the async site this is the
	// one teardown path with no cs.sending guard at all, so it is the only
	// way a detached connState reaches the list with a SEND in flight -- the
	// immortal-entry case that pins the worker at 100% CPU (celeris#529).
	w.removeDirty(cs)
	w.removeLiveConn(cs)
	w.conns[fd] = nil
	w.connCount--
	w.activeConns.Add(-1)
	// Detach-for-transplant, not a close: no OnDisconnect fires (the conn
	// lives on under epoll), so the ledger entry is the only record of this
	// decrement (celeris#624). closeCount is deliberately NOT bumped here.
	// It used to be, which made EngineMetrics.CloseCount run one ahead of
	// the OnDisconnect count for every reverse transplant — and "engine
	// closes vs hook closes" is the exact discriminator celeris#624 uses to
	// tell a close that skipped its hook from a hand-off that was lost.
	// epoll's detachFromEpoll never counted a detach as a close; this now
	// matches it.
	if w.transplantDetached != nil {
		w.transplantDetached.Add(1)
	}
	// celeris#657 witnesses: a hand-off made with an op still in flight,
	// and the identity marked as handed off, so a stale recv that later
	// reads a request is counted as this hand-off's loss.
	w.noteHandoffInFlight(cs)
	w.cancelConnOps(fd, cs)
	w.noteHandedOffInflight(cs)
	if detachedRelease {
		w.queuePendingReleaseDetached(cs)
	} else {
		w.queuePendingRelease(cs)
	}
	_ = unix.Close(fd)

	if err := h.target.AdoptConn(newFD, carry); err != nil {
		w.reclaimTransplant(newFD, carry, err)
	}
	return true
}

// reclaimTransplant takes back a connection this worker had already
// relinquished for a hand-off epoll then refused. The dup'd descriptor is
// open, connected and at a clean HTTP/1 boundary, and the peer has noticed
// nothing, so re-adopting it here is strictly better than the bare
// unix.Close this replaces — which killed a healthy keep-alive and left
// accepted - closed - active permanently short by one, with no hook and
// nothing recorded (celeris#624). attachAdoptedFD owns every remaining
// failure mode, so this never drops the conn. Worker thread only; both call
// sites already are.
func (w *Worker) reclaimTransplant(newFD int, carry engine.Carryover, cause error) {
	if w.transplantHandoffRefused != nil {
		w.transplantHandoffRefused.Add(1)
	}
	if w.logger != nil {
		w.logger.Warn("transplant hand-off failed; reclaiming the conn onto io_uring (#383, celeris#624)",
			"worker", w.id, "fd", newFD, "remote", carry.RemoteAddr, "err", cause)
	}
	w.attachAdoptedFD(newFD, carry)
}

// asyncTransplantEligible reports whether a promoted async conn sitting at its park
// boundary can be handed to epoll (#383 reverse). Called by runAsyncHandler on the
// DISPATCH GOROUTINE, which owns cs.h1State / cs.h2State / cs.writeBuf between
// requests — so these reads are race-free there. It deliberately reads no
// worker-owned send fields (cs.sending / cs.sendBuf), which it cannot read safely
// from that goroutine. An empty writeBuf here does NOT prove the response is fully
// flushed — the partial-write path converts a direct write back into a ring SEND,
// which empties writeBuf while sendBuf is still in flight (celeris#529). The
// egress check is therefore re-done in finishAsyncTransplant, on the worker
// thread, and this predicate is only a cheap first filter.
func (w *Worker) asyncTransplantEligible(cs *connState) bool {
	if cs.fixedFile {
		return false
	}
	if engine.Protocol(cs.protocol.Load()) != engine.HTTP1 || !cs.detected || cs.h1State == nil {
		return false
	}
	if cs.h1State.Detached.Load() || cs.h2State != nil || cs.asyncH2Promoted.Load() {
		return false
	}
	if len(cs.writeBuf) != 0 {
		return false
	}
	return cs.h1State.AtRequestBoundary() && !cs.h1State.HasPendingData()
}

// enqueueDetach hands cs to the worker's detachQueue and wakes the worker via the
// h2 eventfd. Used by the dispatch goroutine to defer worker-owned work to the
// worker thread (SINGLE_ISSUER). Mirrors the asyncH2Promoted / asyncClosed enqueue.
func (w *Worker) enqueueDetach(cs *connState) {
	w.detachQMu.Lock()
	w.detachQueue = append(w.detachQueue, cs)
	w.detachQPending.Store(1)
	w.detachQMu.Unlock()
	w.wakeFD.Signal()
}

// finishAsyncTransplant completes a self-initiated async transplant on the WORKER
// thread (from drainDetachQueue, or when its reap lands), after the dispatch
// goroutine has marked the conn (transplantPending) and exited. The conn's recv
// is reaped first (celeris#657); with nothing in flight, handOff dups the fd for
// epoll, runs the worker-owned detach, closes the original, and hands the dup
// over. If the drain was stopped in the meantime, or the dup fails, it leaves
// the conn in place — its next recv respawns the dispatch goroutine (asyncRun is
// already false), so nothing is lost.
func (w *Worker) finishAsyncTransplant(cs *connState) {
	h := w.transplant.Load()
	if h == nil {
		return // drain stopped — leave the conn; next recv respawns its goroutine
	}
	// One owner per hand-off (celeris#657, A6): act only for the connState
	// that still owns its slot. If anything else moved or closed it since the
	// goroutine's claim, cs.fd is a number some other connection may hold by
	// now, and dup'ing it would hand that connection off.
	if cs.fd < 0 || cs.fd >= len(w.conns) || w.conns[cs.fd] != cs || cs.closing {
		w.handoffLoss.noteDoubleClaim()
		return
	}
	// Re-validate egress here, on the worker thread (celeris#529).
	//
	// asyncTransplantEligible runs on the DISPATCH goroutine and deliberately
	// reads no worker-owned send fields, inferring "the response is fully
	// flushed" from an empty writeBuf. That inference does not hold. The
	// direct-write path in runAsyncHandler sets partial on a short write or
	// EAGAIN, compacts the remainder BACK into cs.writeBuf and enqueues the
	// conn; drainDetachQueue markDirty's it, and the dirty loop's flushSend
	// then swaps writeBuf into sendBuf and submits a ring SEND. At that point
	// writeBuf is empty — exactly the condition read as "flushed" — while a
	// SEND is in flight against sendBuf.
	//
	// Transplanting then dups the fd for epoll and closes the original, so
	// that SEND's CQE arrives against a closed fd and is dropped by
	// staleConnCQE: cs.sending is never cleared (and the dirty loop skips a
	// sending conn — celeris#527), and the bytes may never have reached the
	// wire with nothing reporting it.
	//
	// sending / zcNotifPending / sendBuf are worker-owned, so reading them is
	// race-free HERE even though it would not have been in the caller. Leaving
	// the conn in place is the same fallback the dup-failure path takes: its
	// next recv respawns the dispatch goroutine, and the transplant is retried
	// at the following park boundary.
	if cs.sending || cs.zcNotifPending || len(cs.sendBuf) != 0 || len(cs.writeBuf) != 0 {
		return
	}
	// The fd-lifetime rule (celeris#657, R0), as in tryTransplant. This used
	// to cancel, dup and close with the recv still armed; a request that
	// recv took was lost (measured on main at an idle revert). The goroutine
	// has exited, so the recv the feed path armed for it is the one op the
	// conn can have: reap it, and come back here at its -ECANCELED. A
	// request that beats the cancel respawns the goroutine as usual.
	if cs.recvArmed || cs.kernelInflight != 0 || cs.zcNotifPending {
		if onlyRecvInFlight(cs) {
			w.startReap(cs)
		}
		return
	}
	w.handOff(cs, cs.fd, h, true)
}
