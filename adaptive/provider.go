//go:build linux

package adaptive

import (
	"errors"
	"sync"

	"github.com/goceleris/celeris/engine"
)

var _ engine.EventLoopProvider = (*Engine)(nil)

// NumWorkers delegates to the currently active sub-engine's EventLoopProvider.
// Returns 0 if the active engine does not implement EventLoopProvider.
func (e *Engine) NumWorkers() int {
	if p, ok := (*e.active.Load()).(engine.EventLoopProvider); ok {
		return p.NumWorkers()
	}
	return 0
}

// WorkerLoop returns a driver-safe wrapper around the currently active
// sub-engine's worker N. The wrapper:
//
//  1. Re-fetches the active sub-engine on every RegisterConn, so the FD
//     lands on whichever sub-engine is active at that instant — this
//     matters only during the tiny window between one register/unregister
//     cycle and the next (the refcount pins frozen while any FD lives).
//  2. Pins the specific inner [engine.WorkerLoop] each FD was registered
//     on so Write/Unregister always go to the right sub-engine.
//  3. Increments the engine's driver-FD refcount on RegisterConn and
//     decrements on UnregisterConn, which keeps [Engine.performSwitch]
//     from swapping sub-engines while any FD is live.
//
// Callers never need to call [Engine.FreezeSwitching] manually.
func (e *Engine) WorkerLoop(n int) engine.WorkerLoop {
	// Sanity check: the current active engine must be a provider. We do
	// not snapshot here — the wrapper resolves on each Register.
	if _, ok := (*e.active.Load()).(engine.EventLoopProvider); !ok {
		panic("celeris/adaptive: active engine does not implement EventLoopProvider")
	}
	return &driverWorkerLoop{
		n:      n,
		engine: e,
	}
}

// driverWorkerLoop is a thin dispatcher that (a) tracks which sub-engine
// each registered FD lives on and (b) participates in the adaptive
// engine's driver-FD refcount via Acquire/Release on the surrounding
// Engine.
type driverWorkerLoop struct {
	n      int
	engine *Engine

	mu     sync.Mutex
	pinned map[int]engine.WorkerLoop // fd -> inner loop it was registered on
}

// RegisterConn atomically (a) increments the engine's driver-FD count
// which pins frozen=true, (b) resolves the active sub-engine's worker N,
// (c) registers there, and (d) records the mapping for later Write /
// Unregister calls.
func (d *driverWorkerLoop) RegisterConn(fd int, onRecv func([]byte), onClose func(error)) error {
	d.engine.acquireDriverFD()
	p, ok := (*d.engine.active.Load()).(engine.EventLoopProvider)
	if !ok {
		d.engine.releaseDriverFD()
		return errors.New("celeris/adaptive: active engine does not implement EventLoopProvider")
	}
	inner := p.WorkerLoop(d.n)
	if err := inner.RegisterConn(fd, onRecv, onClose); err != nil {
		d.engine.releaseDriverFD()
		return err
	}
	d.mu.Lock()
	if d.pinned == nil {
		d.pinned = make(map[int]engine.WorkerLoop)
	}
	d.pinned[fd] = inner
	d.mu.Unlock()
	return nil
}

// UnregisterConn forwards to the inner loop that RegisterConn picked for
// this FD, then decrements the engine's driver-FD count. The order
// matters: the inner Unregister may invoke the onClose callback
// synchronously, and callers must see the engine still holding them
// before the refcount thaws.
func (d *driverWorkerLoop) UnregisterConn(fd int) error {
	d.mu.Lock()
	inner, ok := d.pinned[fd]
	if ok {
		delete(d.pinned, fd)
	}
	d.mu.Unlock()
	if !ok {
		return engine.ErrUnknownFD
	}
	err := inner.UnregisterConn(fd)
	d.engine.releaseDriverFD()
	return err
}

// Write forwards to the sub-engine that holds this FD.
func (d *driverWorkerLoop) Write(fd int, data []byte) error {
	d.mu.Lock()
	inner, ok := d.pinned[fd]
	d.mu.Unlock()
	if !ok {
		return engine.ErrUnknownFD
	}
	return inner.Write(fd, data)
}

// CPUID returns the CPU of the currently active sub-engine's worker N.
// Drivers that have not yet registered an FD may observe a CPU from a
// sub-engine that subsequently changes; post-registration the pinned
// inner's CPU is stable for the FD's lifetime because the engine cannot
// switch while driver FDs are live.
func (d *driverWorkerLoop) CPUID() int {
	p, ok := (*d.engine.active.Load()).(engine.EventLoopProvider)
	if !ok {
		return -1
	}
	return p.WorkerLoop(d.n).CPUID()
}
