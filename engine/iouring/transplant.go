//go:build linux

package iouring

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

// AdoptConn implements [engine.TransplantTarget] (#383). The source engine
// (epoll) has ALREADY detached fd from its own event loop before calling this,
// so fd is owned by no engine at this instant; any pending request bytes wait in
// the kernel socket receive buffer. AdoptConn routes the fd to one worker
// (round-robin) and schedules the attach on that worker's own thread via the
// existing driver-action wakeup primitive (eventfd), where SQE submission is
// single-issuer-safe.
//
// Safe to call from any goroutine. On a returned error the caller still owns fd.
func (e *Engine) AdoptConn(fd int, carry engine.Carryover) error {
	if fd < 0 || fd >= fixedFileTableSize {
		return fmt.Errorf("celeris/iouring: adopt fd %d out of range [0,%d)", fd, fixedFileTableSize)
	}
	// e.workers is published under e.mu by Listen; read it the same way the
	// other post-Listen accessors (NumWorkers/WorkerLoop) do.
	e.mu.Lock()
	n := len(e.workers)
	if n == 0 {
		e.mu.Unlock()
		return fmt.Errorf("celeris/iouring: no workers available to adopt fd %d", fd)
	}
	w := e.workers[int(e.metrics.transplantRR.Add(1)-1)%n]
	e.mu.Unlock()

	// addDriverAction takes the worker's own lock + wakes its ring; safe to call
	// outside e.mu.
	w.addDriverAction(driverAction{kind: driverActionAdopt, adoptFD: fd, adoptCarry: carry})
	return nil
}

// TransplantCount returns the cumulative number of connections this engine has
// adopted from another engine via AdoptConn (#383). Zero unless the transplant
// feature flag is active.
func (e *Engine) TransplantCount() uint64 { return e.metrics.transplantCount.Load() }

var _ engine.TransplantTarget = (*Engine)(nil)

// attachAdoptedFD runs on the worker thread (via drainDriverActions) and turns a
// real, already-connected fd into a full HTTP/1 connection on this worker. It
// mirrors the non-fixed-file path of onAcceptedFD. The fd is assumed to be at a
// clean HTTP/1 request boundary (the source engine only hands off conns with no
// pipelined leftover and a fully-flushed response), so no protocol state needs
// to transfer: a fresh H1State is installed and the next request — already in
// the kernel socket receive buffer — is served by the multishot recv armed here.
func (w *Worker) attachAdoptedFD(newFD int, carry engine.Carryover) {
	if newFD < 0 || newFD >= len(w.conns) {
		_ = unix.Close(newFD)
		w.errCount.Add(1)
		return
	}
	if w.conns[newFD] != nil {
		// Slot already occupied — impossible in practice (the source detached
		// the fd before handing it off, and io_uring nils w.conns[fd] at close,
		// so a reused fd finds an empty slot). Refuse rather than clobber the
		// slot holder; do NOT close the fd here, since the slot holder may close
		// the same descriptor number later and a double-close could hit an
		// unrelated reused fd. Counts as an error; never observed in testing.
		w.errCount.Add(1)
		return
	}

	// Re-apply socket options on the worker's own thread (the fd is a real,
	// non-fixed-file socket). Harmless if epoll already set them.
	_ = sockopts.ApplyFD(newFD, w.sockOpts)

	bufSize := w.resolved.BufferSize
	if w.bufRing != nil {
		bufSize = 0
	}
	ctx := w.runCtx
	if ctx == nil {
		ctx = context.Background()
	}
	cs := acquireConnState(ctxkit.WithWorkerID(ctx, w.id), newFD, bufSize, w.async)
	cs.fixedFile = false
	cs.remoteAddr = carry.RemoteAddr

	w.conns[newFD] = cs
	w.connCount++
	w.addLiveConn(cs)
	if newFD > w.maxFD {
		w.maxFD = newFD
	}
	cs.writeFn = w.makeWriteFn(cs)
	w.activeConns.Add(1)
	if w.transplantCount != nil {
		w.transplantCount.Add(1)
	}
	cs.lastActivity = w.cachedNow

	// #383 adopts HTTP/1 keep-alive conns only; lock the protocol and install a
	// fresh parser at the request boundary.
	cs.protocol.Store(int32(engine.HTTP1))
	cs.detected = true
	w.initProtocol(cs)

	// Replay any carried pipelined NEXT-request bytes through the fresh parser
	// before arming the steady recv — mirrors handleRecv's process→flush. The
	// source guaranteed AtRequestBoundary, so these are whole/partial next
	// requests, never a mid-request continuation.
	if len(carry.Buffered) > 0 {
		if perr := conn.ProcessH1(cs.ctx, carry.Buffered, cs.h1State, w.handler, cs.writeFn); perr != nil {
			if !errors.Is(perr, conn.ErrHijacked) {
				w.closeConn(newFD)
			}
			return // hijacked (handed off) or closed on error — nothing more to arm
		}
		if w.flushSend(cs) {
			w.markDirty(cs)
		}
	}

	if !w.prepareRecv(cs, cs.buf) {
		cs.needsRecv = true
		w.markDirty(cs)
	}
}
