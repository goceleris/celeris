package eventloop

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/goceleris/celeris/engine"
)

// ErrLoopClosed is returned when an operation is issued against a Loop that
// has already been closed.
var ErrLoopClosed = errors.New("celeris/eventloop: loop is closed")

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

// ErrAlreadyRegistered is returned when RegisterConn is called for a fd that
// is already registered on this worker.
var ErrAlreadyRegistered = errors.New("celeris/eventloop: fd already registered")

// loopWorker is the internal interface that all per-worker implementations
// (epoll, io_uring, goroutine-per-conn fallback) must satisfy. It extends
// engine.WorkerLoop with lifecycle methods needed by the Loop container.
type loopWorker interface {
	engine.WorkerLoop
	shutdown() error
}

// wakeable is an optional interface for workers that support explicit wakeup
// via an eventfd or similar mechanism (Linux epoll and io_uring workers).
type wakeable interface {
	wake()
}

// Loop is the standalone event loop used by drivers when no HTTP server is
// available. The struct is shared across all platforms; platform-specific
// details live behind the loopWorker implementations created by newLoop.
type Loop struct {
	workers []loopWorker
	closed  atomic.Bool
	wg      sync.WaitGroup
	cancel  context.CancelFunc
}

// New creates a standalone Loop. If workers <= 0, runtime.NumCPU() is used.
// The caller must invoke [Loop.Close] when finished. Drivers must unregister
// any FDs they own before closing the loop.
func New(workers int) (*Loop, error) {
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	if workers < 1 {
		workers = 1
	}
	return newLoop(workers)
}

// NumWorkers satisfies [engine.EventLoopProvider].
func (l *Loop) NumWorkers() int {
	return len(l.workers)
}

// WorkerLoop satisfies [engine.EventLoopProvider]. Panics if n is out of range.
func (l *Loop) WorkerLoop(n int) engine.WorkerLoop {
	if n < 0 || n >= len(l.workers) {
		panic("celeris/eventloop: worker index out of range")
	}
	return l.workers[n]
}

// Close stops all workers and releases their resources. Drivers must
// unregister their FDs before calling Close; FDs still registered at Close
// time receive onClose(ErrLoopClosed).
func (l *Loop) Close() error {
	if !l.closed.CompareAndSwap(false, true) {
		return nil
	}
	if l.cancel != nil {
		l.cancel()
	}
	for _, w := range l.workers {
		if wk, ok := w.(wakeable); ok {
			wk.wake()
		}
	}
	l.wg.Wait()

	var first error
	for _, w := range l.workers {
		if err := w.shutdown(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// workerForFD hashes fd → worker index. Driver FDs are long-lived so a simple
// modulo hash spreads load adequately; drivers that need specific placement
// can call WorkerLoop(n).RegisterConn directly.
func (l *Loop) workerForFD(fd int) int {
	if fd < 0 {
		fd = -fd
	}
	return fd % len(l.workers)
}
