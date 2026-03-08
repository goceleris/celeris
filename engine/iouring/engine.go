//go:build linux

// Package iouring implements an engine backed by Linux io_uring.
package iouring

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/internal/platform"
	"github.com/goceleris/celeris/probe"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// Engine implements the io_uring-based I/O engine.
type Engine struct {
	workers []*Worker
	tier    TierStrategy
	profile engine.CapabilityProfile
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

// New creates a new io_uring engine.
func New(cfg resource.Config, handler stream.Handler) (*Engine, error) {
	cfg = cfg.WithDefaults()
	if errs := cfg.Validate(); len(errs) > 0 {
		return nil, fmt.Errorf("config validation: %w", errs[0])
	}

	profile := probe.Probe()
	if !profile.IOUringTier.Available() {
		return nil, fmt.Errorf("io_uring not available on this system")
	}

	tier := SelectTier(profile)
	if tier == nil {
		return nil, fmt.Errorf("no suitable io_uring tier available")
	}

	cfg.Logger.Info("io_uring engine selected",
		"tier", tier.Tier().String(),
		"multishot_accept", tier.SupportsMultishotAccept(),
		"provided_buffers", tier.SupportsProvidedBuffers(),
	)

	return &Engine{
		tier:    tier,
		profile: profile,
		cfg:     cfg,
		handler: handler,
	}, nil
}

// Listen starts the io_uring engine and blocks until context is canceled.
func (e *Engine) Listen(ctx context.Context) error {
	objective := resource.ResolveObjective(e.cfg.Objective)
	resolved := e.cfg.Resources.Resolve(runtime.NumCPU())

	cpus := platform.DistributeWorkers(resolved.Workers, e.profile.NumCPU, e.profile.NUMANodes)

	// Probe for the highest working tier by test-creating a ring.
	tier := e.tier
	for {
		testRing, err := NewRing(uint32(resolved.SQERingSize), tier.SetupFlags())
		if err == nil {
			_ = testRing.Close()
			break
		}
		lower := fallbackTier(tier)
		if lower == nil {
			return fmt.Errorf("all io_uring tiers failed, last error: %w", err)
		}
		e.cfg.Logger.Warn("io_uring tier failed, falling back",
			"failed_tier", tier.Tier().String(),
			"fallback_tier", lower.Tier().String(),
			"err", err,
		)
		tier = lower
	}
	e.tier = tier

	workers, err := e.createWorkers(tier, cpus, objective, resolved)
	if err != nil {
		return fmt.Errorf("worker init: %w", err)
	}

	// Inner context allows canceling workers if any fail during init.
	innerCtx, innerCancel := context.WithCancel(ctx)
	defer innerCancel()

	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.run(innerCtx)
		}()
	}

	// Wait for all workers to finish ring initialization (done inside run()
	// after LockOSThread, required by SINGLE_ISSUER).
	for _, w := range workers {
		if initErr := <-w.ready; initErr != nil {
			// A worker failed to create its ring. Cancel all workers.
			innerCancel()
			wg.Wait()
			return initErr
		}
	}

	e.mu.Lock()
	e.workers = workers
	if len(workers) > 0 {
		e.addr = boundAddr(workers[0].listenFD)
	}
	e.mu.Unlock()

	e.cfg.Logger.Info("io_uring engine listening", "addr", e.cfg.Addr, "tier", tier.Tier().String(), "workers", resolved.Workers)

	<-ctx.Done()
	// Workers use SubmitAndWaitTimeout and check ctx.Err() on each iteration,
	// so they will exit within ~100ms of context cancellation.
	wg.Wait()
	return nil
}

func (e *Engine) createWorkers(tier TierStrategy, cpus []int,
	objective resource.ObjectiveParams, resolved resource.ResolvedResources) ([]*Worker, error) {
	workers := make([]*Worker, len(cpus))
	for i := range workers {
		w, err := newWorker(i, cpus[i], tier, e.handler,
			objective, resolved, e.cfg,
			&e.metrics.reqCount, &e.metrics.activeConns, &e.metrics.errCount)
		if err != nil {
			// Clean up already-created workers.
			for _, prev := range workers[:i] {
				if prev != nil {
					prev.shutdown()
				}
			}
			return nil, err
		}
		workers[i] = w
	}
	return workers, nil
}

func fallbackTier(current TierStrategy) TierStrategy {
	switch current.Tier() {
	case engine.Optional:
		return &highTier{}
	case engine.High:
		return &midTier{}
	case engine.Mid:
		return &baseTier{}
	default:
		return nil
	}
}

// Shutdown gracefully shuts down the engine.
func (e *Engine) Shutdown(_ context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
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
	return engine.IOUring
}

// Addr returns the bound listener address.
func (e *Engine) Addr() net.Addr {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.addr
}
