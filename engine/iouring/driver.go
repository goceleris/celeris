//go:build linux

package iouring

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
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
	fd        int
	w         *Worker
	onRecv    func([]byte)
	onClose   func(error)
	buf       []byte
	writeBuf  []byte
	sendBuf   []byte
	sending   bool
	mu        sync.Mutex
	closing   bool
	recvArmed bool

	// inflightOps counts the SQEs of this conn that the worker has prepared
	// and whose CQE it has not yet processed: its RECV and SEND, and every
	// ASYNC_CANCEL, UnregisterConn's and failDriverConn's alike. The kernel
	// may still be reading into dc.buf or writing from dc.sendBuf while a
	// RECV or SEND is in flight, and it resolves a cancel's descriptor
	// number only when the worker submits it. So the conn is finalized, and
	// retire closes opFD and releases dc to GC, only once the counter is
	// zero (celeris#707).
	inflightOps int
	// cancels is the part of inflightOps that is cancels. A close CQE is
	// this conn's own only while it is positive: user_data carries fd and
	// no generation, so handleDriverClose ignores one that finds it zero.
	cancels      int
	closePending bool // set by failDriverConn or a cancel's CQE: finalize once inflightOps is zero
	closeErr     error

	// opFD is the engine's own descriptor for the socket: a duplicate of fd
	// that RegisterConn takes and retire closes (celeris#691). Every SQE of
	// this conn names opFD, never fd. The kernel resolves an SQE's
	// descriptor number when the worker submits it, at the top of its next
	// loop iteration, and an ASYNC_CANCEL's when the worker issues it, after
	// UnregisterConn has queued it. By then a caller that closed fd right
	// after UnregisterConn, as every in-tree driver does, has left the
	// number naming nothing (-EBADF) or, once reused, another socket: a
	// cancel by fd missed the RECV armed on this one (onClose never fired,
	// the socket never closed), and a RECV or SEND prepared by fd went to
	// the other socket. opFD names this socket for as long as the engine
	// holds it. fd stays the key: of driverConns, and in the user_data.
	// Set before dc is published and never changed; closed once, by retire.
	opFD int
	// opFDOpen: opFD is open and the engine's to close. Set with opFD by
	// RegisterConn, cleared by retire under mu. A zero opFD is descriptor
	// 0, so a driverConn built any other way (a test's) closes nothing.
	opFDOpen bool
	// retired: dc has left the worker (finalized, refused by armDriverRecv,
	// or dropped by shutdownDrivers) and opFD is closed. Set with closing,
	// under mu. No SQE is prepared by opFD after it.
	retired bool
}

// retire marks dc as gone from the worker and closes its descriptor, once.
// Every path that removes dc from driverConns calls it, on the worker
// goroutine: finalizeDriver (no SQE in flight, cancels included),
// armDriverRecv's refusal (within the contract only at the register, before
// anything is issued) and shutdownDrivers (after which nothing is submitted;
// closing the ring cancels what is armed). Setting closing too makes a later
// UnregisterConn or Write a no-op, so neither queues work for a conn that is
// gone, and every path that prepares an SQE checks closing first.
func (dc *driverConn) retire() {
	dc.mu.Lock()
	dc.closing = true
	dc.retired = true
	if dc.opFDOpen {
		dc.opFDOpen = false
		_ = unix.Close(dc.opFD)
	}
	dc.mu.Unlock()
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
	// driverActionAdopt (#383) adopts a real fd handed off from another engine
	// (epoll) as a full HTTP connection on this worker. Reuses the driver-action
	// queue purely as the existing cross-thread wakeup primitive; the adopted fd
	// becomes a normal entry in w.conns (not a driverConn).
	driverActionAdopt
)

type driverAction struct {
	kind driverActionKind
	dc   *driverConn
	// adopt-only (#383): the real fd to adopt + its carried-over state.
	adoptFD    int
	adoptCarry engine.Carryover
}

// addDriverAction enqueues work for the worker and wakes it: the wakeup
// handle (celeris#655: a handle, so a driver that registers an fd on a
// worker that has already shut down cannot write a recycled descriptor)
// reaches a worker waiting on its ring, wakeIfSuspended one parked in
// DRAINING→SUSPENDED (celeris#658).
func (w *Worker) addDriverAction(a driverAction) {
	w.driverActionMu.Lock()
	w.driverActionQueue = append(w.driverActionQueue, a)
	w.driverActionPending.Store(1)
	w.driverActionMu.Unlock()
	w.wakeFD.Signal()
	// After releasing driverActionMu: wakeMu is a leaf lock.
	w.wakeIfSuspended()
}

