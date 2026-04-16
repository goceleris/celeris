//go:build linux

package iouring

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"github.com/goceleris/celeris/engine"
	"golang.org/x/sys/unix"
)

// prepCancelFDDriver prepares an ASYNC_CANCEL SQE without CQE_SKIP_SUCCESS,
// so the CQE arrives and drives handleDriverClose. Matches prepCancelFDSkipSuccess
// except for the skip-success flag.
func prepCancelFDDriver(sqePtr unsafe.Pointer, fd int) {
	sqe := (*[sqeSize]byte)(sqePtr)
	sqe[0] = opASYNCCANCEL
	sqe[1] = 0
	*(*int32)(unsafe.Pointer(&sqe[4])) = int32(fd)
	*(*uint32)(unsafe.Pointer(&sqe[28])) = cancelFD | cancelAll
}

// driverRecvBufSize is the single-shot recv buffer size for driver FDs.
// Drivers don't share the HTTP provided buffer ring — each driverConn owns
// its own buffer. 4 KiB is enough for typical request/response framing
// (postgres, redis); drivers that want larger payloads read multiple chunks.
const driverRecvBufSize = 4 * 1024

// driverSendCap is the per-FD outbound backpressure limit. Writes past this
// return engine.ErrQueueFull so the driver can back off its producer.
const driverSendCap = 64 << 20

// driverConn holds per-FD state for an EventLoopProvider-registered FD.
// Separate from connState because the HTTP path has invariants (protocol
// detection, fixed files, detached WS paths) that don't apply to drivers.
type driverConn struct {
	fd       int
	w        *Worker
	onRecv   func([]byte)
	onClose  func(error)
	buf      []byte
	writeBuf []byte
	sendBuf  []byte
	sending  bool
	mu       sync.Mutex
	closing  bool
	recvArmed bool

	// inflightOps counts submitted RECV/SEND SQEs that have not yet
	// delivered a CQE. The kernel may still be reading into dc.buf or
	// writing from dc.sendBuf while an op is in flight, so finalizeDriver
	// must wait for the counter to hit zero before releasing dc to GC.
	inflightOps  int
	closePending bool // cancel-CQE arrived; waiting for in-flight ops to settle
	closeErr     error
}

// driverAction is one pending driver-side action to be applied by the worker
// on its own thread. Driver API calls (RegisterConn, UnregisterConn, Write)
// append here and wake the worker via eventfd; the worker processes them at
// the bottom of its event loop where SQE submission is safe.
type driverActionKind uint8

const (
	driverActionRegister driverActionKind = iota + 1
	driverActionUnregister
	driverActionWrite
)

type driverAction struct {
	kind driverActionKind
	dc   *driverConn
}

// addDriverAction enqueues work for the worker and wakes the ring.
func (w *Worker) addDriverAction(a driverAction) {
	w.driverActionMu.Lock()
	w.driverActionQueue = append(w.driverActionQueue, a)
	w.driverActionPending.Store(1)
	w.driverActionMu.Unlock()
	if w.h2EventFD >= 0 {
		var val [8]byte
		val[0] = 1
		_, _ = unix.Write(w.h2EventFD, val[:])
	}
}

// RegisterConn adds fd to this worker's driver map and schedules a single-shot
// RECV SQE on it. The caller must ensure fd is connected and non-blocking.
func (w *Worker) RegisterConn(fd int, onRecv func([]byte), onClose func(error)) error {
	if fd < 0 {
		return errors.New("celeris/iouring: invalid fd")
	}
	// TOCTOU-safe: first check at API time (racy but cheap), then re-check
	// under driverMu below. The worker goroutine re-checks again during
	// action apply in armDriverRecv, closing the window between this API
	// call and the worker's map insertion.
	if fd < len(w.conns) && w.conns[fd] != nil {
		return fmt.Errorf("celeris/iouring: fd %d is already an HTTP connection", fd)
	}
	w.driverMu.Lock()
	// Re-check under the lock: the worker goroutine may have accepted an
	// HTTP conn on this fd between the first check and the lock acquisition.
	if fd < len(w.conns) && w.conns[fd] != nil {
		w.driverMu.Unlock()
		return fmt.Errorf("celeris/iouring: fd %d is already an HTTP connection", fd)
	}
	if w.driverConns == nil {
		w.driverConns = make(map[int]*driverConn)
	}
	if _, exists := w.driverConns[fd]; exists {
		w.driverMu.Unlock()
		return errors.New("celeris/iouring: fd already registered")
	}
	dc := &driverConn{
		fd:      fd,
		w:       w,
		onRecv:  onRecv,
		onClose: onClose,
		buf:     make([]byte, driverRecvBufSize),
	}
	w.driverConns[fd] = dc
	w.hasDriverConns.Store(true)
	w.driverMu.Unlock()

	w.addDriverAction(driverAction{kind: driverActionRegister, dc: dc})
	return nil
}

