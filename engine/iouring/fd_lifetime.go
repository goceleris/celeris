//go:build linux

package iouring

import (
	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
)

// The fd-lifetime rule of the io_uring→epoll hand-off (celeris#657, face 2):
// a connection leaves io_uring only when no read can still resolve its
// descriptor.
//
// The hand-off dups the fd for epoll and closes the original. Closing does not
// end an io_uring recv (the op holds its own file reference), and a recv SQE
// that has not reached the kernel yet resolves the fd NUMBER when it does. So
// a recv left armed across the hand-off could take the client's next request
// off the socket epoll now owns, or, once a later dup or accept reused the
// number, another connection's request: measured, client errors == stale data
// CQEs, 1,707 == 1,707 in 48 of 48 runs, while the #624 ledger balanced.
//
// Two mechanisms keep the rule; both hand-off sites refuse otherwise.
//
//   - REAP. A connection whose recv is armed is not handed off. The worker
//     submits a REPORTED cancel of exactly that recv (matched on its
//     user_data, generation included, so it cannot touch the fd number's next
//     owner) under a tag of its own, and hands off at the recv's -ECANCELED —
//     the recv's own terminal completion, not the cancel's result. If the
//     request arrives first, the recv completes with it: it is served here
//     and its response is HELD (on a worker with a provided-buffer ring,
//     which has no HOLD, its next recv is reaped instead). If the cancel
//     reports that it matched nothing (the recv completed first, or is still
//     linked behind its SEND and not issued), the reap is retried on the
//     next loop iteration while the recv is still armed. A miss is never
//     followed by a hand-off, and neither is any other result: a cancel that
//     fails outright is counted and not retried. A reap needs
//     IORING_ASYNC_CANCEL flags (Linux 5.19); unless the startup probe found
//     them accepted (probeAsyncCancelFlags) none is placed, and the conn
//     stays until its recv completes on its own (startReap). None is placed
//     either after a hand-off of the conn failed at its dup, until the conn
//     next receives data.
//   - HOLD. While a drain is set, on a worker without a provided-buffer
//     ring, the response of a connection the hand-off would accept is
//     flushed with no recv behind it (not linked, not standalone). Its SEND
//     completion then finds nothing in flight and hands the conn off; the
//     client's next request waits in the socket buffer for epoll.
//     releaseHold arms the recv at that completion whenever the hand-off
//     does not happen, and checkTimeouts rescues (and counts) any held conn
//     a path left unreleased.

// onlyRecvInFlight reports whether the one op the R0 gate found in flight is
// the recv — the case REAP can clear. Anything else (a send, a SEND_ZC
// notification, an accounting the recv alone does not explain) refuses the
// hand-off until the conn's next completion.
func onlyRecvInFlight(cs *connState) bool {
	return cs.recvArmed && cs.kernelInflight == 1 && !cs.zcNotifPending
}

// startReap submits a reported cancel of cs's armed recv, unless a reap is
// already aimed at that recv. Worker thread only. A full SQ ring defers it to
// the next iteration's retry.
//
// Two conditions place no reap and queue no retry. The conn keeps its recv
// armed and stays until that recv completes on its own. A sync conn then has
// the request it brings served here; on a worker without a provided-buffer
// ring (HOLD needs none) its response is HELD and it leaves at that SEND's
// completion with nothing in flight, and on one with a ring it stays on
// io_uring. A promoted async conn, which is never held, stays on io_uring.
// Placement only: nothing is in flight when a conn moves, so no request can
// be lost. The conditions:
//   - the startup probe did not find IORING_ASYNC_CANCEL flags accepted
//     (probeAsyncCancelFlags): a kernel before 5.19 rejects them, and every
//     reap would fail with -EINVAL and leave the recv armed; a probe that got
//     no answer is treated the same. Counted (TransplantReapUnsupported).
//     Buffer rings arrived with the flags, in 5.19, so a kernel that rejects
//     them has no provided-buffer ring and its sync conns are held; a newer
//     kernel whose probe got no answer may have one, and its conns stay. A
//     promoted async conn never gets here on such a worker: its dispatch
//     goroutine makes no claim (asyncTransplantEligible) and stays parked,
//     still running, so tryTransplant leaves it alone too (celeris#681 R1).
//   - the conn's last hand-off failed at its dup (reapSuppressed): reaping
//     the recv re-armed after that failure would fail the same way at once.
//     Only the conn's next data lifts it, not the end of the drain; the
//     field's doc says what that does across drains.
func (w *Worker) startReap(cs *connState) {
	if !w.asyncCancelFlags {
		w.handoffLoss.noteReapUnsupported()
		return
	}
	if cs.reapSuppressed {
		return
	}
	if cs.transplantReap > 0 && !cs.reapStale {
		return // the armed recv already has a reap on its way
	}
	sqe := w.getCancelSQE()
	if sqe == nil {
		w.queueReapRetry(cs)
		return
	}
	prepCancelUserDataReported(sqe, encodeUserDataGen(udRecv, cs.fd, cs.generation))
	setSQEUserData(sqe, encodeUserDataGen(udTransplantReap, cs.fd, cs.generation))
	cs.transplantReap++
	cs.reapStale = false
	w.handoffLoss.noteReap()
}

