//go:build linux

package iouring

import (
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
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
//     no answer, or an answer it does not recognise, is treated the same.
//     Counted (TransplantReapUnsupported). Buffer rings arrived with the
//     flags, in 5.19, so a kernel that rejects them has no provided-buffer
//     ring and its sync conns are held; a newer kernel whose probe got no
//     answer (or an unrecognised one) may have one, and its conns stay. A
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
//
// One owner per hand-off (celeris#657 A6), as in tryTransplant: a goroutine
// that has claimed its hand-off (transplantPending) has exited too, so
// asyncRun alone reads "not running", but the conn belongs to that claim
// until drainDetachQueue takes it off the queue and runs
// finishAsyncTransplant itself. The goroutine enqueues the claim only after
// releasing asyncInMu, so a retry can run between the two, and a reap can
// land before the queue is drained. Acting then handed the conn off with its
// claim still set, and the claim's drain found the slot empty and counted a
// double claim for a conn moved once (celeris#758). Leaving the conn to its
// claim is ordering, counted as TransplantClaimDeferred.
//
// The goroutine's other exits have the same shape (celeris#780): the
// processErr and panic exits set asyncClosed, the h2c-upgrade exit makes the
// conn H2C, and each then clears asyncRun under asyncInMu and enqueues cs only
// after unlocking. A conn in one of those windows belongs to its queued entry,
// which closes it or finishes the upgrade; handing it off first left that
// entry to close the number the hand-off gave up, which the next accept can
// hold, or put an H2C conn on the HTTP/1 target. Both fields are published
// before asyncRun is cleared, so reading them under asyncInMu after seeing it
// clear is ordered.
func (w *Worker) rerunHandOff(fd int, cs *connState) {
	if w.async && cs.asyncPromoted.Load() {
		cs.asyncInMu.Lock()
		running := cs.asyncRun
		claimed := cs.transplantPending.Load()
		exiting := cs.asyncClosed.Load() || engine.Protocol(cs.protocol.Load()) != engine.HTTP1
		cs.asyncInMu.Unlock()
		if running {
			return
		}
		if claimed {
			w.handoffLoss.noteClaimDeferred()
			return
		}
		if exiting {
			return
		}
		w.finishAsyncTransplant(cs)
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

// The same rule for the close paths (celeris#685): a descriptor NUMBER is
// released only when no op that names it can still be submitted or issued.
//
// Fixed files are off (celeris#541), so a recv or send SQE names the
// descriptor by number, and the kernel resolves the number when it ISSUES the
// op, not when the SQE is written. Two kinds of op are not issued yet when a
// close path runs:
//
//   - one still in the SQ ring: prepareRecv (a promoted connection's re-arm,
//     a dirty-list retry) and flushSend only place SQEs, and they reach the
//     kernel at the loop's next io_uring_enter;
//   - one the kernel holds but has not issued: a recv linked behind a SEND
//     (flushSendLink) is issued only when the SEND completes, and on a
//     DEFER_TASKRUN ring as task work after that, which can still be queued
//     when the SEND's CQE is read.
//
// finishClose and finishCloseDetached used to queue the ops' cancels and
// close(2) the descriptor at once. Another thread (a sibling worker's accept,
// the epoll sub-engine) can be given the freed number before this worker's
// next submit. The recv then reads that connection's request and completes
// under the closed connection's (fd, generation); staleConnCQE drops it as
// stale_recv_data_closed, and the new connection waits on an empty socket
// until its header deadline. Measured deterministically by
// TestRecvTheft715ArmA (celeris#715), and the linked form by
// TestRecvTheft685Linked.
//
// The rule is kept by NOT closing while the kernel owes an op on the
// descriptor (fdOwed). The close path does everything else as before (the
// cancels, the closedOps registration, the deferred release) and hands the
// descriptor to its pendingRelease entry, and drainPendingRelease closes it
// once no owed op names it: where it releases the connState, when every owed
// op has delivered its terminal CQE, or earlier when all that is left is
// SEND_ZC notifications (celeris#798). Those name no descriptor and guard
// only the send buffer, so the connState waits for them and the descriptor
// does not (fdOps). Until then the number stays allocated, so no accept or dup
// anywhere in the process can be given it, and an op issued late resolves
// this connection's own socket. To make sure every owed op does end, the
// close path also shuts the socket's read side down (shutdownHow; SHUT_RD
// alone on the fast path, which otherwise makes no shutdown call, see
// fastCloseShutdownHow): an owed recv returns at once when it is issued, even
// one no cancel can find (a linked recv not issued yet), and even on a kernel
// whose cancels fail (celeris#682).
//
// What it costs. No io_uring_enter is added: the close(2) moves from the
// close path to the release, one iteration later, and a path that already
// shut the write side down shuts both down instead. The one added syscall is
// the SHUT_RD of the H1 fast path, taken only when an op is owed there: a
// server-side close of a connection with its recv armed (a timeout); a
// sync-mode Connection: close response has no recv armed when it closes, and
// neither has a client's FIN. What the peer sees barely moves. An issued recv
// holds its own reference to the file, so its socket was never released
// before that recv's cancel landed anyway, and the FIN (or the RST of unread
// data) went out then; it goes out at the same point now. Only a close with
// a recv still in the SQ ring, whose socket nothing held, used to release the
// socket at the close and now does so one enter later. The worker does not
// park while such a close is outstanding (closeFDOwed).
//
// A hijack keeps the socket open under the hijacker, so it cannot wait: it
// submits its cancels before handing the socket over (see hijackConn). Worker
// shutdown ends the ops owed on every descriptor before it closes any
// (endOwedOpsAtShutdown).

// fdOwed reports whether the kernel still owes cs an op that names its
// descriptor: a recv or send whose SQE was written and that has not
// completed (fdOps). A fixed-file connection names a slot, not a number, and
// keeps its own close (CLOSE_DIRECT). A nil cs (closeMissingConnState) owes
// nothing we can know of.
func fdOwed(cs *connState) bool {
	return cs != nil && fdOps(cs) > 0 && !cs.fixedFile
}

// fdOps counts the ops the kernel still owes live cs that name its
// descriptor. kernelInflight counts every recv and send from the moment its
// SQE is placed (prepareRecv, flushSend, flushSendLink, both halves of a
// linked pair), submitted or not, issued or not, and staleConnCQE retires it
// at the terminal CQE; the header timer and the cancels name no descriptor
// and are not counted.
//
// One op in that count may name nothing (celeris#798): a SEND_ZC whose send
// has completed (its first CQE, IORING_CQE_F_MORE, set zcNotifPending) keeps
// its count until the notification CQE, and the notification is not an op on
// the descriptor. It says the kernel has let go of the send buffer's pages,
// which a stalled peer's unread data can hold for as long as the socket
// lives. Counting it kept the descriptor, and so the socket, open until the
// 5 s release backstop forced it (CloseFDForced). So it is left out here,
// and only here: kernelInflight still counts it, and the connState, whose
// sendBuf those pages are, is still released only at the notification
// (drainPendingRelease). A connection has one send in flight at most
// (flushSend and flushSendLink wait out cs.sending and cs.zcNotifPending),
// so zcNotifPending stands for exactly one op.
//
// Live connections only: a closed one's count moves in its closedOps entry
// (closedOpsEntry.fdOps, read by closedFDNamed).
func fdOps(cs *connState) int32 {
	if cs.zcNotifPending {
		return cs.kernelInflight - 1
	}
	return cs.kernelInflight
}

// closedFDNamed reports whether an op the kernel still owes closed cs names
// its descriptor, as drainPendingRelease asks of an entry that kept the
// descriptor (holdsFD) while kernelInflight is not yet 0. Only a SEND_ZC
// notification names none (see fdOps), and only a connection whose last send
// was armed as SEND_ZC (sendIsZC, which nothing changes after the close) can
// owe one; for any other the answer is yes without a lookup. For that one,
// its closedOps entry counts what is left (fdOps). No entry means the
// accounting cannot say, and the descriptor stays kept.
func (w *Worker) closedFDNamed(cs *connState) bool {
	if !cs.sendIsZC {
		return true
	}
	e := w.closedOps[encodeConnOpKey(cs.fd, cs.generation)]
	return e == nil || e.fdOps > 0
}

// releaseKeptFD closes the descriptor a close path left to e (holdsFD): the
// release closes it when cs goes, or earlier, once only a SEND_ZC
// notification is still owed (celeris#798). Worker thread only.
func (w *Worker) releaseKeptFD(e *pendingReleaseEntry) {
	_ = unix.Close(int(e.fd))
	e.holdsFD = false
	w.closeFDOwed--
}

// keptFD is the descriptor a close path hands to its pendingRelease entry:
// fd when an op is owed, else -1 (the close path closes it at once).
func keptFD(fd int, owed bool) int {
	if owed {
		return fd
	}
	return -1
}

// closeUnlessOwed is a close path's last step: close(2) now when nothing is
// owed; otherwise leave the descriptor to its pendingRelease entry, which
// drainPendingRelease closes at the last owed op's terminal CQE.
func closeUnlessOwed(fd int, owed bool) {
	if !owed {
		_ = unix.Close(fd)
	}
}

// shutdownHow is the shutdown(2) of a close path that half-closes: SHUT_WR
// (the FIN goes out now) as before, and the read side too while an op is
// owed, so the owed recv ends as soon as the kernel issues it.
func shutdownHow(owed bool) int {
	if owed {
		return unix.SHUT_RDWR
	}
	return unix.SHUT_WR
}

// fastCloseShutdownHow is the shutdown(2) the H1 fast path adds when an op is
// owed: SHUT_RD, which sends nothing, so the close(2) at the release still
// decides between FIN and RST as the fast path always did. With a SEND still
// owed (the closing-drain sweep reaps a conn whose SEND never completed) the
// write side goes too: a blocked SEND then fails at once, where a cancel
// alone cannot end it on a kernel whose cancels fail (celeris#682).
func fastCloseShutdownHow(cs *connState) int {
	if cs.sending || cs.zcNotifPending {
		return unix.SHUT_RDWR
	}
	return unix.SHUT_RD
}

// shutdownFDDrainNanos bounds how long worker shutdown waits for the ops still
// owed on connection descriptors (endOwedOpsAtShutdown). The cancels and the
// SHUT_RDWR end every one within the first enter or two on a healthy kernel;
// the bound is for a kernel that never answers, and matches the send drain
// before it (shutdownSendDrainNanos).
const shutdownFDDrainNanos int64 = int64(250 * time.Millisecond)

// endOwedOpsAtShutdown is worker shutdown's half of the rule: it ends every op
// the kernel still owes on a live connection's descriptor, and on a
// descriptor a close path left to its pendingRelease entry (holdsFD),
// and closes the latter. shutdown closes the live ones itself, after it.
//
// Without it, shutdown closed every descriptor and then the ring. An op whose
// SQE was still in the SQ ring was harmless there (nothing submits after the
// closes), and an issued op holds its own file. The exception is an op the
// kernel had consumed and not issued: a recv linked behind a SEND, or queued
// as task work when the SEND's CQE was read. Where the ring's teardown still
// issues such work, it resolves a number shutdown had already freed. So each
// live connection with an op owed (fdOwed) has its ops cancelled and its
// socket shut down both ways (a consumed recv then ends as soon as it is
// issued, a blocked SEND at once), and the ring is run until every such op
// has delivered its terminal CQE: recv and send completions are retired by
// staleConnCQE exactly as the loop retires them, and an accept's new
// descriptor, which nobody will serve, is closed. Everything the SQ ring still
// holds is submitted by the first enter, which is why this runs before
// shutdownDrivers closes the driver descriptors. Bounded by
// shutdownFDDrainNanos; past it (or on a ring error) the descriptors are
// closed anyway, as shutdown always did. Worker thread only; skipped under
// SQPOLL (no tier enables it) and without a ring.
//
// It waits for the ops that name a descriptor, not for SEND_ZC
// notifications (celeris#798, see fdOps): a stalled peer can hold one for as
// long as the socket is open, far past this bound. A live connection's
// SEND_ZC whose first CQE the drain reads names the descriptor no more
// either; handleSend, which records that in zcNotifPending, does not run
// here, so the drain keeps its own note (zcDone) and changes nothing on the
// connection, which the dispatch goroutine may still read.
func (w *Worker) endOwedOpsAtShutdown() {
	var owed []*connState
	for _, fd := range w.liveConns {
		cs := w.conns[fd]
		if cs == nil || !fdOwed(cs) {
			continue
		}
		owed = append(owed, cs)
	}
	var zcDone map[*connState]bool
	// named is fdOwed for the drain: fdOps, less a SEND_ZC it saw complete.
	named := func(cs *connState) bool {
		n := fdOps(cs)
		if zcDone[cs] {
			n--
		}
		return n > 0
	}
	pending := func() bool {
		for _, cs := range owed {
			if named(cs) {
				return true
			}
		}
		for i := range w.pendingRelease {
			if e := &w.pendingRelease[i]; e.holdsFD && e.cs.kernelInflight > 0 && w.closedFDNamed(e.cs) {
				return true
			}
		}
		return false
	}
	if w.ring != nil && !w.sqpoll && pending() {
		for _, cs := range owed {
			w.cancelConnOps(cs.fd, cs)
			_ = unix.Shutdown(cs.fd, unix.SHUT_RDWR)
		}
		deadline := time.Now().UnixNano() + shutdownFDDrainNanos
		for pending() && time.Now().UnixNano() < deadline {
			if err := w.ring.SubmitAndWaitTimeout(10 * time.Millisecond); err != nil {
				break
			}
			head, tail := w.ring.BeginCQ()
			for ; head != tail; head++ {
				c := w.ring.cqeAt(head)
				ud := c.UserData
				switch ud & udMask {
				case udRecv, udSend:
					fd := int(ud & fdMask)
					// A live connection's SEND_ZC completing (F_MORE marks
					// only that on a send; a connection has one send in
					// flight at most): noted, or its notification would be
					// waited for as an op on the descriptor. A closed
					// identity's is counted by staleConnCQE.
					if ud&udMask == udSend && cqeHasMore(c.Flags) && fd < len(w.conns) {
						if cs := w.conns[fd]; cs != nil && cs.generation == decodeGen(ud) && !cs.zcNotifPending {
							if zcDone == nil {
								zcDone = make(map[*connState]bool)
							}
							zcDone[cs] = true
						}
					}
					w.staleConnCQE(c, fd, ud)
				case udAccept:
					if c.Res >= 0 && !w.fixedFiles {
						_ = unix.Close(int(c.Res))
					}
				}
			}
			w.ring.EndCQ(head)
		}
	}
	for i := range w.pendingRelease {
		if e := &w.pendingRelease[i]; e.holdsFD {
			_ = unix.Close(int(e.fd))
			e.holdsFD = false
		}
	}
	w.closeFDOwed = 0
}
