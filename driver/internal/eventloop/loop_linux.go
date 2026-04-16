//go:build linux

package eventloop

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
)

// isEAGAIN is a fast check for EAGAIN/EWOULDBLOCK that avoids the reflection
// overhead of errors.Is. On Linux EAGAIN == EWOULDBLOCK (both 11), so a
// single comparison suffices. unix.Read/unix.Write return syscall.Errno
// directly (not wrapped), so the type assertion always matches.
func isEAGAIN(err error) bool {
	return err == syscall.EAGAIN
}

// worker owns a single epoll instance and the FDs registered on it.
type worker struct {
	id      int
	epollFD int
	eventFD int

	mu    sync.RWMutex
	conns map[int]*driverConn // fd -> state (protected by mu)

	// pending holds FDs whose outbound buffers have fresh bytes. Writers
	// append, the worker goroutine drains. Access guarded by pendingMu.
	pendingMu sync.Mutex
	pending   []int

	closed atomic.Bool
	events []unix.EpollEvent
	rbuf   []byte
}

// driverConn holds per-FD state. writeBuf/writePos mirror the HTTP epoll
// path but a per-FD mutex replaces the implicit event-loop serialization:
// drivers may call Write from any goroutine and the worker goroutine
// simultaneously drains the buffer, so both sides coordinate through mu.
type driverConn struct {
	fd      int
	onRecv  func([]byte)
	onClose func(error)

	mu       sync.Mutex // guards writeBuf, writePos, sending, closing, closed
	writeBuf []byte
	writePos int
	sending  bool // true while a goroutine is draining writeBuf
	closing  bool // UnregisterConn or error path requested teardown
	closed   bool // onClose has fired
	epollOut bool // EPOLLOUT currently armed on this fd

	// recvMu serializes onRecv calls between the event-loop worker
	// (handleReadable) and WriteAndPoll (caller goroutine). Without this,
	// an in-flight handleReadable from a prior epoll_wait batch can race
	// with WriteAndPoll's caller-side reads.
	recvMu sync.Mutex
}

func newLoop(workers int) (*Loop, error) {
	// The standalone driver loop uses epoll. io_uring's SINGLE_ISSUER
	// constraint conflicts with the WriteAndPoll sync fast path (caller
	// goroutine does direct read/write while the ring worker goroutine
	// owns SQE submission). The HTTP engine's io_uring path is separate
	// and unaffected — it uses its own ring per worker.
	return newEpollLoop(workers)
}

func newEpollLoop(workers int) (*Loop, error) {
	l := &Loop{workers: make([]loopWorker, 0, workers)}
	ctx, cancel := context.WithCancel(context.Background())
	l.cancel = cancel

	for i := 0; i < workers; i++ {
		w, err := newWorker(i)
		if err != nil {
			_ = l.shutdownPartial()
			cancel()
			return nil, err
		}
		l.workers = append(l.workers, w)
	}

	for _, lw := range l.workers {
		l.wg.Add(1)
		go func(w *worker) {
			defer l.wg.Done()
			w.run(ctx)
		}(lw.(*worker))
	}
	return l, nil
}

func newWorker(id int) (*worker, error) {
	epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("epoll_create1: %w", err)
	}
	efd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
	if err != nil {
		_ = unix.Close(epfd)
		return nil, fmt.Errorf("eventfd: %w", err)
	}
	if err := unix.EpollCtl(epfd, unix.EPOLL_CTL_ADD, efd, &unix.EpollEvent{
		Events: unix.EPOLLIN | unix.EPOLLET,
		Fd:     int32(efd),
	}); err != nil {
		_ = unix.Close(efd)
		_ = unix.Close(epfd)
		return nil, fmt.Errorf("epoll_ctl eventfd: %w", err)
	}
	return &worker{
		id:      id,
		epollFD: epfd,
		eventFD: efd,
		conns:   make(map[int]*driverConn),
		events:  make([]unix.EpollEvent, 128),
		rbuf:    make([]byte, 16<<10),
	}, nil
}

