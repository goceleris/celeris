//go:build linux

package adaptive

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/engine/epoll"
	"github.com/goceleris/celeris/engine/iouring"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// addrEngine is an engine that also exposes its bound address.
type addrEngine interface {
	engine.Engine
	Addr() net.Addr
}

// Engine is an adaptive meta-engine that switches between io_uring and epoll.
type Engine struct {
	primary   addrEngine // io_uring
	secondary addrEngine // epoll
	active    atomic.Pointer[engine.Engine]
	ctrl      *controller
	cfg       resource.Config
	handler   stream.Handler
	addr      atomic.Pointer[net.Addr]
	mu        sync.Mutex
	frozen    atomic.Bool
	logger    *slog.Logger
}

// New creates a new adaptive engine with io_uring as primary and epoll as secondary.
func New(cfg resource.Config, handler stream.Handler) (*Engine, error) {
	cfg = cfg.WithDefaults()
	if errs := cfg.Validate(); len(errs) > 0 {
		return nil, fmt.Errorf("config validation: %w", errs[0])
	}

	iouringCfg := cfg
	epollCfg := cfg

	primary, err := iouring.New(iouringCfg, handler)
	if err != nil {
		return nil, fmt.Errorf("io_uring sub-engine: %w", err)
	}

	secondary, err := epoll.New(epollCfg, handler)
	if err != nil {
		return nil, fmt.Errorf("epoll sub-engine: %w", err)
	}

	sampler := newLiveSampler()
	logger := cfg.Logger

	e := &Engine{
		primary:   primary,
		secondary: secondary,
		cfg:       cfg,
		handler:   handler,
		logger:    logger,
	}

	e.ctrl = newController(primary, secondary, sampler, logger)

	// Set initial active engine per SDD 7.5.
	var initialActive engine.Engine
	switch cfg.Protocol {
	case engine.H2C:
		initialActive = engine.Engine(secondary)
		e.ctrl.state.activeIsPrimary = false
	default: // HTTP1, Auto → io_uring
		initialActive = engine.Engine(primary)
		e.ctrl.state.activeIsPrimary = true
	}
	e.active.Store(&initialActive)

	return e, nil
}

// newFromEngines creates an adaptive engine from pre-built engines (for testing).
func newFromEngines(primary, secondary addrEngine, sampler TelemetrySampler, cfg resource.Config) *Engine {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	e := &Engine{
		primary:   primary,
		secondary: secondary,
		cfg:       cfg,
		logger:    logger,
	}

	e.ctrl = newController(primary, secondary, sampler, logger)

	var initialActive engine.Engine
	switch cfg.Protocol {
	case engine.H2C:
		initialActive = engine.Engine(secondary)
		e.ctrl.state.activeIsPrimary = false
	default:
		initialActive = engine.Engine(primary)
		e.ctrl.state.activeIsPrimary = true
	}
	e.active.Store(&initialActive)

	return e
}

// Listen starts both sub-engines and the evaluation loop.
func (e *Engine) Listen(ctx context.Context) error {
	innerCtx, innerCancel := context.WithCancel(ctx)
	defer innerCancel()

	var wg sync.WaitGroup

	errCh := make(chan error, 2)

	wg.Go(func() {
		if err := e.primary.Listen(innerCtx); err != nil {
			errCh <- fmt.Errorf("primary (io_uring): %w", err)
		}
	})

	wg.Go(func() {
		if err := e.secondary.Listen(innerCtx); err != nil {
			errCh <- fmt.Errorf("secondary (epoll): %w", err)
		}
	})

	// Wait for both engines to bind their addresses.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if e.primary.Addr() != nil && e.secondary.Addr() != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if e.primary.Addr() == nil || e.secondary.Addr() == nil {
		innerCancel()
		wg.Wait()
		return fmt.Errorf("sub-engines failed to initialize")
	}

	addr := e.primary.Addr()
	e.addr.Store(&addr)

	// Pause standby engine's accept.
	if e.ctrl.state.activeIsPrimary {
		if ac, ok := e.secondary.(engine.AcceptController); ok {
			_ = ac.PauseAccept()
		}
	} else {
		if ac, ok := e.primary.(engine.AcceptController); ok {
			_ = ac.PauseAccept()
		}
	}

	e.logger.Info("adaptive engine listening",
		"addr", e.cfg.Addr,
		"active", (*e.active.Load()).Type().String(),
	)

	// Start evaluation loop.
	wg.Go(func() {
		e.runEvalLoop(innerCtx)
	})

	select {
	case <-ctx.Done():
	case err := <-errCh:
		innerCancel()
		wg.Wait()
		return err
	}

	innerCancel()
	wg.Wait()
	return nil
}

func (e *Engine) runEvalLoop(ctx context.Context) {
	ticker := time.NewTicker(e.ctrl.evalInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if e.ctrl.evaluate(now, e.frozen.Load()) {
				e.performSwitch()
			}
		}
	}
}

func (e *Engine) performSwitch() {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now()

	var newActive, newStandby addrEngine
	if e.ctrl.state.activeIsPrimary {
		// Switching: primary → secondary.
		newActive = e.secondary
		newStandby = e.primary
	} else {
		// Switching: secondary → primary.
		newActive = e.primary
		newStandby = e.secondary
	}

	// Pause standby (was active), resume new active.
	if ac, ok := newStandby.(engine.AcceptController); ok {
		_ = ac.PauseAccept()
	}
	if ac, ok := newActive.(engine.AcceptController); ok {
		_ = ac.ResumeAccept()
	}

	var eng engine.Engine = newActive
	e.active.Store(&eng)
	e.ctrl.recordSwitch(now)

	e.logger.Info("engine switch completed",
		"now_active", newActive.Type().String(),
		"now_standby", newStandby.Type().String(),
	)
}

// Shutdown gracefully shuts down both sub-engines.
func (e *Engine) Shutdown(ctx context.Context) error {
	return errors.Join(
		e.primary.Shutdown(ctx),
		e.secondary.Shutdown(ctx),
	)
}

// Metrics aggregates metrics from both sub-engines.
func (e *Engine) Metrics() engine.EngineMetrics {
	pm := e.primary.Metrics()
	sm := e.secondary.Metrics()
	return engine.EngineMetrics{
		RequestCount:      pm.RequestCount + sm.RequestCount,
		ActiveConnections: pm.ActiveConnections + sm.ActiveConnections,
		ErrorCount:        pm.ErrorCount + sm.ErrorCount,
		Throughput:        pm.Throughput + sm.Throughput,
		LatencyP50:        max(pm.LatencyP50, sm.LatencyP50),
		LatencyP99:        max(pm.LatencyP99, sm.LatencyP99),
		LatencyP999:       max(pm.LatencyP999, sm.LatencyP999),
	}
}

// Type returns the engine type.
func (e *Engine) Type() engine.EngineType {
	return engine.Adaptive
}

// Addr returns the bound listener address.
func (e *Engine) Addr() net.Addr {
	if p := e.addr.Load(); p != nil {
		return *p
	}
	return nil
}

// FreezeSwitching prevents the controller from switching engines.
func (e *Engine) FreezeSwitching() {
	e.frozen.Store(true)
}

// UnfreezeSwitching allows the controller to switch engines again.
func (e *Engine) UnfreezeSwitching() {
	e.frozen.Store(false)
}

// ActiveEngine returns the currently active engine.
func (e *Engine) ActiveEngine() engine.Engine {
	return *e.active.Load()
}
