//go:build linux

package eventloop

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
)

// ---------------------------------------------------------------------------
// io_uring constants (mirrored from engine/iouring to avoid import)
// ---------------------------------------------------------------------------

const (
	sysIOUringSetup = 425
	sysIOUringEnter = 426
)

const (
	iouSetupCoopTaskrun  = 1 << 8
	iouSetupSingleIssuer = 1 << 12
)

const (
	iouEnterGetEvents = 1 << 0
	iouEnterExtArg    = 1 << 3
)

const (
	iouOpPOLLADD     = 6
	iouOpASYNCCANCEL = 14
	iouOpSEND        = 26
	iouOpRECV        = 27
)

const (
	iouOffSQRing = 0
	iouOffCQRing = 0x8000000
	iouOffSQEs   = 0x10000000
)

const (
	iouSQESize = 64
	iouCQESize = 16
)

const (
	iouCancelFD  = 1 << 1
	iouCancelAll = 1 << 0
)

// User data encoding: upper 8 bits = op tag, lower 56 bits = fd or sentinel.
const (
	udRECV    uint64 = 0x01 << 56
	udSEND    uint64 = 0x02 << 56
	udCANCEL  uint64 = 0x03 << 56
	udEVENTFD uint64 = 0x04 << 56
	udPOLLOUT uint64 = 0x05 << 56
	udPOLLIN  uint64 = 0x06 << 56
	udTagMask uint64 = 0xFF << 56
	udFDMask  uint64 = (1 << 56) - 1
)

// ---------------------------------------------------------------------------
// io_uring ring structs
// ---------------------------------------------------------------------------

type iouParams struct {
	sqEntries    uint32
	cqEntries    uint32
	flags        uint32
	sqThreadCPU  uint32
	sqThreadIdle uint32
	features     uint32
	wqFD         uint32
	resv         [3]uint32
	sqOff        iouSQOff
	cqOff        iouCQOff
}

type iouSQOff struct {
	head, tail, ringMask, ringEntries, flags, dropped, array uint32
	resv1                                                    uint32
	userAddr                                                 uint64
}

type iouCQOff struct {
	head, tail, ringMask, ringEntries, overflow, cqes, flags uint32
	resv1                                                    uint32
	userAddr                                                 uint64
}

type iouCQE struct {
	UserData uint64
	Res      int32
	Flags    uint32
}

type iouTimespec struct {
	Sec  int64
	Nsec int64
}

type iouGeteventsArg struct {
	Sigmask   uint64
	SigmaskSz uint32
	Pad       uint32
	Ts        uint64
}

// ---------------------------------------------------------------------------
// iouRing — minimal io_uring wrapper
// ---------------------------------------------------------------------------

type iouRing struct {
	fd       int
	sqRing   []byte
	cqRing   []byte
	sqes     []byte
	params   iouParams
	sqMask   uint32
	cqMask   uint32
	pending  uint32
	sqHead   unsafe.Pointer
	sqTail   unsafe.Pointer
	sqArray  unsafe.Pointer
	cqHead   unsafe.Pointer
	cqTail   unsafe.Pointer
	cqesBase unsafe.Pointer
}

func newIouRing(entries uint32) (*iouRing, error) {
	var p iouParams
	p.flags = iouSetupCoopTaskrun

	fd, _, errno := unix.Syscall(
		uintptr(sysIOUringSetup),
		uintptr(entries),
		uintptr(unsafe.Pointer(&p)),
		0,
	)
	if errno != 0 {
		return nil, fmt.Errorf("io_uring_setup: %w", errno)
	}
	r := &iouRing{fd: int(fd), params: p}
	if err := r.mmap(); err != nil {
		_ = unix.Close(int(fd))
		return nil, err
	}
	return r, nil
}

func (r *iouRing) mmap() error {
	sqSize := uint64(r.params.sqOff.array) + uint64(r.params.sqEntries)*4
	var err error
	r.sqRing, err = unix.Mmap(r.fd, iouOffSQRing, int(sqSize),
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		return fmt.Errorf("mmap sq: %w", err)
	}

	cqSize := uint64(r.params.cqOff.cqes) + uint64(r.params.cqEntries)*uint64(iouCQESize)
	r.cqRing, err = unix.Mmap(r.fd, iouOffCQRing, int(cqSize),
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		_ = unix.Munmap(r.sqRing)
		return fmt.Errorf("mmap cq: %w", err)
	}

	sqesSize := uint64(r.params.sqEntries) * iouSQESize
	r.sqes, err = unix.Mmap(r.fd, iouOffSQEs, int(sqesSize),
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		_ = unix.Munmap(r.sqRing)
		_ = unix.Munmap(r.cqRing)
		return fmt.Errorf("mmap sqes: %w", err)
	}

	r.sqMask = *(*uint32)(unsafe.Pointer(&r.sqRing[r.params.sqOff.ringMask]))
	r.cqMask = *(*uint32)(unsafe.Pointer(&r.cqRing[r.params.cqOff.ringMask]))
	r.sqHead = unsafe.Pointer(&r.sqRing[r.params.sqOff.head])
	r.sqTail = unsafe.Pointer(&r.sqRing[r.params.sqOff.tail])
	r.sqArray = unsafe.Pointer(&r.sqRing[r.params.sqOff.array])
	r.cqHead = unsafe.Pointer(&r.cqRing[r.params.cqOff.head])
	r.cqTail = unsafe.Pointer(&r.cqRing[r.params.cqOff.tail])
	r.cqesBase = unsafe.Pointer(&r.cqRing[r.params.cqOff.cqes])
	return nil
}

