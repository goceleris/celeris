//go:build linux

package epoll

import (
	"context"
	"log/slog"
	"sync/atomic"

	"github.com/goceleris/celeris/engine/scaler"
)

// epollScalerSource adapts the epoll Engine to the scaler.Source
// interface. Generation always returns 0 — epoll isn't a meta-engine.
type epollScalerSource struct {
	e           *Engine
	activeConns *atomic.Int64
}

func (s *epollScalerSource) NumWorkers() int      { return s.e.NumWorkers() }
func (s *epollScalerSource) ActiveConns() int64   { return s.activeConns.Load() }
func (s *epollScalerSource) PauseWorker(i int)    { s.e.PauseWorker(i) }
func (s *epollScalerSource) ResumeWorker(i int)   { s.e.ResumeWorker(i) }
func (s *epollScalerSource) Generation() uint64   { return 0 }
func (s *epollScalerSource) Logger() *slog.Logger { return s.e.cfg.Logger }

// runScaler is started by Engine.Listen when scaler config is enabled.
// All algorithm logic lives in engine/scaler.
func (e *Engine) runScaler(ctx context.Context, cfg scaler.Config, activeConns *atomic.Int64) {
	scaler.Run(ctx, &epollScalerSource{e: e, activeConns: activeConns}, cfg)
}

// PauseWorker deactivates loop i. The loop drains in-flight conns and
// goes SUSPENDED. Asynchronous; returns immediately.
func (e *Engine) PauseWorker(i int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if i < 0 || i >= len(e.loops) {
		return
	}
	e.loops[i].inactive.Store(true)
}

// ResumeWorker reactivates loop i. Wakes the loop from SUSPENDED if it was
// already idle.
func (e *Engine) ResumeWorker(i int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if i < 0 || i >= len(e.loops) {
		return
	}
	l := e.loops[i]
	l.inactive.Store(false)
	l.listenFDClosed.Store(false)
	l.wakeMu.Lock()
	if l.suspended.Load() {
		close(l.wake)
		l.wake = make(chan struct{})
		l.suspended.Store(false)
	}
	l.wakeMu.Unlock()
}