func (w *worker) shutdown() error {
	if !w.closed.CompareAndSwap(false, true) {
		return nil
	}
	// Fire onClose for any still-registered FDs before tearing down. Hold
	// w.mu across the epoll/event fd teardown so late-arriving
	// UnregisterConn/Register callers (e.g. pgConn.Close racing Loop.Close)
	// observe the swap to -1 under the lock rather than a torn read.
	w.mu.Lock()
	conns := make([]*driverConn, 0, len(w.conns))
	for _, c := range w.conns {
		conns = append(conns, c)
	}
	w.conns = map[int]*driverConn{}
	var first error
	if w.eventFD >= 0 {
		if err := unix.Close(w.eventFD); err != nil && first == nil {
			first = err
		}
		w.eventFD = -1
	}
	if w.epollFD >= 0 {
		if err := unix.Close(w.epollFD); err != nil && first == nil {
			first = err
		}
		w.epollFD = -1
	}
	w.mu.Unlock()
	for _, c := range conns {
		c.mu.Lock()
		fired := c.closed
		c.closed = true
		cb := c.onClose
		c.mu.Unlock()
		if !fired && cb != nil {
			cb(ErrLoopClosed)
		}
	}
	return first
}

// wake triggers the worker's epoll_wait to return via the eventfd.
func (w *worker) wake() {
	if w.eventFD < 0 {
		return
	}
	var val [8]byte
	val[0] = 1
	_, _ = unix.Write(w.eventFD, val[:])
}

// CPUID reports the CPU the worker is pinned to. Standalone loops do not
// pin (the Go scheduler is free to migrate the goroutine), so this is -1.
func (w *worker) CPUID() int { return -1 }

// RegisterConn satisfies [engine.WorkerLoop].
func (w *worker) RegisterConn(fd int, onRecv func([]byte), onClose func(error)) error {
	if w.closed.Load() {
		return ErrLoopClosed
	}
	if fd < 0 {
		return errors.New("celeris/eventloop: negative fd")
	}
	c := &driverConn{fd: fd, onRecv: onRecv, onClose: onClose}
	w.mu.Lock()
	if w.epollFD < 0 {
		w.mu.Unlock()
		return ErrLoopClosed
	}
	if _, ok := w.conns[fd]; ok {
		w.mu.Unlock()
		return ErrAlreadyRegistered
	}
	w.conns[fd] = c
	epfd := w.epollFD
	w.mu.Unlock()

	if err := unix.EpollCtl(epfd, unix.EPOLL_CTL_ADD, fd, &unix.EpollEvent{
		Events: unix.EPOLLIN | unix.EPOLLET | unix.EPOLLRDHUP,
		Fd:     int32(fd),
	}); err != nil {
		w.mu.Lock()
		delete(w.conns, fd)
		w.mu.Unlock()
		return fmt.Errorf("epoll_ctl add: %w", err)
	}
	return nil
}

// UnregisterConn satisfies [engine.WorkerLoop]. It removes fd from the epoll
// set and fires onClose(nil) if not already closed. The caller owns the fd
// and is responsible for closing it.
func (w *worker) UnregisterConn(fd int) error {
	w.mu.Lock()
	c, ok := w.conns[fd]
	if ok {
		delete(w.conns, fd)
	}
	epfd := w.epollFD
	w.mu.Unlock()
	if !ok {
		return engine.ErrUnknownFD
	}
	if epfd >= 0 {
		_ = unix.EpollCtl(epfd, unix.EPOLL_CTL_DEL, fd, nil)
	}

	c.mu.Lock()
	fired := c.closed
	c.closed = true
	c.closing = true
	cb := c.onClose
	c.mu.Unlock()
	if !fired && cb != nil {
		cb(nil)
	}
	return nil
}

// Write satisfies [engine.WorkerLoop]. Data is appended to the FD's outbound
// buffer and flushed asynchronously under c.mu (one write(2) in flight per
// FD). Returns engine.ErrQueueFull when the buffer would exceed the cap.
func (w *worker) Write(fd int, data []byte) error {
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
	if len(c.writeBuf)-c.writePos+len(data) > maxPendingBytes {
		c.mu.Unlock()
		return engine.ErrQueueFull
	}
	c.writeBuf = append(c.writeBuf, data...)
	if c.sending {
		// Another goroutine is already draining; it will observe the
		// appended bytes before returning.
		c.mu.Unlock()
		return nil
	}
	c.sending = true
	// Drain under mu — this serializes writes (one write(2) per FD in
	// flight) and is bounded because we release the lock when the kernel
	// returns EAGAIN, handing further drainage to the worker goroutine.
	err := w.flushLocked(c)
	c.sending = false
	pending := c.writePos < len(c.writeBuf)
	c.mu.Unlock()

	if err != nil {
		w.errorClose(fd, err)
		return err
	}
	if pending {
		w.enqueueFlush(fd)
	}
	return nil
}