// UnregisterConn cancels any in-flight RECV/SEND on fd and triggers onClose
// once pending operations settle. The caller is responsible for closing the
// underlying fd.
func (w *Worker) UnregisterConn(fd int) error {
	w.driverMu.RLock()
	dc, ok := w.driverConns[fd]
	w.driverMu.RUnlock()
	if !ok {
		return engine.ErrUnknownFD
	}
	dc.mu.Lock()
	if dc.closing {
		dc.mu.Unlock()
		return nil
	}
	dc.closing = true
	dc.mu.Unlock()
	w.addDriverAction(driverAction{kind: driverActionUnregister, dc: dc})
	return nil
}

// Write enqueues data for fd. Returns ErrQueueFull if the per-FD outbound
// buffer would exceed driverSendCap, and ErrUnknownFD if fd isn't registered.
func (w *Worker) Write(fd int, data []byte) error {
	w.driverMu.RLock()
	dc, ok := w.driverConns[fd]
	w.driverMu.RUnlock()
	if !ok {
		return engine.ErrUnknownFD
	}
	dc.mu.Lock()
	if dc.closing {
		dc.mu.Unlock()
		return nil
	}
	if len(dc.writeBuf)+len(dc.sendBuf)+len(data) > driverSendCap {
		dc.mu.Unlock()
		return engine.ErrQueueFull
	}
	dc.writeBuf = append(dc.writeBuf, data...)
	needSubmit := !dc.sending
	dc.mu.Unlock()
	if needSubmit {
		w.addDriverAction(driverAction{kind: driverActionWrite, dc: dc})
	}
	return nil
}

// CPUID returns the CPU this worker is pinned to, or -1 if unpinned.
func (w *Worker) CPUID() int {
	return w.cpuID
}

var _ engine.WorkerLoop = (*Worker)(nil)

// drainDriverActions applies enqueued driver-side actions on the worker thread.
// Called from the event loop after CQE processing so SQE submission is safe
// (single-issuer invariant).
func (w *Worker) drainDriverActions() {
	if w.driverActionPending.Load() == 0 {
		return
	}
	w.driverActionMu.Lock()
	w.driverActionSpare, w.driverActionQueue = w.driverActionQueue, w.driverActionSpare[:0]
	w.driverActionPending.Store(0)
	w.driverActionMu.Unlock()

	// Arm the eventfd poll so subsequent wakeups from driver goroutines
	// reach the ring event-driven rather than waiting for the adaptive
	// timeout (up to 100ms).
	if !w.h2PollArmed && w.h2EventFD >= 0 {
		w.prepareH2Poll()
		w.h2PollArmed = true
	}

	for _, a := range w.driverActionSpare {
		switch a.kind {
		case driverActionRegister:
			w.armDriverRecv(a.dc)
		case driverActionUnregister:
			w.cancelDriverConn(a.dc)
		case driverActionWrite:
			w.flushDriverSend(a.dc)
		}
	}
	w.driverActionSpare = w.driverActionSpare[:0]
}