func (r *iouRing) getSQE() unsafe.Pointer {
	tail := atomic.LoadUint32((*uint32)(r.sqTail))
	head := atomic.LoadUint32((*uint32)(r.sqHead))
	if tail-head >= r.params.sqEntries {
		return nil
	}
	idx := tail & r.sqMask
	*(*uint32)(unsafe.Add(r.sqArray, uintptr(idx)*4)) = idx
	sqePtr := unsafe.Add(unsafe.Pointer(&r.sqes[0]), uintptr(idx)*iouSQESize)
	*(*[48]byte)(sqePtr) = [48]byte{}
	// StoreUint32 ensures the kernel sees the updated tail with a
	// store-release barrier. Without this, the kernel may process stale
	// SQEs when io_uring_enter reads the tail.
	atomic.StoreUint32((*uint32)(r.sqTail), tail+1)
	r.pending++
	return sqePtr
}

func (r *iouRing) submit() error {
	if r.pending == 0 {
		return nil
	}
	n := r.pending
	r.pending = 0
	_, _, errno := unix.Syscall6(
		uintptr(sysIOUringEnter),
		uintptr(r.fd), uintptr(n), 0, 0, 0, 0,
	)
	if errno != 0 {
		return fmt.Errorf("io_uring_enter submit: %w", errno)
	}
	return nil
}

func (r *iouRing) submitAndWaitTimeout(timeout time.Duration) error {
	n := r.pending
	r.pending = 0
	ts := iouTimespec{
		Sec:  int64(timeout / time.Second),
		Nsec: int64(timeout % time.Second),
	}
	arg := iouGeteventsArg{
		Ts: uint64(uintptr(unsafe.Pointer(&ts))),
	}
	_, _, errno := unix.Syscall6(
		uintptr(sysIOUringEnter),
		uintptr(r.fd), uintptr(n), 1,
		uintptr(iouEnterGetEvents|iouEnterExtArg),
		uintptr(unsafe.Pointer(&arg)),
		unsafe.Sizeof(arg),
	)
	if errno != 0 {
		if errno == unix.ETIME || errno == unix.EINTR {
			return nil
		}
		return fmt.Errorf("io_uring_enter submit+wait: %w", errno)
	}
	return nil
}

func (r *iouRing) peekCQ() (head, tail uint32) {
	head = *(*uint32)(r.cqHead)
	tail = atomic.LoadUint32((*uint32)(r.cqTail))
	return
}

func (r *iouRing) cqeAt(head uint32) *iouCQE {
	idx := head & r.cqMask
	return (*iouCQE)(unsafe.Add(r.cqesBase, uintptr(idx)*iouCQESize))
}

func (r *iouRing) advanceCQ(newHead uint32) {
	atomic.StoreUint32((*uint32)(r.cqHead), newHead)
}

func (r *iouRing) close() error {
	if r.sqes != nil {
		_ = unix.Munmap(r.sqes)
	}
	if r.cqRing != nil {
		_ = unix.Munmap(r.cqRing)
	}
	if r.sqRing != nil {
		_ = unix.Munmap(r.sqRing)
	}
	return unix.Close(r.fd)
}

// ---------------------------------------------------------------------------
// SQE helpers
// ---------------------------------------------------------------------------

func iouPrepSend(sqe unsafe.Pointer, fd int, buf []byte) {
	s := (*[iouSQESize]byte)(sqe)
	s[0] = iouOpSEND
	*(*int32)(unsafe.Pointer(&s[4])) = int32(fd)
	if len(buf) > 0 {
		*(*uint64)(unsafe.Pointer(&s[16])) = uint64(uintptr(unsafe.Pointer(&buf[0])))
		*(*uint32)(unsafe.Pointer(&s[24])) = uint32(len(buf))
	}
}