// addAdoptAction queues a transplanted descriptor for this worker (#383). It is
// addDriverAction with one difference: it can refuse. Once the worker has shut
// down nothing will ever drain the queue, and an adoption accepted there is a
// descriptor the source has let go of and no engine owns (celeris#658). The
// error leaves fd with the caller, whose reclaim path takes the connection
// back.
//
// The wakeup is signalled under driverActionMu: Worker.shutdown takes this
// lock (closeAdoptQueue) before it closes the eventfd. Since celeris#655 the
// handle enforces that on its own — Signal and Close share a lock — and the
// ordering is kept here because it also publishes the queue entry.
func (w *Worker) addAdoptAction(fd int, carry engine.Carryover) error {
	w.driverActionMu.Lock()
	if w.adoptClosed {
		w.driverActionMu.Unlock()
		return fmt.Errorf("celeris/iouring: worker %d has shut down, cannot adopt fd %d", w.id, fd)
	}
	w.driverActionQueue = append(w.driverActionQueue,
		driverAction{kind: driverActionAdopt, adoptFD: fd, adoptCarry: carry})
	w.driverActionPending.Store(1)
	w.wakeFD.Signal()
	w.driverActionMu.Unlock()
	// A standby worker parked in DRAINING→SUSPENDED does not read the
	// eventfd; kick it (celeris#658). After releasing driverActionMu:
	// wakeMu is a leaf lock.
	w.wakeIfSuspended()
	return nil
}

// RegisterConn adds fd to this worker's driver map and schedules a single-shot
// RECV SQE on it. The caller must ensure fd is connected and non-blocking.
// The engine holds its own duplicate of fd until the conn is finalized, and
// every operation on the socket goes through it (celeris#691), so a caller
// that closes fd without calling UnregisterConn leaves the socket open until
// its peer closes it. Once the worker has shut down RegisterConn returns an
// error wrapping errEngineShutdown; on any error fd stays the caller's to
// close.
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
	// The engine's own descriptor for the socket, taken while fd is surely
	// the caller's (celeris#691). Lowest number 3: it never lands on stdin,
	// stdout or stderr in a process that closed them, where log output
	// would reach the driver's socket. Outside driverMu: it is a syscall.
	opFD, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 3)
	if err != nil {
		return fmt.Errorf("celeris/iouring: duplicate fd %d: %w", fd, err)
	}
	w.driverMu.Lock()
	var refused error
	switch {
	case w.driversClosed:
		// A worker that has shut down never retires a conn again, so nothing
		// would close opFD (celeris#691). Engine.WorkerLoop still hands such
		// a worker out: after the engine stops, or after this worker alone
		// exited. Refuse, as AdoptConn does; fd stays the caller's.
		refused = fmt.Errorf("celeris/iouring: worker %d has shut down, cannot register fd %d: %w",
			w.id, fd, errEngineShutdown)
	case fd < len(w.conns) && w.conns[fd] != nil:
		// Re-check under the lock: the worker goroutine may have accepted an
		// HTTP conn on this fd between the first check and the lock
		// acquisition.
		refused = fmt.Errorf("celeris/iouring: fd %d is already an HTTP connection", fd)
	case w.driverConns[fd] != nil:
		refused = errors.New("celeris/iouring: fd already registered")
	}
	if refused != nil {
		w.driverMu.Unlock()
		_ = unix.Close(opFD)
		return refused
	}
	if w.driverConns == nil {
		w.driverConns = make(map[int]*driverConn)
	}
	dc := &driverConn{
		fd:       fd,
		opFD:     opFD,
		opFDOpen: true,
		w:        w,
		onRecv:   onRecv,
		onClose:  onClose,
		buf:      make([]byte, driverRecvBufSize),
	}
	w.driverConns[fd] = dc
	w.hasDriverConns.Store(true)
	w.driverMu.Unlock()

	w.addDriverAction(driverAction{kind: driverActionRegister, dc: dc})
	return nil
}