// flushLocked drains c.writeBuf into the kernel. Caller must hold c.mu.
// Returns nil on success (whether fully or partially drained); returns a
// non-EAGAIN error when the connection must be torn down.
//
// On EAGAIN, arms EPOLLOUT (edge-triggered) so the worker goroutine wakes
// when the socket becomes writable again; on full drain, disarms EPOLLOUT
// so idle conns don't wake the loop on every send-buffer drain.
func (w *worker) flushLocked(c *driverConn) error {
	for c.writePos < len(c.writeBuf) {
		n, err := unix.Write(c.fd, c.writeBuf[c.writePos:])
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			if isEAGAIN(err) {
				if !c.epollOut {
					modErr := unix.EpollCtl(w.epollFD, unix.EPOLL_CTL_MOD, c.fd, &unix.EpollEvent{
						Events: unix.EPOLLIN | unix.EPOLLOUT | unix.EPOLLET | unix.EPOLLRDHUP,
						Fd:     int32(c.fd),
					})
					if modErr == nil {
						c.epollOut = true
					}
				}
				return nil
			}
			c.writeBuf = c.writeBuf[:0]
			c.writePos = 0
			return err
		}
		if n == 0 {
			return nil
		}
		c.writePos += n
	}
	// Fully drained — reset for reuse.
	c.writeBuf = c.writeBuf[:0]
	c.writePos = 0
	if c.epollOut {
		_ = unix.EpollCtl(w.epollFD, unix.EPOLL_CTL_MOD, c.fd, &unix.EpollEvent{
			Events: unix.EPOLLIN | unix.EPOLLET | unix.EPOLLRDHUP,
			Fd:     int32(c.fd),
		})
		c.epollOut = false
	}
	return nil
}

// enqueueFlush appends fd to the worker's pending list and wakes the
// goroutine so it retries the write when the socket is writable.
func (w *worker) enqueueFlush(fd int) {
	w.pendingMu.Lock()
	w.pending = append(w.pending, fd)
	w.pendingMu.Unlock()
	w.wake()
}

// errorClose tears down a connection after a fatal I/O error.
func (w *worker) errorClose(fd int, err error) {
	w.mu.Lock()
	c, ok := w.conns[fd]
	if ok {
		delete(w.conns, fd)
	}
	w.mu.Unlock()
	if !ok {
		return
	}
	_ = unix.EpollCtl(w.epollFD, unix.EPOLL_CTL_DEL, fd, nil)
	c.mu.Lock()
	fired := c.closed
	c.closed = true
	c.closing = true
	cb := c.onClose
	c.mu.Unlock()
	if !fired && cb != nil {
		cb(err)
	}
}

// SyncRoundTripper is an optional interface for workers that support a
// combined write+read fast path. The driver calls WriteAndPoll to send query
// bytes and then poll for the response on the calling goroutine (no event-loop
// round trip, no channel signal, no futex). The caller must supply a readBuf
// and the same onRecv callback as RegisterConn. If the socket returns EAGAIN
// before any data is read, ok=false is returned and the caller should fall back
// to the normal async path.
//
// Contract:
//   - EPOLLIN is temporarily masked while the caller reads, preventing the
//     event loop from racing the read. It is restored on return.
//   - The caller must NOT hold c.mu during the read (flushLocked already
//     released it). Only the writing side holds c.mu; the read side is
//     serialized by the EPOLLIN mask.
//   - Edge-triggered epoll: we drain to EAGAIN inside WriteAndPoll, so no
//     stale edge is left. After EPOLLIN is re-armed, the next kernel-buffer
//     arrival fires a fresh edge.
type SyncRoundTripper interface {
	WriteAndPoll(fd int, data []byte, rbuf []byte, onRecv func([]byte)) (ok bool, err error)
}

