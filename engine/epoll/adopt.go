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
	l.adoptQueue = append(l.adoptQueue, adoptItem{fd: fd, carry: carry})
	l.adoptQPending.Store(1)
	l.adoptQMu.Unlock()
	// Wake the loop so the adopt is applied promptly (the loop drains the
	// eventfd counter and then drainAdoptQueue).
	if l.eventFD >= 0 {
		var val [8]byte
		val[0] = 1
		_, _ = unix.Write(l.eventFD, val[:])
	}
	return nil
}

var _ engine.TransplantTarget = (*Engine)(nil)

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
	l.adoptQSpare = l.adoptQSpare[:0]
}

// attachAdoptedFD registers a transplanted real fd onto this loop as a fresh
// HTTP/1 connection. Mirrors acceptAll's per-conn setup. Runs on the loop thread
// (epoll_ctl + connState setup are loop-owned). The fd is assumed to be at a
// clean HTTP/1 request boundary; any pending request bytes wait in the kernel
// socket receive buffer and are picked up by the EPOLLIN edge after registration.
func (l *Loop) attachAdoptedFD(ctx context.Context, fd int, carry engine.Carryover, now int64) {
	if fd < 0 {
		return
	}
	if fd >= connTableSize {
		_ = unix.Close(fd)
		l.errCount.Add(1)
		return
	}
	if fd >= len(l.conns) {
		l.growConns(fd)
	}
	if l.conns[fd] != nil {
		// Slot occupied — the source detached fd before handing it off, so this
		// should not happen; refuse rather than clobber a live conn. Do not close
		// (the slot holder may close the same descriptor later).
		l.errCount.Add(1)
		return
	}

	_ = sockopts.ApplyFD(fd, l.sockOpts)
	if err := unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_ADD, fd, &unix.EpollEvent{
		Events: unix.EPOLLIN | unix.EPOLLET | unix.EPOLLRDHUP,
		Fd:     int32(fd),
	}); err != nil {
		_ = unix.Close(fd)
		l.errCount.Add(1)
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
