//go:build linux

package iouring

import (
	"context"
	"log/slog"
	"sync/atomic"

	"github.com/goceleris/celeris/engine/scaler"
)

// iouringScalerSource adapts the iouring Engine to the scaler.Source
// interface. Generation always returns 0 — iouring isn't a meta-engine
// and doesn't switch identities at runtime; only the adaptive engine's
// source returns non-zero generations.
type iouringScalerSource struct {
	e           *Engine
	activeConns *atomic.Int64
}

func (s *iouringScalerSource) NumWorkers() int       { return s.e.NumWorkers() }
func (s *iouringScalerSource) ActiveConns() int64    { return s.activeConns.Load() }
func (s *iouringScalerSource) PauseWorker(i int)     { s.e.PauseWorker(i) }
func (s *iouringScalerSource) ResumeWorker(i int)    { s.e.ResumeWorker(i) }
func (s *iouringScalerSource) Generation() uint64    { return 0 }
func (s *iouringScalerSource) Logger() *slog.Logger  { return s.e.cfg.Logger }

// runScaler is started by Engine.Listen when scaler config is enabled.
// All algorithm logic lives in engine/scaler; this is a thin source
// adapter so the iouring engine plugs into the shared loop.
func (e *Engine) runScaler(ctx context.Context, cfg scaler.Config, activeConns *atomic.Int64) {
	scaler.Run(ctx, &iouringScalerSource{e: e, activeConns: activeConns}, cfg)
}

// PauseWorker deactivates worker i. The worker drains in-flight conns
// and goes SUSPENDED. Asynchronous; returns immediately.
func (e *Engine) PauseWorker(i int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if i < 0 || i >= len(e.workers) {
		return
	}
	e.workers[i].inactive.Store(true)
}

// ResumeWorker reactivates worker i. Wakes the worker from SUSPENDED if
// it was already idle.
func (e *Engine) ResumeWorker(i int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if i < 0 || i >= len(e.workers) {
		return
	}
	w := e.workers[i]
	w.inactive.Store(false)
	w.listenFDClosed.Store(false)
	w.wakeMu.Lock()
	if w.suspended.Load() {
		close(w.wake)
		w.wake = make(chan struct{})
		w.suspended.Store(false)
	}
	w.wakeMu.Unlock()
}
