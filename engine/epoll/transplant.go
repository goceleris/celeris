//go:build linux

package epoll

import (
	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
)

// transplantState is published via Loop.transplant while a drain-to-io_uring is
// in progress (#383). Its non-nil-ness is the hot-path gate; target receives the
// detached connections.
type transplantState struct {
	target engine.TransplantTarget
}

// StartTransplant begins draining this engine's established HTTP/1 keep-alive
// connections to target (an io_uring engine) as each reaches a clean request
// boundary (#383). Idempotent and safe to call from any goroutine. The adaptive
// engine calls this after promoting epoll→io_uring so the pinned keep-alives
// migrate onto io_uring instead of being stranded on the now-standby epoll —
// the hollow-promotion forfeit documented in #396.
func (e *Engine) StartTransplant(target engine.TransplantTarget) {
	if target == nil {
		return
	}
	ts := &transplantState{target: target}
	for _, l := range e.loops {
		l.transplant.Store(ts)
	}
}

// StopTransplant halts any in-progress drain (#383). Called when the adaptive
// engine reverts to epoll, so freshly-accepted conns are not migrated away.
func (e *Engine) StopTransplant() {
	for _, l := range e.loops {
		l.transplant.Store(nil)
	}
}

// tryTransplant moves fd from this loop to the io_uring target when the conn is
// at a clean, fully-flushed HTTP/1 keep-alive boundary. Runs on the loop's own
// thread (after drainRead). A no-op for ineligible conns, which are retried at
// their next boundary. Three cases:
//   - sync conn: detach + hand off immediately, carrying any buffered pipelined
//     next-request bytes.
//   - async conn with NO dispatch goroutine (never promoted / already exited):
//     like sync, but only at a fully-clean boundary (no buffered bytes — async
//     re-injection is out of scope).
//   - async conn WITH a parked, fully-flushed dispatch goroutine: detach the fd,
//     ask the goroutine to quiesce (exit), and finish the hand-off in
//     drainDetachQueue once it has exited. A goroutine that is mid-ProcessH1 or
//     has pending input/backpressure is skipped (retried when it next parks).
func (l *Loop) tryTransplant(fd int) {
	ts := l.transplant.Load()
	if ts == nil {
		return
	}
	if fd < 0 || fd >= len(l.conns) {
		return
	}
	cs := l.conns[fd]
	if cs == nil {
		return
	}

	// RACE SAFETY (always-on, all protocols): in async mode the per-conn dispatch
	// goroutine mutates cs.protocol / cs.h1State / cs.h2State (switchToH2Local on
	// an H1→H2C upgrade) — but ONLY while it is RUNNING (asyncParked==false). So we
	// must confirm the goroutine is PARKED under asyncInMu BEFORE reading any of
	// those fields. The lock + the parked invariant establish happens-before with
	// the goroutine's last write, and no concurrent drainRead can wake it
	// (tryTransplant runs after drainRead on this same loop thread). For a conn
	// with no dispatch goroutine (sync, or async-never-promoted) the fields are
	// touched only by this loop thread, so the reads are inherently safe.
	hasGoroutine := false
	if l.async {
		cs.asyncInMu.Lock()
		running := cs.asyncRun
		parked := cs.asyncParked
		idle := len(cs.asyncInBuf) == 0
		cs.asyncInMu.Unlock()
		if running {
			if !parked || !idle || cs.asyncClosed.Load() || cs.asyncQuiesce.Load() {
				return // goroutine busy / not cleanly parked / tearing down — retry later
			}
			hasGoroutine = true
		}
	}

	// Eligibility — now race-free (see above). Only a plain HTTP/1 keep-alive at a
	// clean, fully-flushed boundary is movable. H2 / h2c / mid-H2C-upgrade /
	// detached (WS/SSE) conns are excluded by the protocol + h2State + Detached +
	// AtRequestBoundary(UpgradeInfo==nil) gates; driver fds never reach here (the
	// run loop dispatches them via handleDriverEvent + continue).
	if cs.protocol != engine.HTTP1 || !cs.detected || cs.h1State == nil {
		return
	}
	if cs.h1State.Detached.Load() {
		return
	}
	// Defense-in-depth: never move a conn that has begun an H2 transition. These
	// are already excluded by the HTTP1/h1State gates above (switchToH2 flips
	// protocol=H2C and nils h1State atomically with these writes), but make the
	// contract explicit against future upgrade-path changes.
	if cs.h2State != nil || cs.asyncH2Promoted.Load() {
		return
	}
	if !l.flushedAtBoundary(cs) {
		return
	}

	if hasGoroutine {
		// Promoted async conn, parked + flushed: detach the fd (stops re-feed /
		// re-promote), ask the goroutine to quiesce, and finish in
		// drainDetachQueue. A parked conn at a clean boundary has an empty buffer;
		// async carries no buffered bytes (destination re-injection is sync-only).
		if cs.h1State.HasPendingData() {
			return
		}
		l.detachFromEpoll(fd, cs)
		cs.transplantPending = true
		cs.asyncQuiesce.Store(true)
		cs.asyncInMu.Lock()
		cs.asyncCond.Broadcast() // wake the parked goroutine so it sees quiesce and exits
		cs.asyncInMu.Unlock()
		return
	}

	// Sync conn, or async conn whose dispatch goroutine never started: detach +
	// hand off now. The async path carries no buffered bytes (re-injection on the
	// destination's async path is out of scope), so require an empty buffer there.
	if l.async && cs.h1State.HasPendingData() {
		return
	}
	carry := engine.Carryover{RemoteAddr: cs.remoteAddr}
	if !l.async {
		// Carry buffered pipelined NEXT-request bytes (sync path). flushedAtBoundary
		// guarantees AtRequestBoundary, so these are next-request bytes, safe to
		// replay on the destination's fresh parser.
		if buffered := cs.h1State.TakeBufferedBytes(); len(buffered) > 0 {
			carry.Buffered = buffered
		}
	}
	l.detachForTransplant(fd, cs)
	if err := ts.target.AdoptConn(fd, carry); err != nil {
		// Target refused the fd (out of range / no workers). We already
		// relinquished it, so close it to avoid a descriptor leak.
		_ = unix.Close(fd)
	}
}

