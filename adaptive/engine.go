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

	// freezeState synchronises the three counters below. The counters are
	// atomic so read-only checks (performSwitch) stay lock-free, but any
	// mutation that may flip frozen must hold this mutex to avoid races
	// where two goroutines observe counters==0 and simultaneously transition
	// frozen in opposite directions.
	freezeState    sync.Mutex
	userFreezes    atomic.Int32  // calls to FreezeSwitching not yet matched by UnfreezeSwitching
	driverFDs      atomic.Int32  // driver FDs currently registered via the provider
	switchRejected atomic.Uint64 // telemetry: how many switches were blocked by driver FDs
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
	var initialActive engine.Engine = primary
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

	initialActive := primary
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

	// Pause standby engine's accept BEFORE publishing Addr so the
	// SO_REUSEPORT group only contains the active engine by the time
	// callers start dialing. Without this gate, a burst of incoming
	// connections in the window between secondary.Listen() succeeding
	// and PauseAccept taking effect lands some dials on the standby's
	// accept queue; closing that queue then RSTs them. H1 clients
	// reconnect transparently but H2 prior-knowledge clients see the
	// dial fail mid-handshake. Read c.state.activeIsPrimary under
	// switchMu to avoid racing with performSwitch → recordSwitch (a
	// concurrent ForceSwitch or controller tick can fire before Listen
	// finishes its own setup).
	e.switchMu.Lock()
	activeIsPrimary := e.ctrl.state.activeIsPrimary
	e.switchMu.Unlock()
	if activeIsPrimary {
		if ac, ok := e.secondary.(engine.AcceptController); ok {
			if err := ac.PauseAccept(); err != nil {
				innerCancel()
				wg.Wait()
				return fmt.Errorf("pause secondary: %w", err)
			}
		}
	} else {
		if ac, ok := e.primary.(engine.AcceptController); ok {
			if err := ac.PauseAccept(); err != nil {
				innerCancel()
				wg.Wait()
				return fmt.Errorf("pause primary: %w", err)
			}
		}
	}

	addr := e.primary.Addr()
	e.addr.Store(&addr)

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

	// Driver FDs are pinned to whichever sub-engine's worker they were
	// registered on — they cannot migrate across epoll ↔ io_uring. If any
	// driver has live FDs we refuse the switch rather than orphan them.
	// Hold freezeState while we (a) check the driver-FD count and
	// (b) commit the active.Store swap, so a concurrent acquireDriverFD
	// either observes the old active and registers on it before the
	// swap, or waits until after active.Store lands and registers on
	// the new active. We deliberately release freezeState BEFORE the
	// final PauseAccept on the old active — synchronous PauseAccept can
	// take O(ms) waiting for the loop to drain its listen queue, and
	// holding freezeState across that wait blocks driver
	// register/unregister flows long enough to trip their onClose
	// timeouts (regression seen in TestAdaptiveConcurrentDriverChurnVsSwitch).
	// Once active.Store has committed, no new driver registrations will
	// land on the about-to-be-paused engine, so it's safe to drop the
	// lock.
	e.freezeState.Lock()
	if e.driverFDs.Load() > 0 {
		e.switchRejected.Add(1)
		e.logger.Warn("refusing engine switch: driver FDs still registered",
			"driver_fds", e.driverFDs.Load(),
		)
		e.freezeState.Unlock()
		return
	}

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

	eng := newActive
	e.active.Store(&eng)
	e.switchMu.Lock()
	e.ctrl.recordSwitch(now)
	e.switchMu.Unlock()

	// Active has been committed — release freezeState so concurrent
	// driver acquireDriverFD calls observe the new active and proceed.
	e.freezeState.Unlock()

	// Pause the old active. Inline (not in a goroutine) so unit tests
	// observing pauseCalls right after performSwitch returns see the
	// effect; PauseAccept itself caps its wait to 2s, but the
	// freezeState release above means concurrent driver
	// register/unregister flows are no longer blocked while we wait.
	if ac, ok := newStandby.(engine.AcceptController); ok {
		_ = ac.PauseAccept()
	}

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
//
// FreezeSwitching is reference-counted: every call must be matched by a
// corresponding UnfreezeSwitching. The engine remains frozen until every
// external freeze has been released AND every driver-registered FD has been
// unregistered. This makes it safe for benchmarks and drivers to hold
// independent freezes without clobbering each other.
func (e *Engine) FreezeSwitching() {
	e.freezeState.Lock()
	e.userFreezes.Add(1)
	e.frozen.Store(true)
	e.freezeState.Unlock()
}

// UnfreezeSwitching releases one external freeze. The engine only becomes
// thawed when the external freeze count and the driver-FD count both reach
// zero. Calling UnfreezeSwitching more times than FreezeSwitching is a
// no-op (and does NOT unfreeze the engine if drivers still hold FDs).
func (e *Engine) UnfreezeSwitching() {
	e.freezeState.Lock()
	defer e.freezeState.Unlock()
	if e.userFreezes.Add(-1) < 0 {
		e.userFreezes.Store(0)
		return
	}
	if e.userFreezes.Load() == 0 && e.driverFDs.Load() == 0 {
		e.frozen.Store(false)
	}
}

// acquireDriverFD registers that a driver has attached a FD to the adaptive
// provider. While any driverFDs are live the engine is held frozen — a
// concurrent FreezeSwitching / UnfreezeSwitching can still run in parallel
// but the net frozen state only thaws when both counts reach zero.
func (e *Engine) acquireDriverFD() {
	e.freezeState.Lock()
	e.driverFDs.Add(1)
	e.frozen.Store(true)
	e.freezeState.Unlock()
}

// releaseDriverFD decrements the driver FD count. If no external freezes
// remain either, the engine is thawed.
func (e *Engine) releaseDriverFD() {
	e.freezeState.Lock()
	defer e.freezeState.Unlock()
	if e.driverFDs.Add(-1) < 0 {
		e.driverFDs.Store(0)
		return
	}
	if e.userFreezes.Load() == 0 && e.driverFDs.Load() == 0 {
		e.frozen.Store(false)
	}
}

// DriverFDCount reports the number of driver FDs currently registered on
// either sub-engine. Exposed for tests and observability.
func (e *Engine) DriverFDCount() int {
	return int(e.driverFDs.Load())
}

// SwitchRejectedCount reports how many engine-switch attempts were blocked
// by outstanding driver FDs since the engine started. Monotonic; useful for
// tests asserting that a switch actually happened (or did not).
func (e *Engine) SwitchRejectedCount() uint64 {
	return e.switchRejected.Load()
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