func iouPrepPollAdd(sqe unsafe.Pointer, fd int, pollMask uint32) {
	s := (*[iouSQESize]byte)(sqe)
	s[0] = iouOpPOLLADD
	*(*int32)(unsafe.Pointer(&s[4])) = int32(fd)
	*(*uint32)(unsafe.Pointer(&s[28])) = pollMask // poll32_events at offset 28
}

func iouPrepCancel(sqe unsafe.Pointer, fd int) {
	s := (*[iouSQESize]byte)(sqe)
	s[0] = iouOpASYNCCANCEL
	*(*int32)(unsafe.Pointer(&s[4])) = int32(fd)
	*(*uint32)(unsafe.Pointer(&s[28])) = iouCancelFD | iouCancelAll
}

func iouSetUD(sqe unsafe.Pointer, data uint64) {
	*(*uint64)(unsafe.Pointer(uintptr(sqe) + 32)) = data
}

func iouEncodeUD(tag uint64, fd int) uint64 { return tag | (uint64(fd) & udFDMask) }

// ---------------------------------------------------------------------------
// iouConn — per-FD state for the io_uring driver loop
// ---------------------------------------------------------------------------

type iouConn struct {
	fd        int
	onRecv    func([]byte)
	onClose   func(error)
	buf       []byte
	writeBuf  []byte
	sendBuf   []byte
	sending   bool
	recvArmed bool

	mu      sync.Mutex
	closing bool
	closed  bool

	// recvMu serializes onRecv between the event-loop CQE handler and
	// WriteAndPoll's direct-read path.
	recvMu sync.Mutex

	inflightOps  int
	closePending bool
	closeErr     error
}

// ---------------------------------------------------------------------------
// iouWorker — one io_uring ring per worker
// ---------------------------------------------------------------------------

type iouWorker struct {
	id   int
	ring *iouRing

	mu    sync.RWMutex
	conns map[int]*iouConn

	closed atomic.Bool

	actionMu      sync.Mutex
	actionQueue   []iouAction
	actionSpare   []iouAction
	actionPending atomic.Int32

	eventFD  int
	eventBuf [8]byte // scratch for eventfd reads
}

type iouActionKind uint8

const (
	iouActionRegister iouActionKind = iota + 1
	iouActionUnregister
	iouActionWrite
)

type iouAction struct {
	kind iouActionKind
	c    *iouConn
}

// ---------------------------------------------------------------------------
// probeIOUring tests whether COOP_TASKRUN io_uring works.
// ---------------------------------------------------------------------------

func probeIOUring() bool {
	ring, err := newIouRing(4)
	if err != nil {
		return false
	}
	_ = ring.close()
	return true
}

// ---------------------------------------------------------------------------
// newIouringLoop creates a Loop backed by io_uring rings.
// ---------------------------------------------------------------------------

func newIouringLoop(nworkers int) (*Loop, error) {
	ctx, cancel := context.WithCancel(context.Background())
	l := &Loop{
		workers: make([]loopWorker, 0, nworkers),
		cancel:  cancel,
	}

	for i := 0; i < nworkers; i++ {
		w, err := newIouWorker(i)
		if err != nil {
			_ = l.shutdownPartial()
			cancel()
			return nil, err
		}
		l.workers = append(l.workers, w)
	}

	// Start worker goroutines with a ready barrier: each goroutine
	// signals readiness after its first submission (which arms the
	// eventfd RECV). This ensures RegisterConn can wake the worker
	// immediately via eventfd without racing goroutine scheduling.
	ready := make(chan struct{}, nworkers)
	for _, lw := range l.workers {
		l.wg.Add(1)
		go func(w *iouWorker) {
			defer l.wg.Done()
			w.runWithReady(ctx, ready)
		}(lw.(*iouWorker))
	}
	// Wait for all workers to signal ready.
	for range nworkers {
		<-ready
	}
	return l, nil
}

func newIouWorker(id int) (*iouWorker, error) {
	ring, err := newIouRing(256)
	if err != nil {
		return nil, fmt.Errorf("iou worker %d: %w", id, err)
	}
	efd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
	if err != nil {
		_ = ring.close()
		return nil, fmt.Errorf("eventfd: %w", err)
	}
	w := &iouWorker{
		id:      id,
		ring:    ring,
		conns:   make(map[int]*iouConn),
		eventFD: efd,
	}
	return w, nil
}

func (w *iouWorker) armEventFD() {
	sqe := w.ring.getSQE()
	if sqe == nil {
		return
	}
	iouPrepPollAdd(sqe, w.eventFD, unix.POLLIN)
	iouSetUD(sqe, udEVENTFD)
}

func (w *iouWorker) wake() {
	if w.eventFD < 0 {
		return
	}
	var val [8]byte
	val[0] = 1
	_, _ = unix.Write(w.eventFD, val[:])
}