// UnregisterConn cancels any in-flight RECV/SEND on fd and triggers onClose
// once pending operations settle. The caller is responsible for closing the
// underlying fd, and may close it as soon as UnregisterConn returns, without
// waiting for onClose: the worker cancels, later, through the engine's own
// duplicate of fd, and closes that duplicate when it finalizes the conn,
// which is when the socket closes (celeris#691). See [engine.WorkerLoop].
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
	// Assign the result — do NOT mark it armed unconditionally. prepareH2Poll
	// reports whether the arm was actually placed, and its docstring spells
	// out why that matters: the poll is single-shot and w.h2PollArmed is
	// never cleared anywhere else, so swallowing a full SQ ring leaves the
	// eventfd deaf for the life of the worker. The loop retries through
	// rearmH2PollIfPending. Every other call site already does this; this one
	// was missed by celeris#523 (celeris#537).
	if !w.h2PollArmed && w.wakeFD.FD() >= 0 {
		w.h2PollArmed = w.prepareH2Poll()
	}

	for _, a := range w.driverActionSpare {
		switch a.kind {
		case driverActionRegister:
			w.armDriverRecv(a.dc)
		case driverActionUnregister:
			w.cancelDriverConn(a.dc)
		case driverActionWrite:
			w.flushDriverSend(a.dc)
		case driverActionAdopt:
			w.attachAdoptedFD(a.adoptFD, a.adoptCarry)
		}
	}
	// Same guard as the detach queue: a driverAction holds pointers, so
	// stale slots keep them reachable after the action has been applied.
	clear(w.driverActionSpare)
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
		// Closes the engine's descriptor, which nothing else would, and
		// makes an UnregisterConn queued behind this register issue nothing.
		dc.retire()
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
	prepRecv(sqe, dc.opFD, dc.buf) // never dc.fd: celeris#691
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
	prepSendPlain(sqe, dc.opFD, dc.sendBuf, false) // never dc.fd: celeris#691
	setSQEUserData(sqe, encodeUserData(udDriverSend, dc.fd))
	dc.sending = true
	dc.inflightOps++
	w.sendsPending = true
	dc.mu.Unlock()
}

