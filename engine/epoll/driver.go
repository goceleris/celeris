//go:build linux

package epoll

import (
	"errors"
	"fmt"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
)

// driverWriteCap bounds the pending outbound bytes per driver FD before
// Write returns engine.ErrQueueFull. Matches the detached-conn ceiling so
// long-lived driver sessions (postgres, redis) can amortize larger bursts.
const driverWriteCap = 64 << 20 // 64 MiB

// driverReadBufSize is the scratch buffer the worker uses to drain data
// from a driver FD on EPOLLIN. The buffer is reused across FDs since the
// onRecv callback runs synchronously on the worker goroutine and copies
// out anything it wants to retain.
const driverReadBufSize = 32 << 10 // 32 KiB

// driverConn holds per-FD state for a driver-registered connection.
//
// Callbacks onRecv and onClose execute on the worker's goroutine (the same
// goroutine that runs the epoll_wait loop); they must not block and must
// not call back into [Loop.RegisterConn] / [Loop.UnregisterConn] /
// [Loop.Write] for the same FD.
type driverConn struct {
	fd      int
	onRecv  func([]byte)
	onClose func(error)

	mu       sync.Mutex
	writeBuf []byte // pending bytes enqueued by Write; drained by flushDriverSend
	sendPos  int    // offset into writeBuf; mirrors flushWrites' writePos pattern
	sending  bool   // at most one write(2) in flight per FD (invariant)
	closed   bool   // true once onClose has been dispatched; guards double-close
	epollOut bool   // true while EPOLLOUT is armed so ResetWrite can disarm it
}

// lookupDriver returns the driverConn for fd, or nil if none is registered.
// Safe to call from the worker goroutine during dispatch.
func (l *Loop) lookupDriver(fd int) *driverConn {
	l.driverMu.RLock()
	dc := l.driverConns[fd]
	l.driverMu.RUnlock()
	return dc
}

// RegisterConn adds fd to this worker's epoll interest set and installs the
// data and close callbacks. fd must already be in non-blocking mode; the
// caller retains ownership of the fd (UnregisterConn does not close it).
//
// Returns an error if fd is already registered as an HTTP conn on this
// worker or already registered as a driver conn.
func (l *Loop) RegisterConn(fd int, onRecv func([]byte), onClose func(error)) error {
	if fd < 0 {
		return fmt.Errorf("celeris/epoll: invalid fd %d", fd)
	}
	if onRecv == nil || onClose == nil {
		return errors.New("celeris/epoll: onRecv and onClose must be non-nil")
	}

	l.driverMu.Lock()
	defer l.driverMu.Unlock()

	// HTTP accept path writes l.conns[fd] on the worker goroutine; a concurrent
	// driver registration for the same fd would race. Re-check under the lock
	// to close the TOCTOU window — a racy accept could have run between an
	// earlier unlocked check and this lock acquisition.
	if fd < len(l.conns) && l.conns[fd] != nil {
		return fmt.Errorf("celeris/epoll: fd %d is already an HTTP connection", fd)
	}

	if l.driverConns == nil {
		l.driverConns = make(map[int]*driverConn)
	}
	if _, exists := l.driverConns[fd]; exists {
		return fmt.Errorf("celeris/epoll: fd %d is already registered", fd)
	}

	dc := &driverConn{
		fd:      fd,
		onRecv:  onRecv,
		onClose: onClose,
	}
	// Level-triggered EPOLLOUT is added lazily (on EAGAIN) via armEpollOut
	// so idle driver conns don't wake the loop on every send-buffer drain.
	if err := unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_ADD, fd, &unix.EpollEvent{
		Events: unix.EPOLLIN | unix.EPOLLET | unix.EPOLLRDHUP,
		Fd:     int32(fd),
	}); err != nil {
		return fmt.Errorf("celeris/epoll: epoll_ctl ADD fd %d: %w", fd, err)
	}

	l.driverConns[fd] = dc
	l.hasDriverConns.Store(true)
	return nil
}