// ---------------------------------------------------------------------------
// Event loop
// ---------------------------------------------------------------------------

func (w *iouWorker) runWithReady(ctx context.Context, ready chan<- struct{}) {
	// Arm and submit the eventfd POLL_ADD on this goroutine. This
	// ensures wake() can interrupt the worker from the very first
	// submitAndWaitTimeout.
	w.armEventFD()
	_ = w.ring.submit()
	ready <- struct{}{}
	w.run(ctx)
}

func (w *iouWorker) run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}

		// Process queued actions FIRST so that RegisterConn/Write/Unregister
		// SQEs are prepared before we submit+wait.
		w.drainActions()

		// Submit any SQEs from drainActions, then wait for completions.
		// The timeout is kept short (5ms) so actions enqueued between
		// iterations are picked up promptly. The eventfd wakeup provides
		// instant interruption for most cases; this timeout is the
		// worst-case fallback for races where the eventfd POLL_ADD SQE
		// is not yet submitted when wake() fires.
		_ = w.ring.submitAndWaitTimeout(5 * time.Millisecond)

		origHead, tail := w.ring.peekCQ()
		head := origHead
		for head != tail {
			cqe := w.ring.cqeAt(head)
			w.handleCQE(cqe)
			head++
		}
		if head != origHead {
			w.ring.advanceCQ(head)
		}

		// Drain actions again after processing CQEs — handleCQE may have
		// re-armed the eventfd POLL (which needs to be submitted), and new
		// actions may have been enqueued while we were processing completions.
		w.drainActions()

		// Submit any SQEs prepared by handleCQE (eventfd re-arm) and the
		// second drainActions without blocking. This ensures the eventfd
		// POLL_ADD is in-flight before the next wake() fires.
		_ = w.ring.submit()
	}
}

func (w *iouWorker) handleCQE(cqe *iouCQE) {
	tag := cqe.UserData & udTagMask
	switch tag {
	case udEVENTFD:
		// POLL_ADD fired — eventfd is readable. Consume the counter
		// with read(2) so the next POLL_ADD doesn't fire immediately,
		// then re-arm.
		_, _ = unix.Read(w.eventFD, w.eventBuf[:])
		w.armEventFD()
	case udPOLLIN:
		w.handlePollIn(cqe, int(cqe.UserData&udFDMask))
	case udSEND:
		w.handleSend(cqe, int(cqe.UserData&udFDMask))
	case udPOLLOUT:
		w.handlePollOut(int(cqe.UserData & udFDMask))
	case udCANCEL:
		w.handleCancel(int(cqe.UserData & udFDMask))
	}
}

// handlePollIn processes a POLL_ADD CQE for POLLIN. The POLL_ADD notifies
// that data is available; it does NOT consume data from the socket. We perform
// read(2) on the worker goroutine under recvMu, then re-arm the POLL_ADD.
// This mirrors the epoll worker's handleReadable and enables the WriteAndPoll
// sync fast path: the caller goroutine can safely do direct read(2) under
// recvMu because no kernel RECV is competing for socket data.
//
// TryLock avoids blocking the event loop when WriteAndPoll holds recvMu.
// If TryLock fails, data is left in the kernel buffer for WriteAndPoll to
// consume; we just re-arm POLL_ADD so the next readability edge fires.
func (w *iouWorker) handlePollIn(cqe *iouCQE, fd int) {
	w.mu.RLock()
	c := w.conns[fd]
	w.mu.RUnlock()
	if c == nil {
		return
	}
	c.mu.Lock()
	c.recvArmed = false
	c.inflightOps--
	closing := c.closing
	pending := c.closePending && c.inflightOps == 0
	closeErr := c.closeErr
	c.mu.Unlock()

	if pending {
		w.finalizeConn(c, closeErr)
		return
	}
	if closing {
		return
	}
	if cqe.Res < 0 {
		if cqe.Res == -int32(unix.ECANCELED) {
			return
		}
		w.finalizeConn(c, unix.Errno(uint32(-cqe.Res)))
		return
	}

	// TryLock: if WriteAndPoll holds recvMu, skip the read and do NOT
	// re-arm POLL_ADD. WriteAndPoll will consume the data. After it
	// releases recvMu, re-arming happens via the deferred armPollIn
	// call in WriteAndPoll/WriteAndPollMulti.
	if !c.recvMu.TryLock() {
		return
	}
	// read(2) under recvMu, drain to EAGAIN (same as epoll handleReadable).
	var readErr error
	for {
		n, err := unix.Read(fd, c.buf)
		if n > 0 && c.onRecv != nil {
			c.onRecv(c.buf[:n])
		}
		if err != nil {
			if isEAGAIN(err) {
				break
			}
			readErr = err
			break
		}
		if n == 0 {
			c.recvMu.Unlock()
			w.finalizeConn(c, nil)
			return
		}
	}
	c.recvMu.Unlock()

	if readErr != nil {
		w.finalizeConn(c, readErr)
		return
	}

	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	w.armPollIn(c)
}