// flushedAtBoundary reports whether cs is at a clean HTTP/1 request boundary with
// its response fully flushed — no body/upgrade mid-receive and no pending
// writeBuf/bodyBuf/sendfile or EPOLLOUT backpressure.
func (l *Loop) flushedAtBoundary(cs *connState) bool {
	if !cs.h1State.AtRequestBoundary() {
		return false
	}
	return !cs.dirty && !cs.epollOut && cs.writePos == 0 && cs.pendingBytes == 0 &&
		len(cs.writeBuf) == 0 && len(cs.bodyBuf) == 0 && cs.sendfile == nil
}

// detachFromEpoll removes cs/fd from this loop's epoll set, live set, and conn
// table and adjusts counters, WITHOUT closing the fd or releasing the connState.
// Used by both the synchronous detach (detachForTransplant) and the deferred
// async path (where the dispatch goroutine still references cs until it exits).
func (l *Loop) detachFromEpoll(fd int, cs *connState) {
	_ = unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_DEL, fd, nil)
	l.removeLiveConn(cs)
	l.driverMu.Lock()
	l.conns[fd] = nil
	l.driverMu.Unlock()
	l.connCount--
	l.activeConns.Add(-1)
}

// detachForTransplant fully detaches cs/fd (epoll + connState release) WITHOUT
// closing the fd, so it can be adopted by another engine. Mirrors hijackConn's
// detach sequence minus the net.Conn wrap. Runs on the loop's own thread. After
// it returns the fd is owned by no engine; incoming bytes queue in the kernel
// socket receive buffer until the destination arms its first read.
func (l *Loop) detachForTransplant(fd int, cs *connState) {
	l.detachFromEpoll(fd, cs)
	// Return the epoll connState to the pool. This clears h1State/buffers but
	// does NOT close the fd (that is closeConn's job) — the fd lives on under
	// the io_uring engine.
	releaseConnState(cs)
}

// finishTransplantHandoff completes a deferred async transplant after the
// dispatch goroutine has exited and enqueued cs (#383). The fd was already
// removed from epoll by tryTransplant; here we capture the carry-over, release
// the connState, and hand the fd to the io_uring target. Runs on the loop thread
// (from drainDetachQueue).
func (l *Loop) finishTransplantHandoff(cs *connState) {
	cs.transplantPending = false
	fd := cs.fd
	carry := engine.Carryover{RemoteAddr: cs.remoteAddr}
	releaseConnState(cs)
	if ts := l.transplant.Load(); ts != nil {
		if err := ts.target.AdoptConn(fd, carry); err == nil {
			return
		}
	}
	// Transplant was stopped, or the target refused the fd — close it.
	_ = unix.Close(fd)
}
