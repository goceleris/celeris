//go:build linux

package epoll

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/ctxkit"
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
	if l.conns[fd] != nil {
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
	l.conns[fd] = cs
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

	// Replay any carried pipelined NEXT-request bytes through the fresh parser
	// (sync path only — the source guaranteed a clean boundary, so for async it
	// carries nothing). Mirrors drainRead's inline process→flush.
	if len(carry.Buffered) > 0 && !l.async {
		if perr := conn.ProcessH1(cs.ctx, carry.Buffered, cs.h1State, l.handler, cs.writeFn); perr != nil {
			if !errors.Is(perr, conn.ErrHijacked) {
				l.closeConn(fd)
			}
			return
		}
		if csWritePending(cs) {
			if fErr := l.flushWrites(cs, true); fErr != nil {
				l.closeConn(fd)
				return
			}
		}
	}
}