func (w *iouWorker) handleSend(cqe *iouCQE, fd int) {
	w.mu.RLock()
	c := w.conns[fd]
	w.mu.RUnlock()
	if c == nil {
		return
	}
	c.mu.Lock()
	c.sending = false
	c.inflightOps--
	pending := c.closePending && c.inflightOps == 0
	closeErr := c.closeErr

	if cqe.Res < 0 {
		errno := unix.Errno(uint32(-cqe.Res))
		if errno == unix.EAGAIN || errno == unix.EWOULDBLOCK {
			// Socket send buffer full. Submit POLL_ADD for POLLOUT so
			// the CQE fires as soon as the socket is writable, avoiding
			// the ~5ms action-queue round-trip latency.
			c.mu.Unlock()
			sqe := w.ring.getSQE()
			if sqe != nil {
				iouPrepPollAdd(sqe, fd, unix.POLLOUT)
				iouSetUD(sqe, iouEncodeUD(udPOLLOUT, fd))
			} else {
				// SQ full fallback: re-post via action queue.
				w.enqueueAction(iouAction{kind: iouActionWrite, c: c})
			}
			return
		}
		c.mu.Unlock()
		if pending {
			w.finalizeConn(c, closeErr)
			return
		}
		if cqe.Res == -int32(unix.ECANCELED) {
			return
		}
		w.finalizeConn(c, errno)
		return
	}

	sent := int(cqe.Res)
	if sent < len(c.sendBuf) {
		remaining := len(c.sendBuf) - sent
		copy(c.sendBuf, c.sendBuf[sent:])
		c.sendBuf = c.sendBuf[:remaining]
	} else {
		c.sendBuf = c.sendBuf[:0]
	}
	hasMore := len(c.sendBuf) > 0 || len(c.writeBuf) > 0
	closing := c.closing
	c.mu.Unlock()

	if pending {
		w.finalizeConn(c, closeErr)
		return
	}
	if closing {
		return
	}
	if hasMore {
		w.flushSend(c)
	}
}