// reapOutcome sees every recv completion of a conn with a reap outstanding
// (transplantReap > 0), first thing in handleRecv. It consumes the recv's
// -ECANCELED, which is the reap landing: the recv is gone and read nothing,
// so the hand-off runs again (the generic negative-result branch would close
// a healthy conn). A pause's cancel has precedence: a detached conn is never
// reaped, so both outstanding at once is only defensive. Any other terminal
// completion means the recv the reaps were aimed at completed by itself — a
// request, a FIN, an error — and is handled as usual; the reaps still counted
// now target a recv that is gone.
//
// Consuming the completion here means doing what handleRecv does for every
// recv completion it ends: a provided buffer the completion carries goes back
// to the ring (a cancelled recv consumed none, so this is defensive), and
// recvLinked, which only the chained recv's own completion clears, is cleared,
// since this completion is that recv's last.
func (w *Worker) reapOutcome(c *completionEntry, fd int, cs *connState) bool {
	if c.Res == -int32(unix.ECANCELED) && !cs.recvPaused && cs.recvCancelPending == 0 {
		if cqeHasBuffer(c.Flags) && w.bufRing != nil {
			w.bufRing.PushBuffer(cqeBufferID(c.Flags))
			w.hasBufReturns = true
		}
		cs.recvLinked = false
		cs.transplantReap--
		cs.reapStale = true
		w.rerunHandOff(fd, cs)
		// Not handed off (the drain stopped, a gate refused, the dup failed):
		// the conn stays and is served here, so it needs its recv back.
		if w.conns[fd] == cs && !cs.closing && !cs.recvArmed && !cs.recvPaused && !cs.transplantHold {
			if !w.prepareRecv(cs, w.pickRecvTarget(cs)) {
				cs.needsRecv = true
				w.markDirty(cs)
			}
		}
		return true
	}
	if !cqeHasMore(c.Flags) {
		cs.reapStale = true
	}
	return false
}

// handleTransplantReap processes a reap cancel's own completion. With
// IORING_ASYNC_CANCEL_ALL, res > 0 is a hit whose -ECANCELED reapOutcome
// retires, and -EALREADY means the recv was found executing and completes on
// its own, which is treated the same way (as handleRecvCancel treats it).
// res == 0 or -ENOENT is the miss: the only event that can report it, and
// the only one retried.
//
// Anything else is a cancel that failed, not an answer about the recv: the
// kernel matched nothing because it never looked. -EINVAL is what a kernel
// that rejects IORING_ASYNC_CANCEL flags returns (before 5.19; the startup
// probe keeps reaps off there). Retrying it placed the same failing cancel
// on every loop iteration for as long as the drain lasted. It is counted
// (TransplantReapFailed, which must stay 0), not retried, and not followed
// by a hand-off: the recv stays armed and the conn is served here until that
// recv completes on its own.
func (w *Worker) handleTransplantReap(c *completionEntry, fd int) {
	if fd < 0 || fd >= len(w.conns) {
		return
	}
	cs := w.conns[fd]
	if cs == nil {
		return
	}
	if c.Res > 0 || c.Res == -int32(unix.EALREADY) {
		return
	}
	miss := c.Res == 0 || c.Res == -int32(unix.ENOENT)
	if miss {
		w.handoffLoss.noteReapMiss()
	} else {
		w.handoffLoss.noteReapFailed()
	}
	if cs.transplantReap > 0 {
		cs.transplantReap--
	}
	if cs.transplantReap > 0 {
		return // another reap is still out; its own outcome decides
	}
	cs.reapStale = false
	if miss && cs.recvArmed && !cs.closing {
		w.queueReapRetry(cs)
	}
}

// rerunHandOff re-runs the hand-off for cs at the site that owns it: a
// promoted async conn (its dispatch goroutine has exited) through
// finishAsyncTransplant, anything else through tryTransplant. A promoted conn
// whose goroutine is running again owns itself and claims its own hand-off at
// its next park.
func (w *Worker) rerunHandOff(fd int, cs *connState) {
	if w.async && cs.asyncPromoted.Load() {
		cs.asyncInMu.Lock()
		running := cs.asyncRun
		cs.asyncInMu.Unlock()
		if !running {
			w.finishAsyncTransplant(cs)
		}
		return
	}
	w.tryTransplant(fd)
}