// armDriverRecv submits a single-shot RECV SQE for dc. If the SQ ring is
// full, the action is re-queued so the next loop iteration retries.
func (w *Worker) armDriverRecv(dc *driverConn) {
	// Worker-side TOCTOU close: if an HTTP accept landed on this fd after
	// RegisterConn's checks, abort before arming RECV so CQEs don't cross
	// channels.
	w.driverMu.Lock()
	if dc.fd < len(w.conns) && w.conns[dc.fd] != nil {
		delete(w.driverConns, dc.fd)
		if len(w.driverConns) == 0 {
			w.hasDriverConns.Store(false)
		}
		w.driverMu.Unlock()
		cb := dc.onClose
		dc.onClose = nil
		if cb != nil {
			cb(fmt.Errorf("celeris/iouring: fd %d is already an HTTP connection", dc.fd))
		}
		return
	}
	w.driverMu.Unlock()

	dc.mu.Lock()
	if dc.closing || dc.recvArmed {
		dc.mu.Unlock()
		return
	}
	sqe := w.ring.GetSQE()
	if sqe == nil {
		dc.mu.Unlock()
		// Re-queue for retry on the next event loop iteration.
		w.addDriverAction(driverAction{kind: driverActionRegister, dc: dc})
		return
	}
	prepRecv(sqe, dc.fd, dc.buf)
	setSQEUserData(sqe, encodeUserData(udDriverRecv, dc.fd))
	dc.recvArmed = true
	dc.inflightOps++
	dc.mu.Unlock()
}

// flushDriverSend submits one SEND SQE for dc, swapping writeBuf into sendBuf.
// Mirrors the HTTP flushSend invariant: at most one SEND in flight per FD.
func (w *Worker) flushDriverSend(dc *driverConn) {
	dc.mu.Lock()
	if dc.closing || dc.sending {
		dc.mu.Unlock()
		return
	}
	if len(dc.sendBuf) == 0 {
		if len(dc.writeBuf) == 0 {
			dc.mu.Unlock()
			return
		}
		dc.sendBuf, dc.writeBuf = dc.writeBuf, dc.sendBuf[:0]
	}
	sqe := w.ring.GetSQE()
	if sqe == nil {
		// SQ ring full — swap back so caller can retry.
		if len(dc.writeBuf) == 0 {
			dc.writeBuf, dc.sendBuf = dc.sendBuf, dc.writeBuf[:0]
		}
		dc.mu.Unlock()
		w.addDriverAction(driverAction{kind: driverActionWrite, dc: dc})
		return
	}
	prepSendPlain(sqe, dc.fd, dc.sendBuf, false)
	setSQEUserData(sqe, encodeUserData(udDriverSend, dc.fd))
	dc.sending = true
	dc.inflightOps++
	w.sendsPending = true
	dc.mu.Unlock()
}

// cancelDriverConn submits an ASYNC_CANCEL targeting all in-flight ops on the
// driver FD. The cancel CQE arrives as a driver CQE (via the udDriverClose
// user-data on the cancel SQE itself), at which point we finalize teardown.
func (w *Worker) cancelDriverConn(dc *driverConn) {
	sqe := w.ring.GetSQE()
	if sqe == nil {
		// Retry on the next event loop iteration.
		w.addDriverAction(driverAction{kind: driverActionUnregister, dc: dc})
		return
	}
	prepCancelFDDriver(sqe, dc.fd)
	setSQEUserData(sqe, encodeUserData(udDriverClose, dc.fd))
	// If no in-flight ops, the ASYNC_CANCEL completes with -ENOENT and we
	// still route through handleDriverClose to fire onClose and clean up.
}

// handleDriverRecv processes a RECV CQE for a driver FD. On data, dispatches
// to onRecv and re-arms. On EOF / error, fires onClose and removes the FD.
func (w *Worker) handleDriverRecv(c *completionEntry, fd int) {
	w.driverMu.RLock()
	dc := w.driverConns[fd]
	w.driverMu.RUnlock()
	if dc == nil {
		return
	}
	dc.mu.Lock()
	dc.recvArmed = false
	dc.inflightOps--
	closing := dc.closing
	pending := dc.closePending && dc.inflightOps == 0
	closeErr := dc.closeErr
	dc.mu.Unlock()
	if pending {
		w.finalizeDriver(dc, closeErr)
		return
	}
	if closing {
		return
	}

	if c.Res < 0 {
		if c.Res == -int32(unix.ECANCELED) {
			return // cancel; handleDriverClose will finalize when inflight hits 0
		}
		w.finalizeDriver(dc, errIORingRecv(c.Res))
		return
	}
	if c.Res == 0 {
		w.finalizeDriver(dc, nil)
		return
	}
	// onRecv receives a slice valid only for this call (contract).
	if dc.onRecv != nil {
		dc.onRecv(dc.buf[:c.Res])
	}
	// Re-arm recv unless the callback closed us.
	dc.mu.Lock()
	if dc.closing {
		dc.mu.Unlock()
		return
	}
	dc.mu.Unlock()
	w.armDriverRecv(dc)
}