func (w *iouWorker) handleCancel(fd int) {
	w.mu.RLock()
	c := w.conns[fd]
	w.mu.RUnlock()
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.inflightOps > 0 {
		c.closePending = true
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	w.finalizeConn(c, nil)
}

func (w *iouWorker) handlePollOut(fd int) {
	w.mu.RLock()
	c := w.conns[fd]
	w.mu.RUnlock()
	if c == nil {
		return
	}
	w.flushSend(c)
}

// ---------------------------------------------------------------------------
// SQE arming helpers
// ---------------------------------------------------------------------------

func (w *iouWorker) armPollIn(c *iouConn) {
	c.mu.Lock()
	if c.closing || c.recvArmed {
		c.mu.Unlock()
		return
	}
	sqe := w.ring.getSQE()
	if sqe == nil {
		c.mu.Unlock()
		w.enqueueAction(iouAction{kind: iouActionRegister, c: c})
		return
	}
	iouPrepPollAdd(sqe, c.fd, unix.POLLIN)
	iouSetUD(sqe, iouEncodeUD(udPOLLIN, c.fd))
	c.recvArmed = true
	c.inflightOps++
	c.mu.Unlock()
}

func (w *iouWorker) flushSend(c *iouConn) {
	c.mu.Lock()
	if c.closing || c.sending {
		c.mu.Unlock()
		return
	}
	if len(c.sendBuf) == 0 {
		if len(c.writeBuf) == 0 {
			c.mu.Unlock()
			return
		}
		c.sendBuf, c.writeBuf = c.writeBuf, c.sendBuf[:0]
	}
	sqe := w.ring.getSQE()
	if sqe == nil {
		if len(c.writeBuf) == 0 {
			c.writeBuf, c.sendBuf = c.sendBuf, c.writeBuf[:0]
		}
		c.mu.Unlock()
		w.enqueueAction(iouAction{kind: iouActionWrite, c: c})
		return
	}
	iouPrepSend(sqe, c.fd, c.sendBuf)
	iouSetUD(sqe, iouEncodeUD(udSEND, c.fd))
	c.sending = true
	c.inflightOps++
	c.mu.Unlock()
}

func (w *iouWorker) cancelFD(c *iouConn) {
	sqe := w.ring.getSQE()
	if sqe == nil {
		w.enqueueAction(iouAction{kind: iouActionUnregister, c: c})
		return
	}
	iouPrepCancel(sqe, c.fd)
	iouSetUD(sqe, iouEncodeUD(udCANCEL, c.fd))
}

func (w *iouWorker) finalizeConn(c *iouConn, err error) {
	w.mu.Lock()
	existing, ok := w.conns[c.fd]
	if !ok || existing != c {
		w.mu.Unlock()
		return
	}
	delete(w.conns, c.fd)
	w.mu.Unlock()

	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	cb := c.onClose
	c.onClose = nil
	if cb != nil {
		cb(err)
	}
}

// ---------------------------------------------------------------------------
// Action queue (driver goroutines → worker)
// ---------------------------------------------------------------------------

func (w *iouWorker) enqueueAction(a iouAction) {
	w.actionMu.Lock()
	w.actionQueue = append(w.actionQueue, a)
	w.actionPending.Store(1)
	w.actionMu.Unlock()
	w.wake()
}

func (w *iouWorker) drainActions() {
	if w.actionPending.Load() == 0 {
		return
	}
	w.actionMu.Lock()
	w.actionSpare, w.actionQueue = w.actionQueue, w.actionSpare[:0]
	w.actionPending.Store(0)
	w.actionMu.Unlock()

	for _, a := range w.actionSpare {
		switch a.kind {
		case iouActionRegister:
			w.armPollIn(a.c)
		case iouActionUnregister:
			w.cancelFD(a.c)
		case iouActionWrite:
			w.flushSend(a.c)
		}
	}
	w.actionSpare = w.actionSpare[:0]
}

// ---------------------------------------------------------------------------
// engine.WorkerLoop implementation
// ---------------------------------------------------------------------------

func (w *iouWorker) RegisterConn(fd int, onRecv func([]byte), onClose func(error)) error {
	if w.closed.Load() {
		return ErrLoopClosed
	}
	if fd < 0 {
		return fmt.Errorf("celeris/eventloop: negative fd")
	}
	w.mu.Lock()
	if _, ok := w.conns[fd]; ok {
		w.mu.Unlock()
		return ErrAlreadyRegistered
	}
	c := &iouConn{
		fd:      fd,
		onRecv:  onRecv,
		onClose: onClose,
		buf:     make([]byte, 16<<10),
	}
	w.conns[fd] = c
	w.mu.Unlock()

	w.enqueueAction(iouAction{kind: iouActionRegister, c: c})
	return nil
}

func (w *iouWorker) UnregisterConn(fd int) error {
	w.mu.RLock()
	c, ok := w.conns[fd]
	w.mu.RUnlock()
	if !ok {
		return engine.ErrUnknownFD
	}
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return nil
	}
	c.closing = true
	c.mu.Unlock()
	w.enqueueAction(iouAction{kind: iouActionUnregister, c: c})
	return nil
}

func (w *iouWorker) Write(fd int, data []byte) error {
	if w.closed.Load() {
		return ErrLoopClosed
	}
	w.mu.RLock()
	c, ok := w.conns[fd]
	w.mu.RUnlock()
	if !ok {
		return engine.ErrUnknownFD
	}
	c.mu.Lock()
	if c.closing || c.closed {
		c.mu.Unlock()
		return engine.ErrUnknownFD
	}
	if len(c.writeBuf)+len(c.sendBuf)+len(data) > maxPendingBytes {
		c.mu.Unlock()
		return engine.ErrQueueFull
	}
	c.writeBuf = append(c.writeBuf, data...)
	needSubmit := !c.sending
	c.mu.Unlock()
	if needSubmit {
		w.enqueueAction(iouAction{kind: iouActionWrite, c: c})
	}
	return nil
}

func (w *iouWorker) CPUID() int { return -1 }

// ---------------------------------------------------------------------------
// SyncRoundTripper: WriteAndPoll
//
// The io_uring standalone loop uses POLL_ADD (not RECV) for read-readiness
// notification — no kernel RECV consumes socket data. This allows the caller
// goroutine to do direct write(2) + poll(2) + read(2), identical to the epoll
// fast path. recvMu serializes with handlePollIn on the event-loop goroutine.
// ---------------------------------------------------------------------------

