//go:build linux

package adaptive

import (
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

// WorkerLoop delegates to the currently active sub-engine's EventLoopProvider.
// Panics if the active engine does not implement EventLoopProvider or if
// switching is not frozen.
func (e *Engine) WorkerLoop(n int) engine.WorkerLoop {
	if !e.frozen.Load() {
		panic("celeris/adaptive: WorkerLoop called without FreezeSwitching; driver FDs cannot survive an engine switch")
	}
	p, ok := (*e.active.Load()).(engine.EventLoopProvider)
	if !ok {
		panic("celeris/adaptive: active engine does not implement EventLoopProvider")
	}
	return p.WorkerLoop(n)
}
