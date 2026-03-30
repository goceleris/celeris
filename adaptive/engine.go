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

var (
	_ engine.Engine        = (*Engine)(nil)
	_ engine.SwitchFreezer = (*Engine)(nil)
)

// Engine is an adaptive meta-engine that switches between io_uring and epoll.
type Engine struct {
	primary   engine.Engine // io_uring
	secondary engine.Engine // epoll
	active    atomic.Pointer[engine.Engine]
	ctrl      *controller
	cfg       resource.Config
	handler   stream.Handler
	addr      atomic.Pointer[net.Addr]
	mu        sync.Mutex
	switchMu  sync.Mutex // protects evaluate + performSwitch coordination
	frozen    atomic.Bool
	logger    *slog.Logger

	// freezeCooldown is the duration to suppress further switches after a switch.
	// Zero means no cooldown (default).
	freezeCooldown time.Duration
}

// New creates a new adaptive engine with epoll as primary and io_uring as secondary.
// Epoll starts first because it has lower H2 latency on current kernels (single-pass
// read→process→write vs io_uring's two-iteration CQE model). The controller may
// switch to io_uring if telemetry indicates it would perform better for the workload.
// Both sub-engines get the full resource config. This is safe because standby
// workers are fully suspended (zero CPU, zero connections, listen sockets closed).
func New(cfg resource.Config, handler stream.Handler) (*Engine, error) {
	cfg = cfg.WithDefaults()
	if errs := cfg.Validate(); len(errs) > 0 {
		return nil, fmt.Errorf("config validation: %w", errs[0])
	}

	// Both sub-engines must share the same port (SO_REUSEPORT) so the
	// adaptive switch works transparently. If the user specified :0,
	// resolve it to a concrete port before creating sub-engines.
	if cfg.Addr != "" {
		resolved, err := resolvePort(cfg.Addr)
		if err == nil {
			cfg.Addr = resolved
		}
	}

	primary, err := epoll.New(cfg, handler)
	if err != nil {
		return nil, fmt.Errorf("epoll sub-engine: %w", err)
	}

	secondary, err := iouring.New(cfg, handler)
	if err != nil {
		return nil, fmt.Errorf("io_uring sub-engine: %w", err)
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

	// Start with primary (epoll) for all protocols. Epoll has better H2
	// throughput on current kernels and matches H1 performance.
	initialActive := engine.Engine(primary)
	e.ctrl.state.activeIsPrimary = true
	e.active.Store(&initialActive)

	return e, nil
}

// newFromEngines creates an adaptive engine from pre-built engines (for testing).
func newFromEngines(primary, secondary engine.Engine, sampler TelemetrySampler, cfg resource.Config) *Engine {
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

	initialActive := engine.Engine(primary)
	e.ctrl.state.activeIsPrimary = true
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
			errCh <- fmt.Errorf("primary (epoll): %w", err)
		}
	})

	wg.Go(func() {
		if err := e.secondary.Listen(innerCtx); err != nil {
			errCh <- fmt.Errorf("secondary (io_uring): %w", err)
		}
	})

	// Wait for both engines to bind their addresses.
	// io_uring may need multiple tier fallback attempts, so allow ample time.
	deadline := time.Now().Add(20 * time.Second)
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
			e.switchMu.Lock()
			shouldSwitch := e.ctrl.evaluate(now, e.frozen.Load())
			e.switchMu.Unlock()
			if shouldSwitch {
				e.performSwitch()
			}
		}
	}
}

func (e *Engine) performSwitch() {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now()

	e.switchMu.Lock()
	var newActive, newStandby engine.Engine
	if e.ctrl.state.activeIsPrimary {
		// Switching: primary → secondary.
		newActive = e.secondary
		newStandby = e.primary
	} else {
		// Switching: secondary → primary.
		newActive = e.primary
		newStandby = e.secondary
	}
	e.switchMu.Unlock()

	// Resume new active BEFORE pausing old — this creates a brief overlap
	// where both engines listen (via SO_REUSEPORT), which is correct. The
	// alternative (pause first) creates a window where NEITHER listens,
	// because io_uring ASYNC_CANCEL and epoll listen socket re-creation
	// are asynchronous.
	if ac, ok := newActive.(engine.AcceptController); ok {
		_ = ac.ResumeAccept()
	}
	if ac, ok := newStandby.(engine.AcceptController); ok {
		_ = ac.PauseAccept()
	}

	eng := newActive
	e.active.Store(&eng)
	e.switchMu.Lock()
	e.ctrl.recordSwitch(now)
	e.switchMu.Unlock()

	e.logger.Info("engine switch completed",
		"now_active", newActive.Type().String(),
		"now_standby", newStandby.Type().String(),
	)

	// Suppress further switches for the cooldown period.
	if e.freezeCooldown > 0 {
		e.frozen.Store(true)
		go func() {
			time.Sleep(e.freezeCooldown)
			e.frozen.Store(false)
		}()
	}
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

// SetFreezeCooldown sets the duration to suppress further switches after a switch.
// Zero disables the cooldown (default). This prevents oscillation under unstable load.
func (e *Engine) SetFreezeCooldown(d time.Duration) {
	e.freezeCooldown = d
}

// ActiveEngine returns the currently active engine.
func (e *Engine) ActiveEngine() engine.Engine {
	return *e.active.Load()
}

// ForceSwitch triggers an immediate engine switch (for testing).
func (e *Engine) ForceSwitch() {
	e.performSwitch()
}

// resolvePort resolves ":0" to a concrete ":PORT" by briefly binding a
// listener. Both sub-engines need the same port for SO_REUSEPORT switching.
func resolvePort(addr string) (string, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return addr, err
	}
	resolved := ln.Addr().String()
	_ = ln.Close()
	return resolved, nil
}
