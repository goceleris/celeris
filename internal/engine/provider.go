package engine

import "errors"

// ErrQueueFull is returned by [WorkerLoop.Write] when the worker's outbound
// queue is full and cannot accept more data without blocking. Callers should
// apply backpressure to their data source rather than retry in a tight loop.
var ErrQueueFull = errors.New("celeris/engine: worker write queue full")

// ErrUnknownFD is returned when a [WorkerLoop] operation references a file
// descriptor that is not registered on that worker. [WorkerLoop.Write] also
// returns it for a descriptor that is already closing (after
// [WorkerLoop.UnregisterConn], or after the connection failed).
var ErrUnknownFD = errors.New("celeris/engine: file descriptor not registered")

// ErrSwitchingNotFrozen is returned by adaptive engines when a driver attempts
// to register a connection without first calling [SwitchFreezer.FreezeSwitching].
// Driver FDs cannot migrate across epoll/io_uring worker boundaries, so the
// adaptive engine must be held on a single sub-engine for the driver's lifetime.
var ErrSwitchingNotFrozen = errors.New("celeris/engine: adaptive engine switching is not frozen; call FreezeSwitching before registering driver connections")

// EventLoopProvider is implemented by engines that expose their per-worker
// event loops for database/cache driver integration. Drivers call [WorkerLoop]
// to obtain a specific worker and register file descriptors on it, sharing the
// HTTP server's epoll instance or io_uring rings.
//
// A nil return from [celeris.Server.EventLoopProvider] (or a type assertion
// failure here) indicates the engine does not support driver integration
// (e.g., the standard net/http fallback). Drivers should fall back to the
// standalone mini event loop in driver/internal/eventloop in that case.
//
// Implementations must be safe for concurrent use after the engine has begun
// listening.
type EventLoopProvider interface {
	// NumWorkers returns the number of worker event loops available for
	// driver FD registration. This is typically equal to the engine's
	// configured worker count (one per CPU core).
	NumWorkers() int
	// WorkerLoop returns the WorkerLoop for worker n, where 0 <= n < NumWorkers().
	// Calling WorkerLoop with an out-of-range index panics.
	WorkerLoop(n int) WorkerLoop
}

