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
// # Cost on the loop thread
//
// Only producers take the mutex. FD, which the epoll event loop calls for
// every event it dispatches, is a single relaxed atomic load of a field the
// mutex never shares a cache line with — so the loop thread neither locks nor
// touches a line the producers dirty. The number is written exactly twice in
// a loop's life (Set at startup, Close at shutdown), both on the loop thread,
// so that line stays clean in every core's cache. See wakefd_bench_test.go
// for the measured difference against a mutex-guarded FD.
//
// A nil *WakeFD is a valid, permanently disabled handle: every method is a
// no-op and FD reports -1 — what a loop whose eventfd could not be created,
// and every test fixture that does not want one, needs.
package wakefd

import (
	"sync"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// cacheLine is the padding used to keep the lock-free descriptor number off
// the mutex's cache line. 64 bytes on every architecture celeris runs on
// (amd64, arm64); over-padding costs one struct per event loop.
const cacheLine = 64

// WakeFD is a wakeup eventfd shared between one event loop and the producers
// that wake it. The loop owns Set, FD and Close, all of which run on its own
// thread; any goroutine may call Signal.
type WakeFD struct {
	// num mirrors fd for FD's lock-free read. Written only by Set and
	// Close, under mu, on the loop thread. Kept first and padded so the
	// producers' RLock in Signal never dirties the line the loop thread
	// loads on its hottest path.
	num atomic.Int32
	_   [cacheLine - 4]byte

	mu     sync.RWMutex
	fd     int
	closed bool
}

// nonblocking forces O_NONBLOCK on fd.
//
// Signal holds the read lock across its write(2), and Close waits behind the
// signals already in flight, so a descriptor whose write can block would
// stall a loop's shutdown for as long as the peer takes to drain. Every
// caller today creates its descriptor with EFD_NONBLOCK, and this keeps that
// a property of the type rather than a convention callers must remember:
// hand WakeFD a blocking pipe and it is made non-blocking here, so the read
// lock is held for a bounded time no matter what the caller passed.
func nonblocking(fd int) {
	if fd < 0 {
		return
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil || flags&unix.O_NONBLOCK != 0 {
		return
	}
	_, _ = unix.FcntlInt(uintptr(fd), unix.F_SETFL, flags|unix.O_NONBLOCK)
}

// New returns a handle for fd, which may be -1 when no eventfd could be
// created. A loop allocates its handle before it starts, so producers can
// capture the handle before the descriptor itself exists.
//
// fd is made non-blocking: see nonblocking. WakeFD takes ownership — Close
// closes it.
func New(fd int) *WakeFD {
	w := &WakeFD{fd: fd}
	nonblocking(fd)
	w.num.Store(int32(fd))
	return w
}

// Signal wakes the loop, unless the handle is closed or holds no descriptor.
// Safe on any goroutine, and on a nil handle.
//
// The write is a single write(2) on a descriptor New and Set have made
// non-blocking, so the read lock is held for a bounded time and Close is
// never starved.
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
//
// fd is made non-blocking: see nonblocking. On success WakeFD takes
// ownership.
func (w *WakeFD) Set(fd int) bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return false
	}
	nonblocking(fd)
	w.fd = fd
	w.num.Store(int32(fd))
	return true
}

// FD returns the descriptor, or -1 when there is none. For the loop thread,
// the only goroutine allowed to use the number directly (arming the poll,
// draining the counter); producers go through Signal.
//
// Lock-free by design: the epoll loop calls this for every event it
// dispatches, ahead of every other branch.
func (w *WakeFD) FD() int {
	if w == nil {
		return -1
	}
	return int(w.num.Load())
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
		// Retire the number before the close, so a loop-thread FD() can
		// never hand out a descriptor that is already free to be reused.
		w.num.Store(-1)
		if w.fd >= 0 {
			_ = unix.Close(w.fd)
		}
		w.fd = -1
	}
	w.mu.Unlock()
}
