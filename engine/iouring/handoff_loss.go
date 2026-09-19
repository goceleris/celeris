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
//     closed (the peer's bytes raced a server-side close), one it handed
//     off (the celeris#657 loss), or nothing registered. Counted once per
//     CQE, so a multishot recv's intermediate F_MORE completions count
//     too.
//   - handoffInFlight: a hand-off (tryTransplant or finishAsyncTransplant)
//     that detached a connection with recvArmed, a kernel op outstanding
//     (kernelInflight != 0) or a SEND_ZC notification pending — the
//     precondition of every loss above.
//
// Both are direct atomic adds: they fire on the stale-CQE and hand-off
// paths only, never on the per-request path, and like the celeris#586
// witnesses they are per-event invariants that a per-iteration batch
// could lose at loop exit.
type handoffLossStats struct {
	staleRecvDataClosed       atomic.Uint64
	staleRecvDataTransplanted atomic.Uint64
	staleRecvDataUnattributed atomic.Uint64
	handoffInFlight           atomic.Uint64
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
