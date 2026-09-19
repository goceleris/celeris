//go:build linux

package iouring

import "sync/atomic"

// handoffLossStats are the celeris#657 witnesses: the request loss a
// reverse (io_uring→epoll) hand-off can cause, counted where it happens.
// Exported through engine.EngineMetrics.
//
// The #383 hand-off detaches a connection with its recv still armed: the
// dup'd descriptor goes to epoll, the original is closed, and the armed
// recv is cancelled. Closing a descriptor does not end an io_uring recv
// (the op holds its own file reference), so until that cancel lands the
// recv is still reading the socket epoll now owns — and if the recv SQE
// had not reached the kernel yet, it resolves the fd NUMBER at submit
// time, which a later dup may already have reused for another connection.
// Either way a recv that completes with data has consumed a request some
// client is still waiting on. Its CQE carries the old (fd, generation), so
// staleConnCQE drops it as stale, and nothing counted it: the #624
// hand-off ledger balances exactly while requests go missing.
//
//   - staleRecvData{Closed,Transplanted,Unattributed}: a stale recv CQE
//     with res > 0 — bytes that were read from some socket and thrown
//     away. Split by what Worker.closedOps holds for the CQE's (fd,
//     generation) identity at that moment: a connection this worker
//     closed or hijacked, one it handed off (the celeris#657 loss), or
//     nothing registered. Counted once per CQE, so a multishot recv's
//     intermediate F_MORE completions count too.
//
//     Closed is not proof of a benign race. hijackConn and the close
//     paths all register through noteClosedInflight. After hijackConn
//     the socket lives on under the hijacker's net.Conn, so a recv that
//     completes with data before its cancel lands has read bytes from a
//     connection its client is still using. After a close, a recv that
//     had not reached the kernel yet resolves the fd NUMBER when it does,
//     and a new connection may hold that number by then.
//
//     A stale CQE whose (fd, generation) equals the fd's current
//     occupant's (a generation collision, which needs the process-wide
//     32-bit connGenSeq to wrap while the op is in flight) is taken by
//     staleConnCQE's live branch and counted in none of the three. That
//     residual predates these counters (see the KNOWN RESIDUAL note in
//     staleConnCQE).
//
//   - handoffInFlight: a hand-off (tryTransplant or finishAsyncTransplant)
//     that detached a connection with recvArmed, a kernel op outstanding
//     (kernelInflight != 0) or a SEND_ZC notification pending — the
//     precondition of every loss above.
//
// The fd-lifetime rule that removes that loss (celeris#657 PR-2: no hand-off
// while a read can still resolve the fd) keeps its own counters here too:
//
//   - held: responses flushed with the next recv HELD, because a drain was
//     set and the conn was one the hand-off at that SEND's completion
//     accepts (HOLD). A rate.
//   - reaps / reapMisses: reported cancels of an armed recv submitted so a
//     conn could be handed off (REAP), and those whose own completion said
//     they matched nothing (the recv had completed, or was not issued yet).
//     A miss is retried, never followed by a hand-off. Rates.
//   - reapFailed: reaps whose completion was neither a hit nor a miss (for
//     example -EINVAL from a kernel that rejects the cancel flags the
//     startup probe found accepted). Not retried and never followed by a
//     hand-off. Must stay 0.
//   - reapUnsupported: reaps not placed because this kernel rejects the
//     IORING_ASYNC_CANCEL flags a reap needs (probeAsyncCancelFlags; they
//     exist from 5.19). The conn stays until its recv completes on its own.
//     A rate, 0 on every kernel from 5.19.
//   - holdRescued: held conns the timeout sweep found with their response
//     sent, not handed off and no recv armed — a path that skipped the
//     release. The belt under releaseHold. Must stay 0.
//   - doubleClaim: hand-offs refused at finishAsyncTransplant because the
//     connState no longer owns its fd slot: something moved it out of the
//     table since its dispatch goroutine claimed the hand-off, and in async
//     mode that something is another hand-off of the same conn (a close
//     marks the queued claim detachClosed first, and hijack is refused).
//     Before the checks existed one identity was measured moving twice in
//     167 runs. Must stay 0.
//   - claimDeferred: tryTransplant finding a conn whose dispatch goroutine
//     has claimed its own hand-off (transplantPending) and leaving it to that
//     claim. Counted before tryTransplant's other gates, so it is ordering,
//     not a fault: it fires whenever a completion of the conn (its own
//     response SEND, typically) lands between the goroutine's park and the
//     drain of its claim. A rate.
//
// All are direct atomic adds: they fire on the stale-CQE, drain and hand-off
// paths only, never on the per-request path while no drain is set, and like
// the celeris#586 witnesses they are per-event invariants that a
// per-iteration batch could lose at loop exit.
type handoffLossStats struct {
	staleRecvDataClosed       atomic.Uint64
	staleRecvDataTransplanted atomic.Uint64
	staleRecvDataUnattributed atomic.Uint64
	handoffInFlight           atomic.Uint64
	held                      atomic.Uint64
	reaps                     atomic.Uint64
	reapMisses                atomic.Uint64
	holdRescued               atomic.Uint64
	doubleClaim               atomic.Uint64
	claimDeferred             atomic.Uint64
	reapFailed                atomic.Uint64
	reapUnsupported           atomic.Uint64
}

