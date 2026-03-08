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
	loops   []*Loop
	cfg     resource.Config
	handler stream.Handler
	addr    net.Addr
	mu      sync.Mutex
	metrics struct {
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
	objective := resource.ResolveObjective(e.cfg.Objective)
	resolved := e.cfg.Resources.Resolve(runtime.NumCPU())

	cpus := platform.DistributeWorkers(resolved.Workers, runtime.NumCPU(), 1)

	e.mu.Lock()
	e.loops = make([]*Loop, resolved.Workers)
	for i := range resolved.Workers {
		l, err := newLoop(i, cpus[i], e.handler,
			objective, resolved, e.cfg,
			&e.metrics.reqCount, &e.metrics.activeConns, &e.metrics.errCount)
		if err != nil {
			e.mu.Unlock()
			return fmt.Errorf("loop %d init: %w", i, err)
		}
		e.loops[i] = l
	}
	if len(e.loops) > 0 {
		e.addr = boundAddr(e.loops[0].listenFD)
	}
	e.mu.Unlock()

	var wg sync.WaitGroup
	for _, l := range e.loops {
		wg.Go(func() {
			l.run(ctx)
		})
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

// Addr returns the bound listener address.
func (e *Engine) Addr() net.Addr {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.addr
}
