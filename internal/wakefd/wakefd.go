// Package wakefd owns an event loop's wakeup eventfd.
//
// The engines let producers on other goroutines wake a loop by writing its
// eventfd: a driver registering a connection, a transplant handing over a
// descriptor, a detached WebSocket applying backpressure, an H2 handler
// queueing a response frame. Shutdown closes that descriptor. Before
// celeris#655 the number stayed in the loop's field, so a producer that ran
// after the close wrote 8 bytes into whatever the process opened next — a
// spurious wakeup on another eventfd, bytes injected into a client socket,
// silent corruption of a file. Nothing reported any of it.
//
// WakeFD closes that window by owning the descriptor instead of publishing
// its number: Signal and Close take the same lock, so a signal either happens
// entirely before the close or does not happen at all. It generalises the
// rule celeris#658 applied to the two adopt queues — every write happens
// under a lock shutdown takes before it closes the fd — to every producer,
// including the H2 write queue, which has no engine lock of its own.
//
// The mutex is a LEAF: nothing is acquired while it is held, so Signal is
// safe to call under a queue mutex (epoll AdoptConn holds adoptQMu, io_uring
// addAdoptAction holds driverActionMu). Never call it under wakeMu.
//
// A nil *WakeFD is a valid, permanently disabled handle: every method is a
// no-op and FD reports -1 — what a loop whose eventfd could not be created,
// and every test fixture that does not want one, needs.
package wakefd

import (
	"sync"

	"golang.org/x/sys/unix"
)

// WakeFD is a wakeup eventfd shared between one event loop and the producers
// that wake it. The loop owns Set and Close, both of which run on its own
// thread; any goroutine may call Signal.
type WakeFD struct {
	mu     sync.RWMutex
	fd     int
	closed bool
}

// New returns a handle for fd, which may be -1 when no eventfd could be
// created. A loop allocates its handle before it starts, so producers can
// capture the handle before the descriptor itself exists.
func New(fd int) *WakeFD { return &WakeFD{fd: fd} }

// Signal wakes the loop, unless the handle is closed or holds no descriptor.
// Safe on any goroutine, and on a nil handle. The write is a single
// non-blocking write(2) on an EFD_NONBLOCK eventfd, so the read lock is held
// for a bounded time and Close is never starved.
func (w *WakeFD) Signal() {
	if w == nil {
		return
	}
	w.mu.RLock()
	if !w.closed && w.fd >= 0 {
		var val [8]byte
		val[0] = 1
		_, _ = unix.Write(w.fd, val[:])
	}
	w.mu.RUnlock()
}

// Set installs fd, for a loop that creates its eventfd lazily. It reports
// false once the handle is closed, and then the caller still owns fd and must
// close it — the loop is gone, so nothing else ever would.
func (w *WakeFD) Set(fd int) bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return false
	}
	w.fd = fd
	return true
}

// FD returns the descriptor, or -1 when there is none. For the loop thread,
// the only goroutine allowed to use the number directly (arming the poll,
// draining the counter); producers go through Signal.
func (w *WakeFD) FD() int {
	if w == nil {
		return -1
	}
	w.mu.RLock()
	fd := w.fd
	w.mu.RUnlock()
	return fd
}

// Close closes the descriptor and turns every later Signal into a no-op.
// Called by the loop at shutdown. It blocks until the signals already in
// flight have finished, which is what keeps their write(2) off a descriptor
// number that is about to be recycled. Idempotent.
func (w *WakeFD) Close() {
	if w == nil {
		return
	}
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		if w.fd >= 0 {
			_ = unix.Close(w.fd)
		}
		w.fd = -1
	}
	w.mu.Unlock()
}