// The fd-lifetime counters are nil-safe: a hand-built test Worker has none.

func (s *handoffLossStats) noteHeld() {
	if s != nil {
		s.held.Add(1)
	}
}

func (s *handoffLossStats) noteReap() {
	if s != nil {
		s.reaps.Add(1)
	}
}

func (s *handoffLossStats) noteReapMiss() {
	if s != nil {
		s.reapMisses.Add(1)
	}
}

func (s *handoffLossStats) noteHoldRescued() {
	if s != nil {
		s.holdRescued.Add(1)
	}
}

func (s *handoffLossStats) noteDoubleClaim() {
	if s != nil {
		s.doubleClaim.Add(1)
	}
}

func (s *handoffLossStats) noteClaimDeferred() {
	if s != nil {
		s.claimDeferred.Add(1)
	}
}

func (s *handoffLossStats) noteReapFailed() {
	if s != nil {
		s.reapFailed.Add(1)
	}
}

func (s *handoffLossStats) noteReapUnsupported() {
	if s != nil {
		s.reapUnsupported.Add(1)
	}
}

// noteStaleRecvData counts one stale recv CQE that carried data, under the
// class of its (fd, generation) identity. Must run BEFORE
// noteStaleTerminalOp, which deletes the closedOps entry once the kernel
// owes it nothing — after that, every identity would read as unattributed.
// Worker thread only (closedOps is worker-owned).
func (w *Worker) noteStaleRecvData(ud uint64) {
	s := w.handoffLoss
	if s == nil {
		return
	}
	e := w.closedOps[connOpKey(ud)]
	switch {
	case e == nil:
		s.staleRecvDataUnattributed.Add(1)
	case e.handoff:
		s.staleRecvDataTransplanted.Add(1)
	default:
		s.staleRecvDataClosed.Add(1)
	}
}

// noteHandoffInFlight counts a hand-off that detached cs while the kernel
// still held, or was about to be handed, an op on its descriptor. Called at
// both hand-off sites after the hand-off is committed and before the close
// path's cancels are queued. Worker thread only.
//
// When the accounting is consistent, kernelInflight != 0 alone would do.
// Every arm that sets recvArmed (prepareRecv, flushSendLink's linked recv)
// also counts the recv in kernelInflight. A SEND_ZC keeps its count until
// its notification CQE, and that CQE is also what clears zcNotifPending.
// The other two terms stay because that accounting has one known way to
// go wrong. A generation-collision misroute (the KNOWN RESIDUAL in
// staleConnCQE) can take kernelInflight to 0 while a recv is still armed
// or a notification is still pending. The three terms are also the
// condition the celeris#657 fix refuses a hand-off on, so this counts
// exactly the hand-offs that fix removes. The unit tests check each term
// on its own.
func (w *Worker) noteHandoffInFlight(cs *connState) {
	if w.handoffLoss == nil {
		return
	}
	if cs.recvArmed || cs.kernelInflight != 0 || cs.zcNotifPending {
		w.handoffLoss.handoffInFlight.Add(1)
	}
}

// noteHandedOffInflight is noteClosedInflight for a connection detached
// for a hand-off rather than closed: it registers the same closedOps entry
// (so the kernel accounting and the release gate are unchanged) and marks
// it as a hand-off, so a stale data CQE for this identity is attributed to
// the hand-off and not to a close. Under an (fd, generation) collision the
// entry is shared, and one hand-off among the colliding conns marks it.
func (w *Worker) noteHandedOffInflight(cs *connState) {
	w.noteClosedInflight(cs)
	if cs.kernelInflight <= 0 {
		return // noteClosedInflight registered nothing
	}
	if e := w.closedOps[encodeConnOpKey(cs.fd, cs.generation)]; e != nil {
		e.handoff = true
	}
}
