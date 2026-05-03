//go:build linux

package adaptive

import (
	"context"
	"log/slog"
	"sync/atomic"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/engine/scaler"
)

// adaptiveScalerSource adapts the adaptive Engine to the scaler.Source
// interface. The active sub-engine can change at runtime
// (performSwitch); this source watches for that and routes
// PauseWorker / ResumeWorker calls to whichever engine is active right
// now. Generation increments on each switch so the shared scaler loop
// can re-baseline the new active engine's worker pause state.
//
// Both sub-engines must implement engine.WorkerScaler — verified via a
// type-assertion in the constructor. Both must have the same NumWorkers;
// if they ever diverge (defensive), the smaller pool is used as the cap.
type adaptiveScalerSource struct {
	e       *Engine
	primary engine.WorkerScaler
	standby engine.WorkerScaler
	gen     atomic.Uint64

	// lastActive tracks which sub-engine was active on the previous
	// scaler-source method call. When activeFor() observes a different
	// engine, gen is incremented so the scaler.Run loop notices and
	// re-baselines.
	lastActive atomic.Pointer[engine.Engine]
}

// newAdaptiveScalerSource attempts to construct a Source. Returns nil if
// either sub-engine fails the engine.WorkerScaler assertion (defensive
// — should not happen in production since both iouring and epoll
// implement it).
func newAdaptiveScalerSource(e *Engine) *adaptiveScalerSource {
	primary, ok1 := e.primary.(engine.WorkerScaler)
	standby, ok2 := e.secondary.(engine.WorkerScaler)
	if !ok1 || !ok2 {
		return nil
	}
	return &adaptiveScalerSource{e: e, primary: primary, standby: standby}
}

// activeFor returns the WorkerScaler view of the engine that is
// currently active in adaptive. Bumps Generation if the active engine
// changed since the previous call, so scaler.Run re-baselines.
func (s *adaptiveScalerSource) activeFor() engine.WorkerScaler {
	cur := *s.e.active.Load()
	prev := s.lastActive.Load()
	if prev == nil || *prev != cur {
		s.lastActive.Store(&cur)
		s.gen.Add(1)
	}
	if cur == s.e.secondary {
		return s.standby
	}
	return s.primary
}

func (s *adaptiveScalerSource) NumWorkers() int {
	a, b := s.primary.NumWorkers(), s.standby.NumWorkers()
	if a < b {
		return a
	}
	return b
}

func (s *adaptiveScalerSource) ActiveConns() int64 {
	return (*s.e.active.Load()).Metrics().ActiveConnections
}

func (s *adaptiveScalerSource) PauseWorker(i int) {
	s.activeFor().PauseWorker(i)
}

func (s *adaptiveScalerSource) ResumeWorker(i int) {
	s.activeFor().ResumeWorker(i)
}

func (s *adaptiveScalerSource) Generation() uint64 {
	// Refresh: a tick reading Generation should also drive the
	// active-engine detection so we don't miss a switch that happens
	// between two ticks where neither Pause/Resume nor ActiveConns ran.
	s.activeFor()
	return s.gen.Load()
}

func (s *adaptiveScalerSource) Logger() *slog.Logger { return s.e.logger }

// runScaler is started by Engine.Listen when the scaler is enabled. The
// algorithm itself lives in engine/scaler; this just wires up the
// adaptive-flavoured source.
func (e *Engine) runScaler(ctx context.Context, cfg scaler.Config) {
	src := newAdaptiveScalerSource(e)
	if src == nil {
		if e.logger != nil {
			e.logger.Warn("adaptive scaler disabled: sub-engine does not implement WorkerScaler")
		}
		return
	}
	scaler.Run(ctx, src, cfg)
}
