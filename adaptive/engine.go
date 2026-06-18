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

	// listenMu guards listenCancel/listenDone, which let Shutdown deterministically
	// stop and JOIN the evaluation-loop goroutine started by Listen. Without
	// this, Shutdown could return (sub-engines stopped) while the eval loop is
	// still mid-Sample on the CPU monitor the server is about to close.
	listenMu     sync.Mutex
	listenCancel context.CancelFunc
	listenDone   chan struct{}

	// freezeState synchronises the three counters below. The counters are
	// atomic so read-only checks (performSwitch) stay lock-free, but any
	// mutation that may flip frozen must hold this mutex to avoid races
	// where two goroutines observe counters==0 and simultaneously transition
	// frozen in opposite directions.
	freezeState     sync.Mutex
	userFreezes     atomic.Int32  // calls to FreezeSwitching not yet matched by UnfreezeSwitching
	driverFDs       atomic.Int32  // driver FDs currently registered via the provider
	cooldownFreezes atomic.Int32  // post-switch cooldown timers currently holding the freeze
	switchRejected  atomic.Uint64 // telemetry: how many switches were blocked by driver FDs
}

// New creates a new adaptive engine with epoll as primary and io_uring as secondary.
// Epoll starts first because it has lower H2 latency on current kernels (single-pass
// read→process→write vs io_uring's two-iteration CQE model). The controller may
// switch to io_uring if telemetry indicates it would perform better for the workload.
// Both sub-engines get the full resource config. This is safe because standby
// workers are fully suspended (zero CPU, zero connections, listen sockets closed).
//
// cpuMon is an engine.CPUMonitor (the public interface); when non-nil it
// supplies the live sampler with CPU utilization data so the io_uring bias can
// fire in the empirical sweet spot. External callers can pass their own
// implementation or the built-in /proc/stat monitor. Pass nil for tests or
// when CPU monitoring is not available; the sampler degrades gracefully with
// CPUUtilization=0 in the snapshot.
func New(cfg resource.Config, handler stream.Handler, cpuMon engine.CPUMonitor) (*Engine, error) {
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

	sampler := newLiveSampler(cpuMon)
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

	// Publish the cancel + a done channel so Shutdown can stop and join the
	// goroutine this Listen owns (the eval loop) before the server closes
	// shared resources such as the CPU monitor.
	done := make(chan struct{})
	e.listenMu.Lock()
	e.listenCancel = innerCancel
	e.listenDone = done
	e.listenMu.Unlock()
	defer close(done)

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
	// io_uring may need multiple tier fallback attempts, so allow ample time —
	// but if either sub-engine has already returned an error to errCh
	// (e.g. ENOMEM at io_uring_setup under low RLIMIT_MEMLOCK), surface it
	// immediately instead of waiting out the deadline.
	deadline := time.Now().Add(20 * time.Second)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	bindWait := time.NewTimer(time.Until(deadline))
	defer bindWait.Stop()
	var startErr error
bindLoop:
	for e.primary.Addr() == nil || e.secondary.Addr() == nil {
		select {
		case startErr = <-errCh:
			break bindLoop
		case <-bindWait.C:
			break bindLoop
		case <-tick.C:
		}
	}

	if startErr != nil {
		innerCancel()
		wg.Wait()
		return fmt.Errorf("sub-engine startup failed: %w", startErr)
	}
	if e.primary.Addr() == nil || e.secondary.Addr() == nil {
		innerCancel()
		wg.Wait()
		return fmt.Errorf("sub-engines failed to initialize within 20s deadline")
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
	case <-innerCtx.Done():
		// Parent context cancelled, or Shutdown cancelled innerCtx directly to
		// stop and join the eval-loop goroutine.
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

	// Suppress further switches for the cooldown period. The cooldown is
	// tracked as its own freeze reason routed through freezeState so it
	// never clobbers a concurrent user or driver freeze: the thaw at the end
	// of the timer only clears frozen when userFreezes, driverFDs AND any
	// other in-flight cooldown timers have all reached zero. Two overlapping
	// switches therefore can't have one timer thaw while the other still
	// wants the gate held.
	if e.freezeCooldown > 0 {
		e.freezeState.Lock()
		e.cooldownFreezes.Add(1)
		e.frozen.Store(true)
		e.freezeState.Unlock()
		go func() {
			time.Sleep(e.freezeCooldown)
			e.freezeState.Lock()
			e.cooldownFreezes.Add(-1)
			e.maybeThawLocked()
			e.freezeState.Unlock()
		}()
	}
}

// maybeThawLocked clears the frozen gate only when no freeze reason remains —
// no external freezes, no live driver FDs, and no in-flight post-switch
// cooldown timers. Callers must hold freezeState. This is the single chokepoint
// that flips frozen false so independent freeze reasons never clobber each
// other.
func (e *Engine) maybeThawLocked() {
	if e.userFreezes.Load() == 0 && e.driverFDs.Load() == 0 && e.cooldownFreezes.Load() == 0 {
		e.frozen.Store(false)
	}
}

// Shutdown gracefully shuts down both sub-engines.
//
// It first cancels and JOINS the goroutine started by Listen (the evaluation
// loop), so that no controller tick can still be sampling
// telemetry — including the CPU monitor the server closes immediately after
// Shutdown returns — by the time this function completes. Only then are the
// sub-engines shut down. This is purely a join/sequencing concern; it does not
// touch the ACTIVE→DRAINING→SUSPENDED worker lifecycle.
func (e *Engine) Shutdown(ctx context.Context) error {
	e.listenMu.Lock()
	cancel := e.listenCancel
	done := e.listenDone
	e.listenMu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			// Honour the caller's deadline even if Listen is slow to unwind;
			// the ProcStat closed-flag still makes a late Sample safe.
		}
	}

	return errors.Join(
		e.primary.Shutdown(ctx),
		e.secondary.Shutdown(ctx),
	)
}

// Metrics aggregates metrics from both sub-engines.
func (e *Engine) Metrics() engine.EngineMetrics {
	pm := e.primary.Metrics()
	sm := e.secondary.Metrics()
	// Both sub-engines were built from the same handler + cfg, so their
	// AsyncRoutes counts are identical; take one (not the sum) for the
	// adaptive view. AsyncPromotedConns IS additive — promotions on the
	// old sub-engine during a switch still count.
	asyncRoutes := pm.AsyncRoutes
	if asyncRoutes == 0 {
		asyncRoutes = sm.AsyncRoutes
	}
	return engine.EngineMetrics{
		RequestCount:       pm.RequestCount + sm.RequestCount,
		ActiveConnections:  pm.ActiveConnections + sm.ActiveConnections,
		ErrorCount:         pm.ErrorCount + sm.ErrorCount,
		Throughput:         pm.Throughput + sm.Throughput,
		AsyncRoutes:        asyncRoutes,
		AsyncPromotedConns: pm.AsyncPromotedConns + sm.AsyncPromotedConns,
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
	e.maybeThawLocked()
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
	e.maybeThawLocked()
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