// UnregisterConn removes fd from this worker's interest set and schedules
// the onClose callback (with a nil error) to fire on the next worker
// iteration. The fd itself is NOT closed — the driver owns its lifetime.
func (l *Loop) UnregisterConn(fd int) error {
	l.driverMu.Lock()
	dc, ok := l.driverConns[fd]
	if !ok {
		l.driverMu.Unlock()
		return engine.ErrUnknownFD
	}
	delete(l.driverConns, fd)
	if len(l.driverConns) == 0 {
		l.hasDriverConns.Store(false)
	}
	l.driverMu.Unlock()

	_ = unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_DEL, fd, nil)

	// Synchronous close: the caller is typically the driver's own goroutine
	// (not the worker). A deferred dispatch via eventfd would require the
	// worker to own a scheduled-callback queue; for now, we invoke onClose
	// on the caller's goroutine after marking the conn closed so the worker
	// sees a no-op if it concurrently observes an event for this fd.
	dc.mu.Lock()
	already := dc.closed
	dc.closed = true
	cb := dc.onClose
	dc.mu.Unlock()

	if !already && cb != nil {
		cb(nil)
	}
	return nil
}

// Write enqueues data for transmission on fd. Returns engine.ErrUnknownFD
// if fd is not registered on this worker, or engine.ErrQueueFull if the
// pending backlog would exceed driverWriteCap.
//
// The call is non-blocking: it performs at most one write(2) inline when
// the socket is idle and otherwise arms EPOLLOUT so the worker flushes the
// remainder on the next iteration.
func (l *Loop) Write(fd int, data []byte) error {
	dc := l.lookupDriver(fd)
	if dc == nil {
		return engine.ErrUnknownFD
	}

	dc.mu.Lock()
	if dc.closed {
		dc.mu.Unlock()
		return engine.ErrUnknownFD
	}
	pending := len(dc.writeBuf) - dc.sendPos
	if pending+len(data) > driverWriteCap {
		dc.mu.Unlock()
		return engine.ErrQueueFull
	}
	dc.writeBuf = append(dc.writeBuf, data...)

	// If another caller already holds the "sending" slot, piggy-back: that
	// caller's flushDriverSend will drain the newly-appended bytes before
	// returning. This preserves the one-write-in-flight-per-FD invariant.
	if dc.sending {
		dc.mu.Unlock()
		return nil
	}
	dc.sending = true
	err := l.flushDriverSendLocked(dc)
	dc.sending = false
	dc.mu.Unlock()
	if err != nil {
		// Non-EAGAIN write error: tear down the conn so the driver sees
		// onClose. Without this, the conn stays "registered" forever and
		// future Writes return the same error indefinitely.
		l.closeDriver(dc, err)
	}
	return err
}

// flushDriverSendLocked drains dc.writeBuf to the kernel. Caller MUST hold
// dc.mu and MUST have set dc.sending = true before entry. On EAGAIN we
// arm EPOLLOUT (level-triggered) so the worker resumes on the next kernel
// buffer-available signal.
func (l *Loop) flushDriverSendLocked(dc *driverConn) error {
	for dc.sendPos < len(dc.writeBuf) {
		n, err := unix.Write(dc.fd, dc.writeBuf[dc.sendPos:])
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
				if !dc.epollOut {
					// EPOLLIN stays edge-triggered (ET) but EPOLLOUT is
					// level-triggered: it fires as long as the socket is
					// writable, so there is no risk of a missed wakeup
					// after sending clears. EPOLLET only applies to EPOLLIN.
					modErr := unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_MOD, dc.fd, &unix.EpollEvent{
						Events: unix.EPOLLIN | unix.EPOLLOUT | unix.EPOLLRDHUP,
						Fd:     int32(dc.fd),
					})
					if modErr == nil {
						dc.epollOut = true
					}
				}
				return nil
			}
			// Non-EAGAIN error: the write failed and the caller alone can't
			// recover — the conn is still registered but no further I/O
			// will succeed. Clear pending data and return the error; the
			// caller path invokes closeDriver to fire onClose and unregister.
			dc.writeBuf = dc.writeBuf[:0]
			dc.sendPos = 0
			return err
		}
		dc.sendPos += n
	}
	// Fully flushed: reset and disarm EPOLLOUT so idle FDs don't wake.
	dc.writeBuf = dc.writeBuf[:0]
	dc.sendPos = 0
	if dc.epollOut {
		_ = unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_MOD, dc.fd, &unix.EpollEvent{
			Events: unix.EPOLLIN | unix.EPOLLET | unix.EPOLLRDHUP,
			Fd:     int32(dc.fd),
		})
		dc.epollOut = false
	}
	return nil
}

// CPUID returns the CPU the worker is pinned to, or -1 if the worker was
// not successfully pinned (platform.PinToCPU best-effort).
func (l *Loop) CPUID() int {
	return l.cpuID
}

