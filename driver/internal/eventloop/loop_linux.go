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
	"github.com/goceleris/celeris/internal/wakefd"
)

// isEAGAIN is a fast check for EAGAIN/EWOULDBLOCK that avoids the reflection
// overhead of errors.Is. On Linux EAGAIN == EWOULDBLOCK (both 11), so a
// single comparison suffices. unix.Read/unix.Write return syscall.Errno
// directly (not wrapped), so the type assertion always matches.
func isEAGAIN(err error) bool {
	return err == syscall.EAGAIN
}

// maxPendingBytes is the per-FD outbound buffer cap on the Linux worker.
// Writes beyond this return engine.ErrQueueFull. Chosen to match the H1/H2
// backpressure limit used by the HTTP epoll engine.
const maxPendingBytes = 4 << 20 // 4 MiB

// shutdownPartial closes any workers created before a failure in newLoop.
func (l *Loop) shutdownPartial() error {
	var first error
	for _, w := range l.workers {
		if err := w.shutdown(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// worker owns a single epoll instance and the FDs registered on it.
//
// Callers on other goroutines reach the worker's own two descriptors, and
// shutdown closes both, so neither number is used outside a lock shutdown
// takes before it closes it (celeris#862). The wakeup eventfd is written only
// through wakeFD, whose Signal and Close share a lock. epollFD is used under
// w.mu, under the conn's c.mu after a check of c.closed (shutdown marks every
// registered conn closed, under its c.mu, before it closes epollFD under
// w.mu), or by the worker goroutine, which Loop.Close joins before shutdown.
type worker struct {
	id      int
	epollFD int
	// wakeFD owns the wakeup eventfd. Producers (enqueueFlush, Loop.Close)
	// call Signal, never write(2) the number, so a producer that runs after
	// shutdown has closed the eventfd writes nothing: it cannot reach a
	// socket or a file that has taken the number (celeris#862, the driver
	// loop's twin of celeris#655).
	wakeFD *wakefd.WakeFD

	mu    sync.RWMutex
	conns map[int]*driverConn // fd -> state (protected by mu)
	// gen is the generation of the last registration on this worker
	// (protected by mu). Each RegisterConn takes the next one, never 0.
	gen uint32

	// pending holds the conns whose outbound buffers have fresh bytes.
	// Writers append, the worker goroutine drains. Access guarded by
	// pendingMu. It holds conns, not fd numbers, so a flush queued for a
	// conn that has gone can never act on a conn that took its number.
	pendingMu sync.Mutex
	pending   []*driverConn

	closed atomic.Bool
	events []unix.EpollEvent
	rbuf   []byte
}

// driverConn holds per-FD state. writeBuf/writePos mirror the HTTP epoll
// path but a per-FD mutex replaces the implicit event-loop serialization:
// drivers may call Write from any goroutine and the worker goroutine
// simultaneously drains the buffer, so both sides coordinate through mu.
//
// Once closed is set, no read(2), write(2) or epoll_ctl is issued on fd's
// number again: the owner may have closed it, and the number may name
// another file (celeris#784). Every read is issued under rmu, and every write
// and EPOLL_CTL_MOD under mu, each after a check of closed in the same
// critical section. Every teardown sets closed with both locks held
// (markClosed) before it removes the conn from the worker.
//
// Reads have a lock of their own so that a read never waits for a flush:
// flushLocked holds mu across its whole write(2) loop, and the worker
// goroutine, which reads for every conn on it, must not wait for one conn's
// writer.
type driverConn struct {
	fd int
	// gen is the generation of this registration of fd, unique on the
	// worker. Every epoll_event of the registration carries it in Pad (the
	// ADD and every MOD; see epollEvent), and the worker dispatches an event
	// only to the conn whose gen it carries: an event the worker collected
	// for a conn that has since been unregistered must not reach the conn
	// that took its number (celeris#842). Set before the conn is published,
	// then read-only.
	gen     uint32
	onRecv  func([]byte)
	onClose func(error)

	mu       sync.Mutex // guards writeBuf, writePos, sending, closing, epollOut; held across each write(2) and EPOLL_CTL_MOD of fd
	writeBuf []byte
	writePos int
	sending  bool // true while a goroutine is draining writeBuf
	closing  bool // UnregisterConn or error path requested teardown
	closed   bool // torn down; set with mu and rmu held, read under either; no further read, write or epoll_ctl of fd; onClose fires once, after it is set
	epollOut bool // EPOLLOUT currently armed on this fd

	// rmu is held across each read(2) of fd, with the closed check before
	// it (readOpen). Writers never take it.
	rmu sync.Mutex

	// recvMu serializes onRecv calls between the event-loop worker
	// (handleReadable) and WriteAndPoll (caller goroutine). Without this,
	// an in-flight handleReadable from a prior epoll_wait batch can race
	// with WriteAndPoll's caller-side reads. Lock order: recvMu, then w.mu,
	// then mu, then rmu; no path takes them in another order. The teardown
	// paths never take recvMu: they run inside onRecv/onClose callbacks,
	// which hold it.
	recvMu sync.Mutex
}

// testHookBeforeRead, when non-nil, runs inside readOpen's critical section,
// after the closed check and before the read, with c.rmu held. Tests only
// (celeris#784): a test sets it before it creates the worker and clears it
// after the worker is shut down.
var testHookBeforeRead func(fd int)

// testHookBeforeAdd, when non-nil, runs in RegisterConn once the conn is in
// the worker's map and before the critical section that issues its
// EPOLL_CTL_ADD, with no lock held: where a shutdown can run in between.
// Tests only (celeris#862): a test sets it before it creates the worker and
// clears it after the worker is shut down.
var testHookBeforeAdd func(fd int)

// testHookDroppedEvent, when non-nil, runs on the worker goroutine for each
// event it drops because the event's registration has ended. Tests only
// (celeris#842): set before the worker is created, cleared after Close.
var testHookDroppedEvent func(fd int, events uint32)

// readOpen reads c.fd into buf unless c has been torn down, in which case it
// returns engine.ErrUnknownFD without reading. The closed check and the read
// are one critical section under c.rmu, a lock every teardown takes to set
// c.closed (UnregisterConn does so before it returns), so a read is never
// issued on fd's number once the owner may have closed it (celeris#784). A
// teardown that asks for c.rmu while a read is in flight waits for that one
// read(2), not for onRecv: the bytes it returned were c's, and the caller
// still hands them to onRecv. A flush of c, which holds c.mu, does not delay
// the read.
func readOpen(c *driverConn, buf []byte) (int, error) {
	c.rmu.Lock()
	if c.closed {
		c.rmu.Unlock()
		return 0, engine.ErrUnknownFD
	}
	if h := testHookBeforeRead; h != nil {
		h(c.fd)
	}
	n, err := unix.Read(c.fd, buf)
	c.rmu.Unlock()
	return n, err
}

// markClosed sets c.closed and c.closing with c.mu and c.rmu held and
// reports whether c was already closed. It waits for a flush, an epoll_ctl
// and a read of fd already in flight; none is issued after it returns.
func (c *driverConn) markClosed() (already bool) {
	c.mu.Lock()
	c.rmu.Lock()
	already = c.closed
	c.closed = true
	c.closing = true
	c.rmu.Unlock()
	c.mu.Unlock()
	return already
}

// epollEvent returns the epoll_event for c's registration with the given
// interest: the number in Fd, and the registration's generation in Pad,
// which the kernel hands back with each event it reports for fd. Every
// EPOLL_CTL_ADD and EPOLL_CTL_MOD of a conn uses it: a MOD replaces the whole
// event data, and one without the generation would make the worker drop
// every later event of the conn (celeris#842).
func (c *driverConn) epollEvent(events uint32) unix.EpollEvent {
	return unix.EpollEvent{Events: events, Fd: int32(c.fd), Pad: int32(c.gen)}
}

// setEvents sets c's epoll interest to EPOLLET|EPOLLRDHUP, plus EPOLLIN when
// in is set and EPOLLOUT while it is armed, unless c has been torn down. The
// EPOLL_CTL_MOD is issued by number, so it goes under c.mu after the closed
// check, like a write: once closed is set the number may name a conn that
// registered it since, and a MOD would change that conn's events
// (celeris#784). shutdown marks every conn closed before it closes epfd, so a
// MOD issued here never reaches a closed or reused epoll descriptor either.
func (c *driverConn) setEvents(epfd int, in bool) {
	c.mu.Lock()
	if !c.closed {
		ev := uint32(unix.EPOLLET | unix.EPOLLRDHUP)
		if in {
			ev |= unix.EPOLLIN
		}
		if c.epollOut {
			ev |= unix.EPOLLOUT
		}
		e := c.epollEvent(ev)
		_ = unix.EpollCtl(epfd, unix.EPOLL_CTL_MOD, c.fd, &e)
	}
	c.mu.Unlock()
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
		wakeFD:  wakefd.New(efd),
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
	// observe the swap to -1 under the lock rather than a torn read. Each
	// conn is marked closed before it leaves the map, as in UnregisterConn:
	// a caller that then finds its fd unknown may close it at once.
	w.mu.Lock()
	conns := make([]*driverConn, 0, len(w.conns))
	fired := make([]bool, 0, len(w.conns))
	for _, c := range w.conns {
		conns = append(conns, c)
		fired = append(fired, c.markClosed())
	}
	w.conns = map[int]*driverConn{}
	var first error
	// Waits for a Signal already in flight, and makes every later one a
	// no-op: a Write that passed its w.closed check before Loop.Close began
	// can still reach enqueueFlush now (celeris#862).
	w.wakeFD.Close()
	if w.epollFD >= 0 {
		if err := unix.Close(w.epollFD); err != nil && first == nil {
			first = err
		}
		w.epollFD = -1
	}
	w.mu.Unlock()
	for i, c := range conns {
		if !fired[i] && c.onClose != nil {
			c.onClose(ErrLoopClosed)
		}
	}
	return first
}

// wake triggers the worker's epoll_wait to return via the eventfd. Safe on
// any goroutine at any time: once shutdown has closed the eventfd, it writes
// nothing (celeris#862).
func (w *worker) wake() {
	w.wakeFD.Signal()
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
	// The registration's generation, set before the conn is published
	// (celeris#842).
	w.gen++
	if w.gen == 0 { // 0 never names a registration
		w.gen = 1
	}
	c.gen = w.gen
	// In the map before the ADD: the worker looks conns up under w.mu, and an
	// edge it found no conn for would be lost.
	w.conns[fd] = c
	w.mu.Unlock()

	if h := testHookBeforeAdd; h != nil {
		h(fd)
	}
	// The EPOLL_CTL_ADD is issued under c.mu, after a check of c.closed, like
	// every other epoll_ctl of a conn (flushLocked, setEvents). c is in the
	// map, and a conn leaves the map only once it is marked closed. shutdown
	// marks every conn in the map closed, each under its c.mu, before it
	// closes epollFD: so it cannot close the epoll fd while this ADD is in
	// flight, and an ADD issued here never lands on the number of a closed
	// epoll descriptor, or on another epoll instance that has taken it
	// (celeris#862). An UnregisterConn of fd marks c closed before its
	// EPOLL_CTL_DEL, so the ADD comes before that DEL or not at all. w.mu is
	// not held, so the worker's lookups never wait for the syscall. If c was
	// torn down since it entered the map (by shutdown, or by an UnregisterConn
	// of fd racing this call), its onClose has fired: no ADD is issued, and
	// the registration is reported as made, the same as for a conn torn down
	// right after it.
	var err error
	c.mu.Lock()
	if !c.closed {
		ev := c.epollEvent(unix.EPOLLIN | unix.EPOLLET | unix.EPOLLRDHUP)
		err = unix.EpollCtl(w.epollFD, unix.EPOLL_CTL_ADD, fd, &ev)
		if err != nil {
			// Not registered: marked closed here, under c.mu, so no
			// teardown fires onClose for it, and the loop never uses fd's
			// number again (markClosed, inline: c.mu is held).
			c.rmu.Lock()
			c.closed, c.closing = true, true
			c.rmu.Unlock()
		}
	}
	c.mu.Unlock()
	if err != nil {
		w.mu.Lock()
		if cur, ok := w.conns[fd]; ok && cur == c {
			delete(w.conns, fd)
		}
		w.mu.Unlock()
		return fmt.Errorf("epoll_ctl add: %w", err)
	}
	return nil
}

// UnregisterConn satisfies [engine.WorkerLoop]. It removes fd from the epoll
// set and fires onClose(nil) if not already closed. The caller owns the fd
// and is responsible for closing it, and may close it as soon as
// UnregisterConn returns, whatever it returns: by then no read(2), write(2)
// or epoll_ctl of fd is in flight, none is issued afterwards, and fd is out
// of the epoll set. A worker inside the conn's read loop stops before its
// next read of fd, so a socket that takes the number at once keeps its bytes
// (celeris#784). One call on the number can still be in flight: a
// WriteAndPoll* call on the conn may poll(2) the number for readiness until
// its next read, which it then does not issue. poll(2) reads nothing and
// changes nothing.
//
// A read that completed before UnregisterConn took the conn's read lock read
// the conn's own bytes, and they are still delivered: onRecv can run once
// more, concurrently with or after onClose.
func (w *worker) UnregisterConn(fd int) error {
	w.mu.RLock()
	c, ok := w.conns[fd]
	w.mu.RUnlock()
	if !ok {
		// Unknown, or a teardown already removed it. A teardown marks the
		// conn closed before it removes it, and removes it from the map and
		// the epoll set under w.mu, which this lookup took: so fd is already
		// safe to close.
		return engine.ErrUnknownFD
	}
	fired := c.markClosed()
	w.forget(c)
	if !fired && c.onClose != nil {
		c.onClose(nil)
	}
	return nil
}

// forget removes c from the worker's map and its fd from the epoll set, if
// the map entry for c.fd is still c. Both happen under w.mu, so a caller
// whose lookup misses (UnregisterConn returning ErrUnknownFD) knows the
// EPOLL_CTL_DEL is done before it closes the fd, and the DEL can never land
// on a later conn that registered the same number. The caller has marked c
// closed (markClosed) first.
func (w *worker) forget(c *driverConn) {
	w.mu.Lock()
	if cur, ok := w.conns[c.fd]; ok && cur == c {
		delete(w.conns, c.fd)
		if w.epollFD >= 0 {
			_ = unix.EpollCtl(w.epollFD, unix.EPOLL_CTL_DEL, c.fd, nil)
		}
	}
	w.mu.Unlock()
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
		w.errorClose(c, err)
		return err
	}
	if pending {
		w.enqueueFlush(c)
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
					ev := c.epollEvent(unix.EPOLLIN | unix.EPOLLOUT | unix.EPOLLET | unix.EPOLLRDHUP)
					modErr := unix.EpollCtl(w.epollFD, unix.EPOLL_CTL_MOD, c.fd, &ev)
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
		ev := c.epollEvent(unix.EPOLLIN | unix.EPOLLET | unix.EPOLLRDHUP)
		_ = unix.EpollCtl(w.epollFD, unix.EPOLL_CTL_MOD, c.fd, &ev)
		c.epollOut = false
	}
	return nil
}

// enqueueFlush appends c to the worker's pending list and wakes the
// goroutine so it retries the write when the socket is writable.
func (w *worker) enqueueFlush(c *driverConn) {
	w.pendingMu.Lock()
	w.pending = append(w.pending, c)
	w.pendingMu.Unlock()
	w.wake()
}

// errorClose tears down c after a fatal I/O error, or after the peer's
// orderly shutdown (err nil). It acts on the conn, not on its fd number: by
// the time a reader gets here the conn may have been unregistered and the
// number registered again by another conn, which must be left alone
// (celeris#784).
func (w *worker) errorClose(c *driverConn, err error) {
	fired := c.markClosed()
	w.forget(c)
	if !fired && c.onClose != nil {
		c.onClose(err)
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
//   - The reads are serialized with the event loop by recvMu and the
//     EPOLLIN mask. Each read(2) is issued under c.rmu after a check that
//     the conn has not been torn down (readOpen), and no lock but recvMu is
//     held across onRecv. The mask and the re-arm are issued under c.mu
//     after the same check (setEvents). Once the conn is torn down (an
//     UnregisterConn on another goroutine, or an I/O error on the worker),
//     the call issues no further read(2) or epoll_ctl on the number: the
//     next read it would issue makes it return engine.ErrUnknownFD instead
//     (celeris#784).
//   - Edge-triggered epoll: we drain to EAGAIN inside WriteAndPoll, so no
//     stale edge is left. After EPOLLIN is re-armed, the next kernel-buffer
//     arrival fires a fresh edge.
type SyncRoundTripper interface {
	WriteAndPoll(fd int, data []byte, rbuf []byte, onRecv func([]byte)) (ok bool, err error)
}

// SyncBusyRoundTripper is the variant of SyncRoundTripper that skips the
// runtime.Gosched() yield in its poll spin. Intended for callers known to
// run on a runtime.LockOSThread'd goroutine (the celeris HTTP engine's
// io_uring / epoll worker), where every Gosched incurs a stoplockedm +
// startlockedm futex pair and collapses integrated throughput.
//
// Unlocked callers (standalone mini-loop + foreign HTTP) should use
// [SyncRoundTripper]. Locked callers (celeris HTTP engine host)
// must use WriteAndPollBusy. Drivers select the path at open time based
// on whether an engine was supplied — see the driver's WithEngine option.
type SyncBusyRoundTripper interface {
	WriteAndPollBusy(fd int, data []byte, rbuf []byte, onRecv func([]byte)) (ok bool, err error)
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
		w.errorClose(c, werr)
		return false, werr
	}
	if pending {
		w.enqueueFlush(c)
	}

	// Step 2: Take recvMu so any in-flight handleReadable (from a prior
	// epoll_wait batch) completes before we start reading. Then mask
	// EPOLLIN so the event-loop worker does not wake up for this fd
	// while we hold the lock. Without the mask epoll_wait still fires
	// (edge-triggered), the worker blocks on recvMu, and the block/
	// unblock cycle is more expensive than the EpollCtl syscall. A
	// TryLock-based alternative deadlocks: edge-triggered epoll
	// delivers each edge exactly once, and a skip consumes the edge
	// without draining, leaving the response stranded when the caller
	// is already parked on doneCh waiting for it. The conn can be torn
	// down while we wait for recvMu, and another conn can register its
	// number: setEvents then masks nothing, so the mask cannot land on
	// that conn (celeris#784).
	c.recvMu.Lock()
	c.setEvents(epfd, false)

	// Step 3: Poll for the response in three phases:
	//   A: one non-blocking read (catches pre-arrived data from TCP
	//      coalescing / NAPI tail-drain). A tight 64-read probe was tried
	//      and dropped — kernel-to-kernel loopback RTT is 20–50µs, so
	//      rounds 2+ were pure EAGAIN waste at ~20% CPU.
	//   B: 16 poll(0) + Gosched rounds — yields the P so other handlers
	//      on it make progress. Required for foreign-HTTP throughput
	//      under 256-way concurrency.
	//   C: poll(1ms) as last resort.
	gotData := false
	var readErr error
	if n, err := readOpen(c, rbuf); n > 0 {
		gotData = true
		onRecv(rbuf[:n])
		for {
			n2, err2 := readOpen(c, rbuf)
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
	} else if err != nil && !isEAGAIN(err) {
		readErr = err
	}
	// Phase B: poll(0) + Gosched spin. Locked callers (celeris HTTP
	// engine workers) must NOT take this path — Gosched forces
	// stoplockedm + startlockedm (a futex pair per call); those callers
	// route through WriteAndPollBusy instead.
	if !gotData && readErr == nil {
		var pfd [1]unix.PollFd
		pfd[0].Fd = int32(fd)
		pfd[0].Events = unix.POLLIN
		for range 16 {
			pfd[0].Revents = 0
			np, perr := unix.Poll(pfd[:], 0)
			if np > 0 && perr == nil && pfd[0].Revents&unix.POLLIN != 0 {
				for {
					n, err := readOpen(c, rbuf)
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
				n, err := readOpen(c, rbuf)
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
			n, err := readOpen(c, rbuf)
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
	// event-loop worker owns reads on this fd again. setEvents leaves a conn
	// torn down under us alone: its number may be another conn's by now.
	c.setEvents(epfd, true)
	c.recvMu.Unlock()

	if readErr != nil {
		w.errorClose(c, readErr)
		return false, readErr
	}
	if !gotData {
		return false, nil // EAGAIN — fall back to event-loop path
	}
	return true, nil
}

// WriteAndPollBusy implements [SyncBusyRoundTripper]. It mirrors WriteAndPoll
// but omits the runtime.Gosched() yield in its Phase B spin. Callers running
// on a runtime.LockOSThread'd goroutine (celeris HTTP engine workers) use
// this variant to avoid the stoplockedm+startlockedm futex storm that
// Gosched triggers on locked Ms — measured at 40%+ of CPU samples and
// responsible for a 2–3× throughput collapse on integrated HTTP+DB load.
//
// Contract matches WriteAndPoll (see that function's doc). Differs only in
// Phase B: instead of yielding between poll(0) calls, it does a tight 16-
// round poll(0) spin (~8µs) to catch responses that arrived just after
// Phase A finished, then falls through to poll(1ms). No scheduler hand-off
// during the spin — the locked M stays on-CPU.
func (w *worker) WriteAndPollBusy(fd int, data []byte, rbuf []byte, onRecv func([]byte)) (bool, error) {
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
		w.errorClose(c, werr)
		return false, werr
	}
	if pending {
		w.enqueueFlush(c)
	}

	// Step 2: Mask EPOLLIN under recvMu.
	c.recvMu.Lock()
	c.setEvents(epfd, false)

	// Step 3a — Phase A: 64 tight non-blocking reads. Unlike the
	// yielding WriteAndPoll variant (which drops Phase A to a single
	// read to free its P for other handlers), the busy variant runs on
	// a LockOSThread'd engine worker — its P is dedicated to one
	// handler at a time, so burning ~15µs of CPU to catch responses
	// that arrive mid-spin is a net win (avoids the Phase B/C latency
	// for the fastest responses). Profile shows this recovers ~15-20%
	// throughput vs a single-read Phase A on the celeris-engine path.
	const spinRounds = 64
	gotData := false
	var readErr error
	for range spinRounds {
		n, err := readOpen(c, rbuf)
		if n > 0 {
			gotData = true
			onRecv(rbuf[:n])
			for {
				n2, err2 := readOpen(c, rbuf)
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
	// Step 3b — Phase B (busy): 16 tight poll(0) without yielding. No
	// Gosched — on a locked M it would futex-storm; on an unlocked M
	// the caller is expected to be in the foreign-HTTP path instead.
	if !gotData && readErr == nil {
		var pfd [1]unix.PollFd
		pfd[0].Fd = int32(fd)
		pfd[0].Events = unix.POLLIN
		for range 16 {
			pfd[0].Revents = 0
			np, perr := unix.Poll(pfd[:], 0)
			if np > 0 && perr == nil && pfd[0].Revents&unix.POLLIN != 0 {
				for {
					n, err := readOpen(c, rbuf)
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
		}
	}
	// Step 3c — Phase C: blocking poll(1ms) as last resort. During this
	// syscall the Go runtime detaches P so other Gs on the same P run.
	if !gotData && readErr == nil {
		var pfd [1]unix.PollFd
		pfd[0].Fd = int32(fd)
		pfd[0].Events = unix.POLLIN
		np, perr := unix.Poll(pfd[:], 1)
		if np > 0 && perr == nil && pfd[0].Revents&unix.POLLIN != 0 {
			for {
				n, err := readOpen(c, rbuf)
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
			n, err := readOpen(c, rbuf)
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

	// Step 5: Re-enable EPOLLIN and release recvMu, unless the conn was torn
	// down under us (see WriteAndPoll).
	c.setEvents(epfd, true)
	c.recvMu.Unlock()

	if readErr != nil {
		w.errorClose(c, readErr)
		return false, readErr
	}
	if !gotData {
		return false, nil
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
		w.errorClose(c, werr)
		return false, werr
	}
	if pending {
		w.enqueueFlush(c)
	}

	// Step 2: Mask EPOLLIN.
	c.recvMu.Lock()
	c.setEvents(epfd, false)

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
		n, err := readOpen(c, rbuf)
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
				n, err := readOpen(c, rbuf)
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
			n, err := readOpen(c, rbuf)
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

	// Re-enable EPOLLIN, unless the conn was torn down under us (see
	// WriteAndPoll).
	c.setEvents(epfd, true)
	c.recvMu.Unlock()

	if readErr != nil {
		w.errorClose(c, readErr)
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
	// The eventfd stays open while run runs: shutdown closes it only after
	// Loop.Close has joined this goroutine.
	efd := w.wakeFD.FD()
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
			if int(ev.Fd) == efd {
				var sink [8]byte
				for {
					if _, rerr := unix.Read(efd, sink[:]); rerr != nil {
						break
					}
				}
				continue
			}
			w.dispatch(ev)
		}
		w.drainPending()
	}
}

// dispatch hands one epoll event of a driver conn to the conn it was
// collected for. The event names that conn by its number (Fd) and by the
// generation of its registration (Pad). An event the worker collected before
// the conn was unregistered or torn down is dispatched after it, and by then
// the number may be registered again, by the conn that took it: that conn has
// another generation, so the event is dropped instead of being applied to it
// (celeris#842). Read first, then flush: a peer may send and close at once.
func (w *worker) dispatch(ev unix.EpollEvent) {
	fd := int(ev.Fd)
	c := w.connFor(fd, uint32(ev.Pad))
	if c == nil {
		if h := testHookDroppedEvent; h != nil {
			h(fd, ev.Events)
		}
		return
	}
	if ev.Events&(unix.EPOLLIN|unix.EPOLLRDHUP|unix.EPOLLHUP|unix.EPOLLERR) != 0 {
		w.handleReadable(c, ev.Events)
	}
	if ev.Events&unix.EPOLLOUT != 0 {
		w.drainOne(c)
	}
}

// connFor returns the conn registered on fd if it is the registration gen
// names, or nil: the conn has gone, or another conn has registered the
// number since.
func (w *worker) connFor(fd int, gen uint32) *driverConn {
	w.mu.RLock()
	c, ok := w.conns[fd]
	w.mu.RUnlock()
	if !ok || c.gen != gen {
		return nil
	}
	return c
}

// handleReadable drains readable data from c and invokes onRecv. A zero-byte
// read (EOF) or an error triggers the close path with the matching error.
func (w *worker) handleReadable(c *driverConn, events uint32) {
	// Serialize with WriteAndPoll's caller-side reads so the protocol
	// state machine (driven by onRecv) is never entered concurrently.
	c.recvMu.Lock()
	defer c.recvMu.Unlock()
	// Edge-triggered epoll delivers exactly one edge per "not readable" →
	// "readable" transition. We MUST drain to EAGAIN; stopping early (e.g.
	// on a short read) leaves bytes in the kernel buffer and, crucially,
	// no further edge will fire until those bytes are first consumed AND
	// new data arrives — a silent stall under pipelined traffic.
	//
	// Each read goes through readOpen: once the conn is torn down
	// (UnregisterConn, on another goroutine, while onRecv runs here), fd's
	// number may already belong to another file, so the loop stops, and it
	// does not finish this event's EPOLLRDHUP teardown either (celeris#784).
	for {
		n, err := readOpen(c, w.rbuf)
		if err == engine.ErrUnknownFD {
			return
		}
		if n > 0 && c.onRecv != nil {
			c.onRecv(w.rbuf[:n])
		}
		if err != nil {
			if isEAGAIN(err) {
				break
			}
			w.errorClose(c, err)
			return
		}
		if n == 0 {
			// Peer performed orderly shutdown.
			w.errorClose(c, nil)
			return
		}
	}
	if events&(unix.EPOLLRDHUP|unix.EPOLLHUP|unix.EPOLLERR) != 0 {
		w.errorClose(c, nil)
	}
}

// drainPending processes the conns queued by enqueueFlush after a flush
// stopped at EAGAIN. A fresh backing array is installed under pendingMu so
// concurrent enqueueFlush appends don't race with the worker's iteration
// over the snapshot.
func (w *worker) drainPending() {
	w.pendingMu.Lock()
	list := w.pending
	w.pending = nil
	w.pendingMu.Unlock()
	for _, c := range list {
		w.drainOne(c)
	}
}

// drainOne flushes c's pending bytes under c.mu, on EPOLLOUT or from the
// pending list, unless c has been torn down.
func (w *worker) drainOne(c *driverConn) {
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
		w.errorClose(c, err)
	}
}