// WriteAndPoll implements SyncRoundTripper. It writes data to fd, then polls
// for the response directly on the calling goroutine. If data arrives within
// the poll window, it invokes onRecv and returns ok=true. If the socket
// returns EAGAIN before any data, ok=false.
func (w *worker) WriteAndPoll(fd int, data []byte, rbuf []byte, onRecv func([]byte)) (bool, error) {
	if w.closed.Load() {
		return false, ErrLoopClosed
	}
	w.mu.RLock()
	c, ok := w.conns[fd]
	epfd := w.epollFD
	w.mu.RUnlock()
	if !ok {
		return false, engine.ErrUnknownFD
	}

	// Step 1: Write (same logic as worker.Write).
	c.mu.Lock()
	if c.closing || c.closed {
		c.mu.Unlock()
		return false, engine.ErrUnknownFD
	}
	if len(c.writeBuf)-c.writePos+len(data) > maxPendingBytes {
		c.mu.Unlock()
		return false, engine.ErrQueueFull
	}
	c.writeBuf = append(c.writeBuf, data...)
	if c.sending {
		c.mu.Unlock()
		return false, nil // fall back: concurrent write in progress
	}
	c.sending = true
	werr := w.flushLocked(c)
	c.sending = false
	pending := c.writePos < len(c.writeBuf)
	c.mu.Unlock()
	if werr != nil {
		w.errorClose(fd, werr)
		return false, werr
	}
	if pending {
		w.enqueueFlush(fd)
	}

	// Step 2: Take recvMu so any in-flight handleReadable (from a prior
	// epoll_wait batch) completes before we start reading. Then mask EPOLLIN
	// so no NEW readable events arrive for this fd while we hold the lock.
	c.recvMu.Lock()
	evMask := uint32(unix.EPOLLET | unix.EPOLLRDHUP)
	if c.epollOut {
		evMask |= unix.EPOLLOUT
	}
	_ = unix.EpollCtl(epfd, unix.EPOLL_CTL_MOD, fd, &unix.EpollEvent{
		Events: evMask,
		Fd:     int32(fd),
	})

	// Step 3: Poll for the response. Three phases:
	//   Phase A: 512 non-blocking reads. Catches ultra-fast responses
	//            without any kernel involvement.
	//   Phase B: 32 non-blocking poll(0) + Gosched() rounds (~50-200µs).
	//            Catches localhost responses without paying the 1ms poll
	//            ceiling that dominates latency on fast networks.
	//   Phase C: poll(2) with 1ms timeout as last resort before falling
	//            back to the full event-loop path.
	const spinRounds = 512
	gotData := false
	var readErr error
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
					readErr = nil
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
	// Phase B: non-blocking poll(0) + Gosched() spin. Each round costs
	// ~1-3µs (syscall + scheduler yield) vs 1ms for a blocking poll.
	// 32 rounds ≈ 50-100µs budget — well within localhost RTT.
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
	// Phase C: blocking poll(1ms) as last resort.
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

	// Step 4: Final drain to EAGAIN before re-arming EPOLLIN.
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

	// Step 5: Re-enable EPOLLIN and release recvMu. After this, the
	// event-loop worker owns reads on this fd again.
	evMask = unix.EPOLLIN | unix.EPOLLET | unix.EPOLLRDHUP
	if c.epollOut {
		evMask |= unix.EPOLLOUT
	}
	_ = unix.EpollCtl(epfd, unix.EPOLL_CTL_MOD, fd, &unix.EpollEvent{
		Events: evMask,
		Fd:     int32(fd),
	})
	c.recvMu.Unlock()

	if readErr != nil {
		w.errorClose(fd, readErr)
		return false, readErr
	}
	if !gotData {
		return false, nil // EAGAIN — fall back to event-loop path
	}
	return true, nil
}