// queueReapRetry defers a reap to the next loop iteration. Keyed by (fd,
// generation), so a conn that closes in between, or whose number is reused,
// is skipped rather than touched.
func (w *Worker) queueReapRetry(cs *connState) {
	w.reapRetry = append(w.reapRetry, encodeConnOpKey(cs.fd, cs.generation))
}

// retryReaps re-runs the hand-off for every conn whose reap missed, or could
// not be placed, while its recv was still armed. Runs once per loop
// iteration, at the head of drainDetachQueue: after the io_uring_enter that
// processed the miss, so a linked recv that was not issued yet has been by
// now, and the new cancel finds it.
func (w *Worker) retryReaps() {
	keys := w.reapRetry
	w.reapRetry = w.reapRetrySpare[:0]
	for _, k := range keys {
		fd := int(k & fdMask)
		if fd >= len(w.conns) {
			continue
		}
		cs := w.conns[fd]
		if cs == nil || cs.generation != uint32((k&genMask)>>genShift) || cs.closing || !cs.recvArmed {
			continue
		}
		if w.transplant.Load() == nil {
			continue // the drain stopped: the recv simply stays armed
		}
		w.rerunHandOff(fd, cs)
	}
	w.reapRetrySpare = keys[:0]
}

// holdEligible reports whether cs's response, about to be flushed, can be
// sent with its next recv HELD: exactly the conns tryTransplant would hand off
// at that SEND's completion (plain HTTP/1 at a clean boundary, not detached,
// not H2, not a promoted async conn, nothing pending) and only when a SEND is
// coming, since that completion is what releases the hold. Worker thread,
// under cs.detachMu when the conn has one.
func (w *Worker) holdEligible(cs *connState) bool {
	if cs.fixedFile || cs.recvPaused || cs.closing {
		return false
	}
	if !cs.detected || cs.h1State == nil || engine.Protocol(cs.protocol.Load()) != engine.HTTP1 {
		return false
	}
	if cs.h1State.Detached.Load() || cs.h2State != nil || cs.asyncH2Promoted.Load() {
		return false
	}
	if w.async && cs.asyncPromoted.Load() {
		return false
	}
	if !cs.h1State.AtRequestBoundary() || cs.h1State.HasPendingData() {
		return false
	}
	return cs.sending || len(cs.writeBuf) > 0 || len(cs.sendBuf) > 0 || len(cs.bodyBuf) > 0
}

// releaseHold runs after the hand-off attempt at every send dispatch site
// (the inlined one and processCQE's), unconditionally: a conn held for a
// hand-off (transplantHold) whose response has been sent but that is still
// here — the drain stopped, a gate refused, the dup failed — gets its recv
// armed. No counter gates the call; a drifting one could skip a re-arm and
// leave a conn that cannot read. Small enough to inline: one slot load and
// one flag per send completion.
//
// The recv dispatch sites do not call it. A held conn has no recv armed:
// every path that arms one clears the hold first (releaseHoldSlow) or
// refuses a held conn (reapOutcome), so the only recv completion that finds
// a hold is the one whose own response just set it, with that SEND still in
// flight, and releaseHoldSlow would return at its egress check. Measured the
// same way: 60,559 recv-site entries with the hold set, none changed a thing.
func (w *Worker) releaseHold(fd int) {
	if cs := w.conns[fd]; cs != nil && cs.transplantHold {
		w.releaseHoldSlow(cs, false)
	}
}

// releaseHoldSlow clears cs's hold and arms its recv once nothing is left to
// send; while something is, the next SEND completion decides. rescued marks
// the timeout sweep's belt, which is counted. Worker thread.
func (w *Worker) releaseHoldSlow(cs *connState, rescued bool) {
	if cs.closing {
		cs.transplantHold = false
		return
	}
	if mu := cs.detachMu; mu != nil {
		// A held conn is never a promoted async one, so nothing contends
		// this; TryLock only because checkTimeouts never blocks on it.
		if !mu.TryLock() {
			return
		}
		defer mu.Unlock()
	}
	if cs.sending || cs.zcNotifPending || len(cs.sendBuf) > 0 || len(cs.writeBuf) > 0 || len(cs.bodyBuf) > 0 {
		return
	}
	cs.transplantHold = false
	if rescued {
		w.handoffLoss.noteHoldRescued()
	}
	if cs.recvArmed || cs.recvPaused {
		return
	}
	if !w.prepareRecv(cs, w.pickRecvTarget(cs)) {
		cs.needsRecv = true
		w.markDirty(cs)
	}
}

// rescueHold is checkTimeouts' belt under releaseHold: every path that
// completes a held conn's send releases it, so finding one here with its send
// done means a path skipped the release, and the conn could not have read
// until now. Arms the recv and counts it (TransplantHoldRescued, must stay 0).
func (w *Worker) rescueHold(cs *connState) {
	w.releaseHoldSlow(cs, true)
}
