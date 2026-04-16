package engine

import "errors"

// ErrQueueFull is returned by [WorkerLoop.Write] when the worker's outbound
// queue is full and cannot accept more data without blocking. Callers should
// apply backpressure to their data source rather than retry in a tight loop.
var ErrQueueFull = errors.New("celeris/engine: worker write queue full")

// ErrUnknownFD is returned when a [WorkerLoop] operation references a file
// descriptor that is not registered on that worker.
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
// All methods are safe to call from any goroutine. [Write] is serialized
// per-FD internally (at most one send is in flight per descriptor), following
// the same invariant the HTTP path relies on.
//
// # Callback semantics
//
// The onRecv and onClose callbacks passed to [RegisterConn] are invoked from
// the worker's own goroutine. The []byte slice passed to onRecv is only valid
// for the duration of the callback — callers must copy before retaining it.
// onClose is called exactly once, either in response to [UnregisterConn] or
// when the kernel reports a closed/errored connection. The error argument to
// onClose is nil on orderly shutdown and non-nil on I/O errors.
//
// Drivers must not call [RegisterConn] / [UnregisterConn] / [Write] from
// within an onRecv or onClose callback targeting the same FD; use a separate
// goroutine if such re-entrance is needed.
type WorkerLoop interface {
	// RegisterConn adds fd to the worker's interest set and installs the
	// data and close callbacks. The fd is expected to be already connected
	// and in non-blocking mode. Returns an error if fd is already registered
	// on this or another worker.
	RegisterConn(fd int, onRecv func([]byte), onClose func(error)) error
	// UnregisterConn removes fd from the worker's interest set and triggers
	// the onClose callback (with a nil error) after any in-flight operations
	// complete. The caller is responsible for closing the fd itself.
	UnregisterConn(fd int) error
	// Write enqueues data for transmission on fd. The call does not block on
	// the kernel send buffer: data is buffered and flushed asynchronously by
	// the worker. Returns [ErrQueueFull] if the worker's outbound queue is
	// saturated. The data slice may be retained by the worker until the
	// write completes; callers must not modify it after calling Write.
	Write(fd int, data []byte) error
	// CPUID returns the CPU this worker is pinned to, or -1 if the worker is
	// not pinned. Drivers may use this to co-locate allocations (e.g.,
	// NUMA-aware buffers) with the worker.
	CPUID() int
}