// WriteAndPoll implements SyncRoundTripper. It writes data to fd with direct
// write(2), then polls for the response on the calling goroutine using the
// same three-phase strategy as the epoll path: spin read, poll(0)+Gosched,
// poll(1ms). Because the io_uring standalone loop uses POLL_ADD for read
// notifications (not kernel RECV), no in-flight io_uring op competes for
// socket data, so direct read(2) is safe.
func (w *iouWorker) WriteAndPoll(fd int, data []byte, rbuf []byte, onRecv func([]byte)) (bool, error) {
	if w.closed.Load() {
		return false, ErrLoopClosed
	}
	w.mu.RLock()
	c, ok := w.conns[fd]
	w.mu.RUnlock()
	if !ok {
		return false, engine.ErrUnknownFD
	}

	// Step 1: Direct write from caller goroutine.
	c.mu.Lock()
	if c.closing || c.closed {
		c.mu.Unlock()
		return false, engine.ErrUnknownFD
	}
	if c.sending || len(c.sendBuf) > 0 || len(c.writeBuf) > 0 {
		// Concurrent async write in progress — fall back.
		c.mu.Unlock()
		return false, nil
	}
	c.mu.Unlock()

	written := 0
	for written < len(data) {
		n, err := unix.Write(fd, data[written:])
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			if isEAGAIN(err) {
				break
			}
			w.finalizeConn(c, err)
			return false, err
		}
		if n == 0 {
			break
		}
		written += n
	}
	if written < len(data) {
		// Partial write — queue remainder for async flush.
		c.mu.Lock()
		c.writeBuf = append(c.writeBuf, data[written:]...)
		c.mu.Unlock()
		w.enqueueAction(iouAction{kind: iouActionWrite, c: c})
	}

	// Step 2: Take recvMu so any in-flight handlePollIn completes before
	// we start reading. POLL_ADD is one-shot and does not consume data,
	// so no kernel op competes with our read(2).
	c.recvMu.Lock()

	// Step 3: Three-phase poll for the response.
	const spinRounds = 512
	gotData := false
	var readErr error
	// Phase A: spin read.
	for range spinRounds {
		n, err := unix.Read(fd, rbuf)
		if n > 0 {
			gotData = true
			onRecv(rbuf[:n])
			for {
				n2, err2 := unix.Read(fd, rbuf)
				if n2 > 0 {
					onRecv(rbuf[:n2])
					continue
				}
				if err2 != nil {
					break
				}
				if n2 == 0 {
					break
				}
			}
			break
		}
		if err != nil {
			if isEAGAIN(err) {
				continue
			}
			readErr = err
			break
		}
		if n == 0 {
			break
		}
	}
	// Phase B: poll(0) + Gosched.
	if !gotData && readErr == nil {
		var pfd [1]unix.PollFd
		pfd[0].Fd = int32(fd)
		pfd[0].Events = unix.POLLIN
		for range 32 {
			pfd[0].Revents = 0
			np, perr := unix.Poll(pfd[:], 0)
			if np > 0 && perr == nil && pfd[0].Revents&unix.POLLIN != 0 {
				for {
					n, err := unix.Read(fd, rbuf)
					if n > 0 {
						gotData = true
						onRecv(rbuf[:n])
						continue
					}
					if err != nil {
						if isEAGAIN(err) {
							break
						}
						readErr = err
					}
					break
				}
				break
			}
			runtime.Gosched()
		}
	}
	// Phase C: blocking poll(1ms).
	if !gotData && readErr == nil {
		var pfd [1]unix.PollFd
		pfd[0].Fd = int32(fd)
		pfd[0].Events = unix.POLLIN
		np, perr := unix.Poll(pfd[:], 1)
		if np > 0 && perr == nil && pfd[0].Revents&unix.POLLIN != 0 {
			for {
				n, err := unix.Read(fd, rbuf)
				if n > 0 {
					gotData = true
					onRecv(rbuf[:n])
					continue
				}
				if err != nil {
					if isEAGAIN(err) {
						break
					}
					readErr = err
				}
				break
			}
		}
	}

	// Step 4: Final drain to EAGAIN.
	if gotData && readErr == nil {
		for {
			n, err := unix.Read(fd, rbuf)
			if n > 0 {
				onRecv(rbuf[:n])
				continue
			}
			if err != nil {
				if isEAGAIN(err) {
					break
				}
				readErr = err
			}
			break
		}
	}

	c.recvMu.Unlock()

	// Re-arm POLL_ADD so the event loop can deliver async data (pub/sub
	// messages, connection errors) that may arrive between WriteAndPoll
	// calls. Because we drained to EAGAIN above, the POLL_ADD won't fire
	// immediately. The enqueueAction cost (~5µs) is negligible vs the
	// round-trip time.
	w.enqueueAction(iouAction{kind: iouActionRegister, c: c})

	if readErr != nil {
		w.finalizeConn(c, readErr)
		return false, readErr
	}
	if !gotData {
		return false, nil
	}
	return true, nil
}