// handleDriverSend processes a SEND CQE for a driver FD, handling partial
// sends and re-submitting if writeBuf has new data.
func (w *Worker) handleDriverSend(c *completionEntry, fd int) {
	w.driverMu.RLock()
	dc := w.driverConns[fd]
	w.driverMu.RUnlock()
	if dc == nil {
		return
	}
	dc.mu.Lock()
	dc.sending = false
	dc.inflightOps--
	pending := dc.closePending && dc.inflightOps == 0
	closeErr := dc.closeErr
	if c.Res < 0 {
		dc.mu.Unlock()
		if pending {
			w.finalizeDriver(dc, closeErr)
			return
		}
		if c.Res == -int32(unix.ECANCELED) {
			return
		}
		w.finalizeDriver(dc, errIORingSend(c.Res))
		return
	}
	sent := int(c.Res)
	if sent < len(dc.sendBuf) {
		// Partial send — shift remainder.
		remaining := len(dc.sendBuf) - sent
		copy(dc.sendBuf, dc.sendBuf[sent:])
		dc.sendBuf = dc.sendBuf[:remaining]
	} else {
		dc.sendBuf = dc.sendBuf[:0]
	}
	hasMore := len(dc.sendBuf) > 0 || len(dc.writeBuf) > 0
	closing := dc.closing
	dc.mu.Unlock()
	if pending {
		w.finalizeDriver(dc, closeErr)
		return
	}
	if closing {
		return
	}
	if hasMore {
		w.flushDriverSend(dc)
	}
}

// handleDriverClose processes the ASYNC_CANCEL CQE for a driver FD. If any
// RECV/SEND ops are still in flight (the kernel is still holding dc.buf or
// dc.sendBuf), defer finalize until their -ECANCELED CQEs arrive. Otherwise
// finalize immediately.
func (w *Worker) handleDriverClose(fd int) {
	w.driverMu.RLock()
	dc := w.driverConns[fd]
	w.driverMu.RUnlock()
	if dc == nil {
		return
	}
	dc.mu.Lock()
	if dc.inflightOps > 0 {
		dc.closePending = true
		// closeErr stays nil — user-initiated cancel completes cleanly.
		dc.mu.Unlock()
		return
	}
	dc.mu.Unlock()
	w.finalizeDriver(dc, nil)
}

// errEngineShutdown is passed to driver onClose callbacks when the Worker
// is shutting down and can no longer service registered FDs.
var errEngineShutdown = errors.New("celeris/iouring: engine shutdown")

// shutdownDrivers fires onClose(errEngineShutdown) for every registered
// driver conn and clears the map. Called from Worker.shutdown on the worker
// goroutine. The caller owns the FDs; we do not close them.
func (w *Worker) shutdownDrivers() {
	w.driverMu.Lock()
	if len(w.driverConns) == 0 {
		w.driverMu.Unlock()
		return
	}
	conns := make([]*driverConn, 0, len(w.driverConns))
	for _, dc := range w.driverConns {
		conns = append(conns, dc)
	}
	w.driverConns = nil
	w.hasDriverConns.Store(false)
	w.driverMu.Unlock()

	for _, dc := range conns {
		dc.mu.Lock()
		if dc.closing {
			dc.mu.Unlock()
			// closing was set by UnregisterConn; the cancel-CQE path will
			// fire onClose. But the ring is being torn down, so fire it
			// here as a fallback — finalizeDriver's map check guards
			// against double-fire.
		} else {
			dc.closing = true
			dc.mu.Unlock()
		}
		cb := dc.onClose
		dc.onClose = nil
		if cb != nil {
			cb(errEngineShutdown)
		}
	}
}

// finalizeDriver removes dc from the driver map, flips the gate if the map
// is now empty, and fires the onClose callback exactly once.
func (w *Worker) finalizeDriver(dc *driverConn, err error) {
	w.driverMu.Lock()
	if existing, ok := w.driverConns[dc.fd]; !ok || existing != dc {
		w.driverMu.Unlock()
		return
	}
	delete(w.driverConns, dc.fd)
	if len(w.driverConns) == 0 {
		w.hasDriverConns.Store(false)
	}
	w.driverMu.Unlock()

	cb := dc.onClose
	dc.onClose = nil
	if cb != nil {
		cb(err)
	}
}
