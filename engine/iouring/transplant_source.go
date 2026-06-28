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
// epoll target when it is at a clean, fully-flushed request boundary. Runs on the
// worker thread (after handleRecv/handleSend). Mirrors hijackConn's detach (cancel
// the armed recv, defer the connState release to its terminal CQE) but hands a
// freshly dup'd non-blocking fd to epoll instead of wrapping it in a net.Conn.
// No-op for ineligible conns; retried at their next boundary.
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
	if w.async {
		cs.asyncInMu.Lock()
		running := cs.asyncRun
		cs.asyncInMu.Unlock()
		if running {
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

	// Dup the fd so epoll gets a clean, ops-free descriptor immediately while
	// io_uring cancels + drains its armed recv on the original.
	newFD, err := unix.Dup(fd)
	if err != nil {
		return
	}
	if serr := unix.SetNonblock(newFD, true); serr != nil {
		_ = unix.Close(newFD)
		return
	}
	carry := engine.Carryover{RemoteAddr: cs.remoteAddr}

	// Detach from io_uring (mirror hijackConn): drop from live set/conn table,
	// cancel the armed recv, defer the connState release to its terminal CQE, and
	// close the ORIGINAL fd (the dup keeps the socket alive for epoll).
	w.removeLiveConn(cs)
	w.conns[fd] = nil
	w.connCount--
	w.activeConns.Add(-1)
	w.closeCount.Add(1)
	w.cancelConnOps(fd, cs)
	w.noteClosedInflight(cs)
	w.queuePendingRelease(cs)
	_ = unix.Close(fd)

	if err := h.target.AdoptConn(newFD, carry); err != nil {
		_ = unix.Close(newFD)
	}
}

// asyncTransplantEligible reports whether a promoted async conn sitting at its park
// boundary can be handed to epoll (#383 reverse). Called by runAsyncHandler on the
// DISPATCH GOROUTINE, which owns cs.h1State / cs.h2State / cs.writeBuf between
// requests — so these reads are race-free there. It deliberately reads no
// worker-owned send fields (cs.sending / cs.sendBuf): in async mode the response is
// direct-written by the goroutine, so an empty writeBuf at the park boundary already
// proves the response is fully flushed.
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
	if w.h2EventFD >= 0 {
		var val [8]byte
		val[0] = 1
		_, _ = unix.Write(w.h2EventFD, val[:])
	}
}

// finishAsyncTransplant completes a self-initiated async transplant on the WORKER
// thread (from drainDetachQueue), after the dispatch goroutine has marked the conn
// (transplantPending) and exited. It dups the fd for epoll, runs the worker-owned
// detach (cancel the armed recv, defer the connState release to its terminal CQE),
// closes the original, and hands the dup to epoll. If the drain was stopped in the
// meantime, or the dup fails, it leaves the conn in place — its next recv respawns
// the dispatch goroutine (asyncRun is already false), so nothing is lost.
func (w *Worker) finishAsyncTransplant(cs *connState) {
	h := w.transplant.Load()
	if h == nil {
		return // drain stopped — leave the conn; next recv respawns its goroutine
	}
	fd := cs.fd
	newFD, err := unix.Dup(fd)
	if err != nil {
		return // can't dup — leave the conn; transplant retried at its next park
	}
	if serr := unix.SetNonblock(newFD, true); serr != nil {
		_ = unix.Close(newFD)
		return
	}
	carry := engine.Carryover{RemoteAddr: cs.remoteAddr}

	w.removeLiveConn(cs)
	w.conns[fd] = nil
	w.connCount--
	w.activeConns.Add(-1)
	w.closeCount.Add(1)
	w.cancelConnOps(fd, cs)
	w.noteClosedInflight(cs)
	// Detached release: the dispatch goroutine may still be in its deferred
	// recover()/Done() block referencing cs; hold cs alive (no pool recycle) until
	// the kernel recv drains. Mirrors finishCloseDetached.
	w.queuePendingReleaseDetached(cs)
	_ = unix.Close(fd)

	if err := h.target.AdoptConn(newFD, carry); err != nil {
		_ = unix.Close(newFD)
	}
}