// handleDriverEvent dispatches an epoll event to a registered driver conn.
// Runs on the worker goroutine. EPOLLIN reads are drained eagerly (ET);
// EPOLLOUT resumes a pending Write. On error/hangup the onClose callback
// is invoked and the conn is removed from the interest set.
func (l *Loop) handleDriverEvent(dc *driverConn, events uint32) {
	// Read first: a peer may send data and immediately close, producing
	// both EPOLLIN and EPOLLRDHUP in one event batch.
	if events&unix.EPOLLIN != 0 {
		l.driverRead(dc)
	}
	if events&unix.EPOLLOUT != 0 {
		dc.mu.Lock()
		if !dc.sending && !dc.closed {
			dc.sending = true
			if err := l.flushDriverSendLocked(dc); err != nil {
				dc.sending = false
				dc.mu.Unlock()
				l.closeDriver(dc, err)
				return
			}
			dc.sending = false
		}
		dc.mu.Unlock()
	}
	if events&(unix.EPOLLERR|unix.EPOLLHUP|unix.EPOLLRDHUP) != 0 {
		l.closeDriver(dc, nil)
	}
}

// driverRead drains fd with edge-triggered reads into the per-loop scratch
// buffer and fans each chunk out to onRecv. The callback runs on this
// goroutine; the slice is invalidated on return.
func (l *Loop) driverRead(dc *driverConn) {
	if l.driverReadBuf == nil {
		l.driverReadBuf = make([]byte, driverReadBufSize)
	}
	for {
		n, err := unix.Read(dc.fd, l.driverReadBuf)
		if err != nil {
			if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
				return
			}
			l.closeDriver(dc, err)
			return
		}
		if n == 0 {
			l.closeDriver(dc, nil)
			return
		}
		dc.mu.Lock()
		if dc.closed {
			dc.mu.Unlock()
			return
		}
		cb := dc.onRecv
		dc.mu.Unlock()
		if cb != nil {
			cb(l.driverReadBuf[:n])
		}
		if n < len(l.driverReadBuf) {
			// Short read implies the socket is drained; ET epoll will
			// notify on the next arrival. Skip the EAGAIN-producing read.
			return
		}
	}
}

// closeDriver removes dc from the worker's interest set and invokes
// onClose exactly once. Safe to call concurrently; the dc.closed flag
// serializes the close path.
func (l *Loop) closeDriver(dc *driverConn, cause error) {
	dc.mu.Lock()
	if dc.closed {
		dc.mu.Unlock()
		return
	}
	dc.closed = true
	cb := dc.onClose
	dc.mu.Unlock()

	l.driverMu.Lock()
	if _, ok := l.driverConns[dc.fd]; ok {
		delete(l.driverConns, dc.fd)
		if len(l.driverConns) == 0 {
			l.hasDriverConns.Store(false)
		}
	}
	l.driverMu.Unlock()

	_ = unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_DEL, dc.fd, nil)

	if cb != nil {
		cb(cause)
	}
}

// shutdownDrivers closes every registered driver conn during Loop.shutdown.
// Invoked from the worker goroutine; onClose callbacks fire synchronously.
func (l *Loop) shutdownDrivers() {
	l.driverMu.Lock()
	if len(l.driverConns) == 0 {
		l.driverMu.Unlock()
		return
	}
	conns := make([]*driverConn, 0, len(l.driverConns))
	for _, dc := range l.driverConns {
		conns = append(conns, dc)
	}
	l.driverConns = nil
	l.hasDriverConns.Store(false)
	l.driverMu.Unlock()

	for _, dc := range conns {
		dc.mu.Lock()
		if dc.closed {
			dc.mu.Unlock()
			continue
		}
		dc.closed = true
		cb := dc.onClose
		dc.mu.Unlock()
		_ = unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_DEL, dc.fd, nil)
		if cb != nil {
			cb(nil)
		}
	}
}

// NumWorkers returns the number of worker event loops. Required by
// [engine.EventLoopProvider].
func (e *Engine) NumWorkers() int {
	e.mu.Lock()
	n := len(e.loops)
	e.mu.Unlock()
	return n
}

// WorkerLoop returns the worker loop at index n. Panics if n is out of
// range or if the engine has not yet started listening.
func (e *Engine) WorkerLoop(n int) engine.WorkerLoop {
	e.mu.Lock()
	defer e.mu.Unlock()
	if n < 0 || n >= len(e.loops) {
		panic(fmt.Sprintf("celeris/epoll: WorkerLoop index %d out of range [0, %d)", n, len(e.loops)))
	}
	return e.loops[n]
}

var (
	_ engine.EventLoopProvider = (*Engine)(nil)
	_ engine.WorkerLoop        = (*Loop)(nil)
)