// WorkerLoop is the per-worker surface that drivers use to register file
// descriptors, send data, and receive callbacks when data arrives or the
// connection closes.
//
// # Thread safety
//
// All methods are safe to call from any goroutine. [WorkerLoop.Write] is serialized
// per-FD internally (at most one send is in flight per descriptor), following
// the same invariant the HTTP path relies on.
//
// # Callback semantics
//
// The onRecv and onClose callbacks passed to [WorkerLoop.RegisterConn] are
// invoked from the worker's own goroutine, with exceptions. On the epoll and
// standalone Linux loops, onClose can run on a caller's goroutine: that of an
// [WorkerLoop.UnregisterConn] that is first to close the conn, of a
// [WorkerLoop.Write] (or, on the standalone loop, a WriteAndPoll-style call)
// that fails and so tears the conn down, and, on the standalone loop, of
// Loop.Close. On the portable loop both callbacks run on the conn's reader
// goroutine, and the standalone loop's synchronous WriteAndPoll-style calls
// run onRecv on the caller's goroutine.
// The []byte slice passed to onRecv is only valid for the duration of the
// callback — callers must copy before retaining it.
// onClose is called exactly once, either in response to [WorkerLoop.UnregisterConn] or
// when the kernel reports a closed/errored connection. The error argument to
// onClose is nil on orderly shutdown and non-nil on I/O errors.
//
// Drivers must not call [WorkerLoop.RegisterConn] / [WorkerLoop.UnregisterConn] / [WorkerLoop.Write] from
// within an onRecv or onClose callback targeting the same FD; use a separate
// goroutine if such re-entrance is needed.
type WorkerLoop interface {
	// RegisterConn adds fd to the worker's interest set and installs the
	// data and close callbacks. The fd is expected to be already connected
	// and in non-blocking mode. Returns an error if fd is already registered
	// on this worker.
	//
	// onClose of an earlier registration of the same number can run after
	// RegisterConn has taken the number again (see [WorkerLoop.UnregisterConn]),
	// so a driver must not key its onClose state by fd number alone.
	RegisterConn(fd int, onRecv func([]byte), onClose func(error)) error
	// UnregisterConn removes fd from the worker's interest set and triggers
	// the onClose callback (with a nil error) after any in-flight operations
	// complete. The caller is responsible for closing the fd itself.
	//
	// fd must still be open when UnregisterConn is called. Closing fd before
	// calling UnregisterConn is outside this contract, because by then its
	// number may name another file, and the epoll engine would remove that
	// file from its interest set.
	//
	// On the io_uring engine the caller may close fd as soon as UnregisterConn
	// returns, without waiting for onClose: the engine works on its own
	// duplicate of fd, taken by RegisterConn, and closes it just before onClose
	// fires (celeris#691). It closes it off the worker, so a close that
	// lingers (SO_LINGER) delays that conn's onClose, not the worker's other
	// connections (celeris#735). Until onClose fires, fd's number may stay
	// registered on that worker: a RegisterConn there of the next socket to
	// get the number, as the lowest free number usually is, fails with "fd
	// already registered" until the conn is finalized, which is at the latest
	// when onClose fires. Closing fd before UnregisterConn does not misdirect
	// the cancel on this engine, which works on its own duplicate of fd, but
	// it is still outside the contract: the duplicate keeps the socket open
	// (the peer sees no FIN) until the conn is finalized, so a caller that
	// closes fd and never unregisters it leaks the socket until the peer closes.
	//
	// The epoll engine fires onClose before UnregisterConn returns, unless
	// another path closed the conn first (a peer close, a read error or the
	// loop's shutdown on the worker, or a failed Write on a caller's
	// goroutine); then onClose runs there and may still be running when
	// UnregisterConn returns. Once UnregisterConn has returned, the epoll worker
	// reads fd no more, even from inside the conn's read loop (celeris#710,
	// celeris#772), and so does the standalone Linux loop (celeris#784,
	// celeris#843). Either way, a read that completed before UnregisterConn
	// took the conn's lock may still deliver its bytes to onRecv, which can run
	// after UnregisterConn has returned.
	//
	// onClose can run after UnregisterConn has returned, and after the same
	// number has been registered again. On epoll, the standalone Linux loop and
	// io_uring that happens when the conn is closed by another path first (a
	// peer close, a read error, a failed write or shutdown), since those
	// engines remove the conn from their table before they run onClose. On the
	// portable loop and on io_uring it also happens after an UnregisterConn:
	// the portable reader goroutine fires onClose once it observes the close
	// (after a Write timeout the entry stays until the owner unregisters it),
	// and io_uring fires it once the conn is finalized, which can be after
	// RegisterConn has taken the number. So a driver must not key its onClose
	// state by fd number; it can key it by the registration, for example by
	// passing onClose a value that identifies it.
	UnregisterConn(fd int) error
	// Write enqueues data for transmission on fd. The call does not block on
	// the kernel send buffer: data is buffered and flushed asynchronously by
	// the worker. Returns [ErrQueueFull] if the worker's outbound queue is
	// saturated, and [ErrUnknownFD] if fd is not registered on this worker or
	// is already closing: after [WorkerLoop.UnregisterConn], or after the
	// connection failed, no further bytes are sent, and the engines' workers
	// report that rather than success (the io_uring worker used to return nil
	// and drop the data; the standalone driver loop reports ErrLoopClosed
	// instead once it is closed). A caller racing its own close treats
	// ErrUnknownFD as a closed connection. The data slice may be retained by
	// the worker until the write completes; callers must not modify it after
	// calling Write.
	Write(fd int, data []byte) error
	// CPUID returns the CPU this worker is pinned to, or -1 if the worker is
	// not pinned. Drivers may use this to co-locate allocations (e.g.,
	// NUMA-aware buffers) with the worker.
	CPUID() int
}
