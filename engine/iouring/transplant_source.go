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
// worker thread (after handleRecv). Mirrors hijackConn's detach (cancel the armed
// recv, defer the connState release to its terminal CQE) but hands a freshly
// dup'd non-blocking fd to epoll instead of wrapping it in a net.Conn. No-op for
// ineligible conns; retried at their next boundary.
func (w *Worker) tryTransplant(fd int) {
	h := w.transplant.Load()
	if h == nil {
		return
	}
	// Async-mode io_uring conns are owned by a dispatch goroutine; the prototype
	// transplants sync conns only (mirrors the epoll→io_uring async gate).
	if w.async {
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
	if engine.Protocol(cs.protocol.Load()) != engine.HTTP1 || !cs.detected || cs.h1State == nil {
		return
	}
	if cs.h1State.Detached.Load() || cs.h2State != nil {
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