// SyncMultiRoundTripper extends SyncRoundTripper with a multi-response
// variant for pipelined protocols. WriteAndPollMulti writes data, then
// repeatedly polls and reads until isDone reports true or a hard timeout
// expires. isDone is called after each onRecv batch (under recvMu) so the
// driver can check how many protocol frames have been parsed.
//
// beforeRearm runs after all reads complete but before EPOLLIN is re-armed
// and recvMu is released. The driver uses it to transition from direct-index
// dispatch to bridge-queue dispatch so the event loop sees a consistent
// state when it resumes reads on this fd. Pass nil to skip.
//
// Returns ok=true when isDone fired. ok=false means the caller should fall
// back to the async event-loop path (no data arrived or isDone never fired).
type SyncMultiRoundTripper interface {
	WriteAndPollMulti(fd int, data []byte, rbuf []byte, onRecv func([]byte), isDone func() bool, beforeRearm func()) (ok bool, err error)
}

// WriteAndPollMulti implements SyncMultiRoundTripper. It writes data, then
// polls in a loop until isDone returns true or the cumulative poll timeout
// (20ms) expires. Designed for pipeline workloads where many RESP frames
// arrive across multiple read(2) calls.
func (w *worker) WriteAndPollMulti(fd int, data []byte, rbuf []byte, onRecv func([]byte), isDone func() bool, beforeRearm func()) (bool, error) {
	if w.closed.Load() {
		return false, ErrLoopClosed
	}
	w.mu.RLock()
	c, ok := w.conns[fd]
	epfd := w.epollFD
	w.mu.RUnlock()
	if !ok {
		return false, engine.ErrUnknownFD
	}

	// Step 1: Write.
	c.mu.Lock()
	if c.closing || c.closed {
		c.mu.Unlock()
		return false, engine.ErrUnknownFD
	}
	if len(c.writeBuf)-c.writePos+len(data) > maxPendingBytes {
		c.mu.Unlock()
		return false, engine.ErrQueueFull
	}
	c.writeBuf = append(c.writeBuf, data...)
	if c.sending {
		c.mu.Unlock()
		return false, nil
	}
	c.sending = true
	werr := w.flushLocked(c)
	c.sending = false
	pending := c.writePos < len(c.writeBuf)
	c.mu.Unlock()
	if werr != nil {
		w.errorClose(fd, werr)
		return false, werr
	}
	if pending {
		w.enqueueFlush(fd)
	}

	// Step 2: Mask EPOLLIN.
	c.recvMu.Lock()
	evMask := uint32(unix.EPOLLET | unix.EPOLLRDHUP)
	if c.epollOut {
		evMask |= unix.EPOLLOUT
	}
	_ = unix.EpollCtl(epfd, unix.EPOLL_CTL_MOD, fd, &unix.EpollEvent{
		Events: evMask,
		Fd:     int32(fd),
	})

	// Step 3: Read loop until isDone or cumulative timeout.
	//
	// Pipeline responses arrive as Redis processes commands sequentially.
	// Strategy: try one non-blocking read (catches data already in the
	// kernel buffer from TCP coalescing), then block on poll(1ms) until
	// data arrives. After each read batch, drain to EAGAIN and check
	// isDone. This replaces the prior 3-phase spin (64 reads + 64
	// poll(0)/Gosched + 50 poll(1ms)) with a tight read-poll-drain loop
	// that uses ~3-6 syscalls instead of ~128+ for typical pipeline sizes.
	gotData := false
	var readErr error
	var pfd [1]unix.PollFd
	pfd[0].Fd = int32(fd)
	pfd[0].Events = unix.POLLIN

	// Initial read: catches responses that arrived during or before the
	// write syscall (TCP coalescing, loopback fast path).
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
	// check isDone, repeat. Budget: 50 rounds x 1ms = 50ms max. For
	// Pipeline100 on localhost this typically completes in 1-2 rounds.
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
		if perr != nil && perr != syscall.EINTR {
			break
		}
		// poll timeout — no data arrived within 1ms.
		if gotData {
			// Already have partial data; keep waiting.
			continue
		}
		break
	}