// cancelDriverConn submits an ASYNC_CANCEL targeting all in-flight ops on the
// driver's socket, by the engine's own descriptor (celeris#691): the caller
// may have closed fd since. The cancel CQE arrives as a driver CQE (via the
// udDriverClose user-data on the cancel SQE itself), at which point we
// finalize teardown.
func (w *Worker) cancelDriverConn(dc *driverConn) {
	dc.mu.Lock()
	if dc.retired {
		// Finalized, refused or dropped while this action was queued:
		// opFD is closed, and its number may name another file by now,
		// even a duplicate of a socket with ops armed on this ring (any
		// dup in the process takes the lowest free number). Issue nothing.
		dc.mu.Unlock()
		return
	}
	sqe := w.ring.GetSQE()
	if sqe == nil {
		dc.mu.Unlock()
		// Retry on the next event loop iteration.
		w.addDriverAction(driverAction{kind: driverActionUnregister, dc: dc})
		return
	}
	prepCancelFDDriver(sqe, dc.opFD)
	setSQEUserData(sqe, encodeUserData(udDriverClose, dc.fd))
	// Counted until its CQE, like a RECV or SEND (celeris#707). If no op is
	// in flight, the ASYNC_CANCEL completes with -ENOENT and still routes
	// through handleDriverClose to fire onClose and clean up.
	dc.inflightOps++
	dc.cancels++
	dc.mu.Unlock()
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
		w.failDriverConn(dc, errIORingRecv(c.Res))
		return
	}
	if c.Res == 0 {
		w.failDriverConn(dc, nil)
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
		w.failDriverConn(dc, errIORingSend(c.Res))
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
// other SQE of the conn is still in flight (a RECV or SEND the kernel may
// still be holding dc.buf or dc.sendBuf for, or another cancel), defer
// finalize until its CQE arrives. Otherwise finalize immediately.
func (w *Worker) handleDriverClose(fd int) {
	w.driverMu.RLock()
	dc := w.driverConns[fd]
	w.driverMu.RUnlock()
	if dc == nil {
		return
	}
	dc.mu.Lock()
	if dc.cancels == 0 {
		// Not a cancel of dc's. Every cancel is counted when it is
		// prepared, and a conn is finalized only after all of its CQEs, so
		// this one belongs to a conn that left the map without waiting for
		// it, and dc holds its number now. Acting on it would close dc
		// (celeris#707).
		dc.mu.Unlock()
		return
	}
	dc.cancels--
	dc.inflightOps--
	dc.closePending = true
	if dc.inflightOps > 0 {
		// closeErr stays as whoever set it first: nil for a user-initiated
		// cancel (UnregisterConn), the I/O error for failDriverConn.
		dc.mu.Unlock()
		return
	}
	closeErr := dc.closeErr
	dc.mu.Unlock()
	w.finalizeDriver(dc, closeErr)
}

// failDriverConn finalizes dc with err — but only once no kernel ops
// remain in flight. The error branches of handleDriverRecv /
// handleDriverSend used to finalize unconditionally, which released the
// worker's reference to dc while the OTHER op could still be kernel-held
// (a send error with the recv SQE still armed on dc.buf, or vice versa):
// the same kernel-writes-freed-memory class as the HTTP close path
// (#256 class, v1.4.15/7beebb9 variant), bypassing the inflightOps gating that handleDriverClose
// applies. When ops remain, record the first error, mark the close
// pending, and submit an ASYNC_CANCEL so their terminal CQEs arrive
// promptly; the closePending checks in the CQE handlers finalize once the
// counter drains. Runs on the worker goroutine (CQE dispatch), so SQE
// submission is safe.
func (w *Worker) failDriverConn(dc *driverConn, err error) {
	dc.mu.Lock()
	if dc.inflightOps > 0 {
		dc.closing = true
		dc.closePending = true
		if dc.closeErr == nil {
			dc.closeErr = err
		}
		dc.mu.Unlock()
		// By opFD: this cancel is submitted later too, and a caller that
		// unregisters and closes fd before then would make a cancel by fd
		// miss (celeris#691). Counted in flight until its CQE: it is
		// prepared during CQE processing, and the conn's other op can
		// complete later in the same batch. Uncounted, that op's CQE
		// finalized the conn and retire closed opFD with this cancel still
		// unsubmitted, so the kernel resolved a number the engine had
		// closed, and the cancel's own CQE reached whatever conn was
		// registered on fd next (celeris#707). getCancelSQE can submit, so
		// it runs outside dc.mu.
		if sqe := w.getCancelSQE(); sqe != nil {
			prepCancelFDDriver(sqe, dc.opFD)
			setSQEUserData(sqe, encodeUserData(udDriverClose, dc.fd))
			dc.mu.Lock()
			dc.inflightOps++
			dc.cancels++
			dc.mu.Unlock()
		}
		return
	}
	dc.mu.Unlock()
	w.finalizeDriver(dc, err)
}

// errEngineShutdown is passed to driver onClose callbacks when the Worker
// is shutting down and can no longer service registered FDs.
var errEngineShutdown = errors.New("celeris/iouring: engine shutdown")

// shutdownDrivers fires onClose(errEngineShutdown) for every registered
// driver conn, clears the map, and makes every later RegisterConn fail.
// Called from Worker.shutdown on the worker goroutine. The caller owns the
// FDs; we do not close them.
func (w *Worker) shutdownDrivers() {
	w.driverMu.Lock()
	// Under the lock RegisterConn inserts under: a conn is either in the map
	// taken below, and retired here, or refused (celeris#691).
	w.driversClosed = true
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
	// Retain the strong references (celeris#545). Dropping the map here is
	// the only thing keeping these driverConns — and their dc.buf backing
	// arrays — alive, and the kernel may still hold a RECV writing into one.
	// Unlike UnregisterConn, this path issues no ASYNC_CANCEL and does not
	// wait for inflightOps to reach zero, because the loop is ending and
	// those CQEs would never be processed. What cancels the ops is closing
	// the ring, which happens later in shutdown(); holding the slice on the
	// Worker keeps the buffers reachable across that window.
	w.shutdownDriverHold = append(w.shutdownDriverHold, conns...)
	w.driverMu.Unlock()

	for _, dc := range conns {
		// If UnregisterConn got here first, the cancel-CQE path would have
		// fired onClose, but the ring is being torn down, so it fires here
		// instead; finalizeDriver's map check guards against double-fire.
		// retire also closes the engine's descriptor (celeris#691). Nothing
		// is submitted after this point, so an SQE still carrying its number
		// never reaches the kernel; closing the ring later in shutdown()
		// cancels the ops armed on the socket.
		dc.retire()
		cb := dc.onClose
		dc.onClose = nil
		if cb != nil {
			cb(errEngineShutdown)
		}
	}
}

// finalizeDriver removes dc from the driver map, flips the gate if the map
// is now empty, closes the engine's descriptor, and fires the onClose
// callback exactly once. The descriptor goes first: a caller that closed fd
// after UnregisterConn has let go of the socket, and with the engine's
// descriptor closed and no op in flight, the peer sees it close before
// onClose runs.
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

	dc.retire()
	cb := dc.onClose
	dc.onClose = nil
	if cb != nil {
		cb(err)
	}
}
