//go:build linux

package epoll

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/internal/platform"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// Engine implements the epoll-based I/O engine.
type Engine struct {
	loops        []*Loop
	cfg          resource.Config
	handler      stream.Handler
	addr         atomic.Pointer[net.Addr]
	mu           sync.Mutex
	acceptPaused atomic.Bool
	metrics      struct {
		reqCount    atomic.Uint64
		activeConns atomic.Int64
		errCount    atomic.Uint64
	}
}

// New creates a new epoll engine.
func New(cfg resource.Config, handler stream.Handler) (*Engine, error) {
	cfg = cfg.WithDefaults()
	if errs := cfg.Validate(); len(errs) > 0 {
		return nil, fmt.Errorf("config validation: %w", errs[0])
	}

	return &Engine{
		cfg:     cfg,
		handler: handler,
	}, nil
}

// Listen starts the epoll engine and blocks until context is canceled.
func (e *Engine) Listen(ctx context.Context) error {
	// If a listener was provided (StartWithListener), use its bound address
	// and close the Go-managed listener so our raw epoll sockets can bind
	// with SO_REUSEPORT. Log the ownership transfer so users see it.
	if e.cfg.Listener != nil {
		e.cfg.Addr = e.cfg.Listener.Addr().String()
		if e.cfg.Logger != nil {
			e.cfg.Logger.Info("epoll: closing supplied listener to rebind via SO_REUSEPORT",
				"addr", e.cfg.Addr)
		}
		e.cfg.Listener.Close()
		e.cfg.Listener = nil
	}

	resolved := e.cfg.Resources.Resolve()

	topo := platform.DetectNUMA()
	cpus := platform.DistributeWorkers(resolved.Workers, runtime.NumCPU(), topo.NumNodes)

	if topo.NumNodes > 1 {
		resolved.MaxEvents = resolved.MaxEvents / topo.NumNodes
		if resolved.MaxEvents < 64 {
			resolved.MaxEvents = 64
		}
	}

	e.mu.Lock()
	e.loops = make([]*Loop, resolved.Workers)
	for i := range resolved.Workers {
		l := newLoop(i, cpus[i], e.handler,
			resolved, e.cfg,
			&e.metrics.reqCount, &e.metrics.activeConns, &e.metrics.errCount,
			&e.acceptPaused)
		e.loops[i] = l
	}
	e.mu.Unlock()

	innerCtx, innerCancel := context.WithCancel(ctx)
	defer innerCancel()

	var wg sync.WaitGroup
	for _, l := range e.loops {
		wg.Go(func() {
			l.run(innerCtx)
		})
	}

	for _, l := range e.loops {
		if initErr := <-l.ready; initErr != nil {
			innerCancel()
			wg.Wait()
			return initErr
		}
	}

	if len(e.loops) > 0 {
		addr := boundAddr(e.loops[0].listenFD)
		e.addr.Store(&addr)
	}

	e.cfg.Logger.Info("epoll engine listening", "addr", e.cfg.Addr, "loops", resolved.Workers)

	<-ctx.Done()
	wg.Wait()
	return nil
}

// Shutdown gracefully shuts down the engine.
func (e *Engine) Shutdown(_ context.Context) error {
	return nil
}

// Metrics returns a snapshot of engine metrics.
func (e *Engine) Metrics() engine.EngineMetrics {
	return engine.EngineMetrics{
		RequestCount:      e.metrics.reqCount.Load(),
		ActiveConnections: e.metrics.activeConns.Load(),
		ErrorCount:        e.metrics.errCount.Load(),
	}
}

// Type returns the engine type.
func (e *Engine) Type() engine.EngineType {
	return engine.Epoll
}

// PauseAccept stops accepting new connections while keeping existing ones alive.
func (e *Engine) PauseAccept() error {
	e.acceptPaused.Store(true)
	return nil
}

// ResumeAccept starts accepting new connections again.
// Wakes any suspended loops so they re-create listen sockets.
func (e *Engine) ResumeAccept() error {
	e.acceptPaused.Store(false)
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, l := range e.loops {
		l.wakeMu.Lock()
		if l.suspended.Load() {
			close(l.wake)
			l.wake = make(chan struct{})
			l.suspended.Store(false)
		}
		l.wakeMu.Unlock()
	}
	return nil
}

var (
	_ engine.Engine           = (*Engine)(nil)
	_ engine.AcceptController = (*Engine)(nil)
)

// Addr returns the bound listener address.
func (e *Engine) Addr() net.Addr {
	if p := e.addr.Load(); p != nil {
		return *p
	}
	return nil
}