done:
	// Final drain to EAGAIN before re-arming.
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

	// Allow the driver to transition dispatch state while recvMu is held and
	// EPOLLIN is still masked. This guarantees the event loop cannot see a
	// half-transitioned state when it resumes reads.
	if beforeRearm != nil {
		beforeRearm()
	}

	// Re-enable EPOLLIN.
	evMask = unix.EPOLLIN | unix.EPOLLET | unix.EPOLLRDHUP
	if c.epollOut {
		evMask |= unix.EPOLLOUT
	}
	_ = unix.EpollCtl(epfd, unix.EPOLL_CTL_MOD, fd, &unix.EpollEvent{
		Events: evMask,
		Fd:     int32(fd),
	})
	c.recvMu.Unlock()

	if readErr != nil {
		w.errorClose(fd, readErr)
		return false, readErr
	}
	if gotData && isDone() {
		return true, nil
	}
	if !gotData {
		return false, nil
	}
	// Got some data but isDone never fired — partial pipeline. Caller falls
	// back to async wait for the remaining responses.
	return false, nil
}

func (w *worker) run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		n, err := unix.EpollWait(w.epollFD, w.events, 100)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			return
		}
		for i := 0; i < n; i++ {
			ev := w.events[i]
			fd := int(ev.Fd)
			if fd == w.eventFD {
				var sink [8]byte
				for {
					if _, rerr := unix.Read(w.eventFD, sink[:]); rerr != nil {
						break
					}
				}
				continue
			}
			if ev.Events&(unix.EPOLLIN|unix.EPOLLRDHUP|unix.EPOLLHUP|unix.EPOLLERR) != 0 {
				w.handleReadable(fd, ev.Events)
			}
			if ev.Events&unix.EPOLLOUT != 0 {
				w.handleWritable(fd)
			}
		}
		w.drainPending()
	}
}

// handleReadable drains readable data from fd and invokes onRecv. A zero-byte
// read (EOF) or an error triggers the close path with the matching error.
func (w *worker) handleReadable(fd int, events uint32) {
	w.mu.RLock()
	c, ok := w.conns[fd]
	w.mu.RUnlock()
	if !ok {
		return
	}
	// Serialize with WriteAndPoll's caller-side reads so the protocol
	// state machine (driven by onRecv) is never entered concurrently.
	c.recvMu.Lock()
	defer c.recvMu.Unlock()
	// Edge-triggered epoll delivers exactly one edge per "not readable" →
	// "readable" transition. We MUST drain to EAGAIN; stopping early (e.g.
	// on a short read) leaves bytes in the kernel buffer and, crucially,
	// no further edge will fire until those bytes are first consumed AND
	// new data arrives — a silent stall under pipelined traffic.
	for {
		n, err := unix.Read(fd, w.rbuf)
		if n > 0 && c.onRecv != nil {
			c.onRecv(w.rbuf[:n])
		}
		if err != nil {
			if isEAGAIN(err) {
				break
			}
			w.errorClose(fd, err)
			return
		}
		if n == 0 {
			// Peer performed orderly shutdown.
			w.errorClose(fd, nil)
			return
		}
	}
	if events&(unix.EPOLLRDHUP|unix.EPOLLHUP|unix.EPOLLERR) != 0 {
		w.errorClose(fd, nil)
	}
}

// handleWritable retries a pending flush when the socket reports writable.
func (w *worker) handleWritable(fd int) {
	w.drainOne(fd)
}

// drainPending processes fds queued by Write after an EAGAIN or by
// enqueueFlush. A fresh backing array is installed under pendingMu so
// concurrent enqueueFlush appends don't race with the worker's iteration
// over the snapshot.
func (w *worker) drainPending() {
	w.pendingMu.Lock()
	list := w.pending
	w.pending = nil
	w.pendingMu.Unlock()
	for _, fd := range list {
		w.drainOne(fd)
	}
}

// drainOne flushes pending bytes for a single fd under c.mu.
func (w *worker) drainOne(fd int) {
	w.mu.RLock()
	c, ok := w.conns[fd]
	w.mu.RUnlock()
	if !ok {
		return
	}
	c.mu.Lock()
	if c.closing || c.closed {
		c.mu.Unlock()
		return
	}
	if c.sending {
		c.mu.Unlock()
		return
	}
	c.sending = true
	err := w.flushLocked(c)
	c.sending = false
	c.mu.Unlock()
	if err != nil {
		w.errorClose(fd, err)
	}
}