// WriteAndPollMulti implements SyncMultiRoundTripper. It writes data, then
// polls in a loop until isDone returns true or the cumulative poll timeout
// expires. Uses direct syscalls (same as epoll path) since the io_uring
// standalone loop uses POLL_ADD for read notifications, not kernel RECV.
func (w *iouWorker) WriteAndPollMulti(fd int, data []byte, rbuf []byte, onRecv func([]byte), isDone func() bool, beforeRearm func()) (bool, error) {
	if w.closed.Load() {
		return false, ErrLoopClosed
	}
	w.mu.RLock()
	c, ok := w.conns[fd]
	w.mu.RUnlock()
	if !ok {
		return false, engine.ErrUnknownFD
	}

	// Step 1: Direct write.
	c.mu.Lock()
	if c.closing || c.closed {
		c.mu.Unlock()
		return false, engine.ErrUnknownFD
	}
	if c.sending || len(c.sendBuf) > 0 || len(c.writeBuf) > 0 {
		c.mu.Unlock()
		// Contended — fall back to async with beforeRearm under recvMu.
		err := w.Write(fd, data)
		if err != nil {
			return false, err
		}
		c.recvMu.Lock()
		if beforeRearm != nil {
			beforeRearm()
		}
		c.recvMu.Unlock()
		return false, nil
	}
	c.mu.Unlock()

	written := 0
	for written < len(data) {
		n, err := unix.Write(fd, data[written:])
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			if isEAGAIN(err) {
				break
			}
			w.finalizeConn(c, err)
			return false, err
		}
		if n == 0 {
			break
		}
		written += n
	}
	if written < len(data) {
		c.mu.Lock()
		c.writeBuf = append(c.writeBuf, data[written:]...)
		c.mu.Unlock()
		w.enqueueAction(iouAction{kind: iouActionWrite, c: c})
	}

	// Step 2: Take recvMu.
	c.recvMu.Lock()

	// Step 3: Read loop until isDone or cumulative timeout.
	gotData := false
	var readErr error
	var pfd [1]unix.PollFd
	pfd[0].Fd = int32(fd)
	pfd[0].Events = unix.POLLIN

	// Initial read: catches responses already in the kernel buffer.
	for {
		n, err := unix.Read(fd, rbuf)
		if n > 0 {
			gotData = true
			onRecv(rbuf[:n])
			continue
		}
		if err != nil {
			if !isEAGAIN(err) {
				readErr = err
			}
		}
		break
	}
	if readErr != nil || (gotData && isDone()) {
		goto done
	}

	// Poll-drain loop: block until data is readable, drain to EAGAIN,
	// check isDone, repeat. Budget: 50 rounds x 1ms = 50ms max.
	for range 50 {
		pfd[0].Revents = 0
		np, perr := unix.Poll(pfd[:], 1)
		if np > 0 && perr == nil && pfd[0].Revents&unix.POLLIN != 0 {
			for {
				n, err := unix.Read(fd, rbuf)
				if n > 0 {
					gotData = true
					onRecv(rbuf[:n])
					continue
				}
				if err != nil {
					if !isEAGAIN(err) {
						readErr = err
					}
				}
				break
			}
			if readErr != nil {
				goto done
			}
			if isDone() {
				goto done
			}
			continue
		}
		if perr != nil && perr != unix.EINTR {
			break
		}
		if gotData {
			continue
		}
		break
	}

done:
	// Final drain to EAGAIN.
	if gotData && readErr == nil {
		for {
			n, err := unix.Read(fd, rbuf)
			if n > 0 {
				onRecv(rbuf[:n])
				continue
			}
			if err != nil {
				if isEAGAIN(err) {
					break
				}
				readErr = err
			}
			break
		}
	}

	if beforeRearm != nil {
		beforeRearm()
	}

	c.recvMu.Unlock()

	// Re-arm POLL_ADD for async data delivery between calls.
	w.enqueueAction(iouAction{kind: iouActionRegister, c: c})

	if readErr != nil {
		w.finalizeConn(c, readErr)
		return false, readErr
	}
	if gotData && isDone() {
		return true, nil
	}
	return false, nil
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

func (w *iouWorker) shutdown() error {
	if !w.closed.CompareAndSwap(false, true) {
		return nil
	}
	w.mu.Lock()
	conns := make([]*iouConn, 0, len(w.conns))
	for _, c := range w.conns {
		conns = append(conns, c)
	}
	w.conns = map[int]*iouConn{}
	w.mu.Unlock()

	for _, c := range conns {
		cb := c.onClose
		c.onClose = nil
		if cb != nil {
			cb(ErrLoopClosed)
		}
	}

	var first error
	if w.eventFD >= 0 {
		if err := unix.Close(w.eventFD); err != nil && first == nil {
			first = err
		}
		w.eventFD = -1
	}
	if err := w.ring.close(); err != nil && first == nil {
		first = err
	}
	return first
}

// Compile-time assertions.
var (
	_ loopWorker         = (*iouWorker)(nil)
	_ SyncRoundTripper   = (*iouWorker)(nil)
	_ SyncMultiRoundTripper = (*iouWorker)(nil)
)
