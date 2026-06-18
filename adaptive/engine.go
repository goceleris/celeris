//go:build linux

package adaptive

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/engine/epoll"
	"github.com/goceleris/celeris/engine/iouring"
	"github.com/goceleris/celeris/probe"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

var (
	_ engine.Engine        = (*Engine)(nil)
	_ engine.SwitchFreezer = (*Engine)(nil)
)

// Engine is an adaptive meta-engine that switches between io_uring and epoll.
//
// The two sub-engine slots map to a fixed protocol direction the controller
// keys off: primary is ALWAYS the epoll engine (the controller's
// activeIsPrimary==true means epoll is active) and secondary is ALWAYS the
// io_uring engine. On the public New() path only the START engine is built
// eagerly; the other slot stays nil until the first switch actually needs it
// (see buildStandby + performSwitch). On a modern kernel that starts on
// io_uring and never reverts, the epoll standby is never constructed, so its
// GC-rooted heap never exists. newFromEngines (tests) populates BOTH slots
// eagerly, exercising the standby-already-exists switch path.
type Engine struct {
	primary   engine.Engine // epoll  (nil until built when it is the lazy standby)
	secondary engine.Engine // io_uring (nil until built when it is the lazy standby)
	active    atomic.Pointer[engine.Engine]
	ctrl      *controller
	cfg       resource.Config
	handler   stream.Handler
	addr      atomic.Pointer[net.Addr]
	mu        sync.Mutex
	switchMu  sync.Mutex // protects evaluate + performSwitch coordination
	frozen    atomic.Bool
	logger    *slog.Logger

	// startType is the engine type chosen for the eager start engine. The
	// standby is the other type; buildStandby constructs it on demand.
	startType engine.EngineType

	// buildStandby constructs the LAZY standby sub-engine on first switch.
	// It captures cfg + handler (+ cpuMon for the sampler symmetry) and is
	// nil on the newFromEngines (tests) path where both engines are eager.
	buildStandby func() (engine.Engine, error)

	// listenCtx / listenWG are captured by Listen so performSwitch can start a
	// freshly-built standby's Listen goroutine under the SAME context and wait
	// group as the active engine. Shutdown then joins it implicitly via the
	// wait group (wg.Wait in Listen) — a never-built standby added nothing to
	// the group, so there is nothing to join. Guarded by mu (performSwitch
	// holds mu across the whole switch; Listen sets these once under mu).
	listenCtx context.Context
	listenWG  *sync.WaitGroup

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

// chooseStartEngine selects which sub-engine the adaptive meta-engine should
// start (and build eagerly) given the probed io_uring capability profile.
//
// io_uring loses to epoll on old kernels (pre-LTS-stability bugs, missing the
// fast-path setup flags) but wins on modern ones for thin-HTTP, so the start
// engine is feature-gated:
//
//   - IOUring when io_uring is mature for thin-HTTP — either the kernel is in
//     the "bundles" era (>6.10, where multishot + provided buffers + defer
//     taskrun are all stable and tuned) OR the 6.1+ fast tier is present
//     (DEFER_TASKRUN + SINGLE_ISSUER + MULTISHOT_RECV + PROVIDED_BUFFERS).
//   - Epoll otherwise (old kernels, or io_uring missing the fast tier).
//
// The env var CELERIS_ADAPTIVE_START overrides the rule:
//
//	iouring | epoll — force that start engine.
//	auto (or unset) — use the capability rule above.
//
// NOTE: the exact capability rule will be refined by a kernel-matrix sweep;
// treat the thresholds here as the current best estimate, not a final answer.
func chooseStartEngine(p engine.CapabilityProfile) engine.EngineType {
	switch os.Getenv("CELERIS_ADAPTIVE_START") {
	case "iouring":
		return engine.IOUring
	case "epoll":
		return engine.Epoll
	case "auto", "":
		// fall through to the capability rule
	default:
		// Unknown value: fall through to auto rather than fail hard.
	}

	bundlesEra := p.KernelMajor > 6 || (p.KernelMajor == 6 && p.KernelMinor >= 10)
	fastTier := p.DeferTaskrun && p.SingleIssuer && p.MultishotRecv && p.ProvidedBuffers
	if bundlesEra || fastTier {
		return engine.IOUring
	}
	return engine.Epoll
}

// New creates a new adaptive engine. Only the START engine is built and
// Listen'd eagerly; the other engine (the standby) is constructed lazily on the
// first switch that actually needs it. The start engine is chosen by
// chooseStartEngine from the probed io_uring capabilities (feature-gated, with a
// CELERIS_ADAPTIVE_START env override).
//
// Both sub-engines bind the SAME SO_REUSEPORT port so the adaptive switch is
// transparent: resolvePort pins a concrete port up front, and the lazily-built
// standby reuses it. Building only the start engine eliminates the parked
// standby's GC-rooted heap — on a modern kernel that starts on io_uring and
// never reverts, the epoll standby is never constructed (≈0 standby tax).
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

	// probe.Probe() reads kernel version + io_uring setup feature bits WITHOUT
	// constructing an engine, so it is cheap enough for the start decision.
	startType := chooseStartEngine(probe.Probe())

	sampler := newLiveSampler(cpuMon)
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Constructors for each slot. The standby's constructor is stored on the
	// Engine and only invoked on the first switch. The io_uring constructor
	// does not take cpuMon (iouring.New has no such parameter); cpuMon already
	// feeds the shared sampler via newLiveSampler above.
	buildEpoll := func() (engine.Engine, error) {
		eng, err := epoll.New(cfg, handler)
		if err != nil {
			return nil, fmt.Errorf("epoll sub-engine: %w", err)
		}
		return eng, nil
	}
	buildIOUring := func() (engine.Engine, error) {
		eng, err := iouring.New(cfg, handler)
		if err != nil {
			return nil, fmt.Errorf("io_uring sub-engine: %w", err)
		}
		return eng, nil
	}

	e := &Engine{
		cfg:       cfg,
		handler:   handler,
		logger:    logger,
		startType: startType,
	}

	var startEngine engine.Engine
	if startType == engine.IOUring {
		// io_uring is the eager start; epoll is the lazy standby.
		// io_uring construction can fail on a kernel that probed as capable
		// but cannot actually set up the ring (e.g. low RLIMIT_MEMLOCK). Fall
		// back to starting on epoll rather than failing New outright.
		eng, err := buildIOUring()
		if err != nil {
			logger.Warn("io_uring start engine unavailable, falling back to epoll start", "error", err)
			startType = engine.Epoll
			e.startType = engine.Epoll
			eng, err = buildEpoll()
			if err != nil {
				return nil, err
			}
			startEngine = eng
			e.primary = eng
			e.buildStandby = buildIOUring
		} else {
			startEngine = eng
			e.secondary = eng
			e.buildStandby = buildEpoll
		}
	} else {
		// epoll is the eager start; io_uring is the lazy standby.
		eng, err := buildEpoll()
		if err != nil {
			return nil, err
		}
		startEngine = eng
		e.primary = eng
		e.buildStandby = buildIOUring
	}

	// The controller needs BOTH engine TYPES to decide switch direction even
	// while the standby engine is nil, but it only ever dereferences the
	// ACTIVE engine (activeEngine()). Pass the start engine for the active slot
	// and nil for the lazy standby slot — newController stores them; activeIsPrimary
	// records which slot the start engine occupies (primary==epoll).
	e.ctrl = newController(e.primary, e.secondary, sampler, logger)
	e.ctrl.state.activeIsPrimary = e.startType == engine.Epoll
	// Conns-per-worker UP/DOWN switching is OFF in production: the feature-gated
	// chooseStartEngine already selects the engine that is best at every
	// concurrency on this kernel, and (because pinned conns never migrate) the
	// down-revert would only fire on idle/warmup dips and strand load on the
	// wrong engine. The always-on error-revert in the controller is unaffected.
	// A future middle-tier kernel with a genuine crossover can flip this on
	// (validated by the kernel matrix).
	e.ctrl.connSwitchEnabled = false

	e.active.Store(&startEngine)
	return e, nil
}

// newFromEngines creates an adaptive engine from pre-built engines (for
// testing). BOTH slots are populated eagerly and buildStandby is left nil, so
// performSwitch exercises the "standby already exists" path (no lazy build).
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
		startType: engine.Epoll,
	}

	e.ctrl = newController(primary, secondary, sampler, logger)

	initialActive := primary
	e.ctrl.state.activeIsPrimary = true
	e.active.Store(&initialActive)

	return e
}

// Listen starts ONLY the active sub-engine and the evaluation loop. The standby
// is built and Listen'd lazily by performSwitch on the first switch (joined
// under the same ctx + wait group captured here).
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

	// Publish ctx + wg so performSwitch can launch the lazily-built standby's
	// Listen goroutine under the same lifetime (Shutdown joins it via wg.Wait).
	e.mu.Lock()
	e.listenCtx = innerCtx
	e.listenWG = &wg
	e.mu.Unlock()

	errCh := make(chan error, 2)

	active := *e.active.Load()
	wg.Go(func() {
		if err := active.Listen(innerCtx); err != nil {
			errCh <- fmt.Errorf("active (%s): %w", active.Type().String(), err)
		}
	})

	// Wait for the ACTIVE engine to bind its address.
	// io_uring may need multiple tier fallback attempts, so allow ample time —
	// but if the active sub-engine has already returned an error to errCh
	// (e.g. ENOMEM at io_uring_setup under low RLIMIT_MEMLOCK), surface it
	// immediately instead of waiting out the deadline.
	deadline := time.Now().Add(20 * time.Second)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	bindWait := time.NewTimer(time.Until(deadline))
	defer bindWait.Stop()
	var startErr error
bindLoop:
	for active.Addr() == nil {
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
	if active.Addr() == nil {
		innerCancel()
		wg.Wait()
		return fmt.Errorf("active sub-engine failed to initialize within 20s deadline")
	}

	// No standby to pause: only the active engine is in the SO_REUSEPORT group,
	// so publishing Addr cannot expose a dial to a phantom standby listener.
	// (The original pause-standby-before-publish-Addr step guarded that window;
	// with a lazy standby there is no standby listening here.)
	addr := active.Addr()
	e.addr.Store(&addr)

	e.logger.Info("adaptive engine listening",
		"addr", e.cfg.Addr,
		"active", active.Type().String(),
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

// buildAndStartStandby constructs the lazy standby sub-engine, launches its
// Listen goroutine under the same ctx + wait group Listen captured (so Shutdown
// joins it), and waits — bounded — for it to bind the shared SO_REUSEPORT port.
// The caller holds e.mu. wantType is purely for error messages. On any failure
// it returns an error and the engine state is left untouched (no slot stored),
// so the current active keeps serving.
func (e *Engine) buildAndStartStandby(wantType engine.EngineType) (engine.Engine, error) {
	if e.buildStandby == nil {
		return nil, fmt.Errorf("no standby builder for %s", wantType.String())
	}
	if e.listenCtx == nil || e.listenWG == nil {
		return nil, fmt.Errorf("cannot build standby before Listen has started")
	}

	built, err := e.buildStandby()
	if err != nil {
		return nil, fmt.Errorf("build %s standby: %w", wantType.String(), err)
	}

	ctx := e.listenCtx
	wg := e.listenWG
	wg.Go(func() {
		if lerr := built.Listen(ctx); lerr != nil {
			e.logger.Warn("lazy standby Listen returned error",
				"standby", built.Type().String(), "error", lerr)
		}
	})

	// Wait (bounded) for the standby to bind the shared port — once Addr() is
	// non-nil it has joined the SO_REUSEPORT group and is accepting, so the
	// resume-before-pause overlap is real and connections are never dropped.
	deadline := time.Now().Add(5 * time.Second)
	for built.Addr() == nil {
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, fmt.Errorf("%s standby failed to bind within 5s", wantType.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	return built, nil
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
	// Release freezeState across the (possibly slow) lazy standby build +
	// Listen + bind-wait below; re-acquired before the active.Store commit.
	// Holding it across a multi-second build would block driver
	// register/unregister flows (same reasoning as the PauseAccept release
	// at the end of this function). e.mu (held for the whole function)
	// already serialises performSwitch against itself, so no other switch
	// can race the build.
	e.freezeState.Unlock()

	now := time.Now()

	// Determine the direction. activeIsPrimary toggles on recordSwitch, so it
	// always reflects the engine we are switching AWAY from.
	e.switchMu.Lock()
	switchingFromPrimary := e.ctrl.state.activeIsPrimary
	e.switchMu.Unlock()

	// Resolve the standby slot for this direction. On the lazy New() path the
	// target slot may be nil and must be built + Listen'd now (it binds the
	// shared SO_REUSEPORT port and joins the accept pool). On the
	// newFromEngines (tests) path both slots are pre-populated and buildStandby
	// is nil, so the build is skipped.
	freshlyBuilt := false
	if switchingFromPrimary {
		// primary (active) → secondary (standby).
		if e.secondary == nil {
			built, err := e.buildAndStartStandby(engine.IOUring)
			if err != nil {
				e.logger.Warn("aborting switch: lazy standby build failed; staying on current active",
					"standby", engine.IOUring.String(), "error", err)
				return
			}
			// Publish the built engine to both the Engine and controller
			// slots under switchMu so a concurrent evaluate (ForceSwitch
			// racing the eval loop) never reads a torn controller slot.
			e.switchMu.Lock()
			e.secondary = built
			e.ctrl.secondary = built
			e.switchMu.Unlock()
			freshlyBuilt = true
		}
	} else {
		// secondary (active) → primary (standby).
		if e.primary == nil {
			built, err := e.buildAndStartStandby(engine.Epoll)
			if err != nil {
				e.logger.Warn("aborting switch: lazy standby build failed; staying on current active",
					"standby", engine.Epoll.String(), "error", err)
				return
			}
			e.switchMu.Lock()
			e.primary = built
			e.ctrl.primary = built
			e.switchMu.Unlock()
			freshlyBuilt = true
		}
	}

	var newActive, newStandby engine.Engine
	if switchingFromPrimary {
		newActive = e.secondary
		newStandby = e.primary
	} else {
		newActive = e.primary
		newStandby = e.secondary
	}

	// Re-acquire freezeState for the commit and RE-CHECK driverFDs: a driver
	// may have registered during the build window above. If so, abort — but the
	// freshly-built standby stays cached for the next attempt. Pause its accept
	// first so it does not sit in the SO_REUSEPORT pool alongside the (still
	// active) old engine; the next switch ResumeAccepts it.
	e.freezeState.Lock()
	if e.driverFDs.Load() > 0 {
		e.switchRejected.Add(1)
		e.logger.Warn("refusing engine switch: driver FDs registered during standby build",
			"driver_fds", e.driverFDs.Load(),
		)
		e.freezeState.Unlock()
		if freshlyBuilt {
			if ac, ok := newActive.(engine.AcceptController); ok {
				_ = ac.PauseAccept()
			}
		}
		return
	}

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

	// Only shut down engines that exist. On the lazy New() path the standby
	// slot is nil if no switch ever built it; cancelling listenCtx (above)
	// already unwound the active engine's Listen goroutine and any lazily
	// started standby Listen goroutine (both share that ctx + wait group).
	e.mu.Lock()
	primary := e.primary
	secondary := e.secondary
	e.mu.Unlock()

	var errs []error
	if primary != nil {
		errs = append(errs, primary.Shutdown(ctx))
	}
	if secondary != nil {
		errs = append(errs, secondary.Shutdown(ctx))
	}
	return errors.Join(errs...)
}

// Metrics aggregates metrics from whichever sub-engines exist. On the lazy
// New() path a never-built standby is nil and contributes nothing.
func (e *Engine) Metrics() engine.EngineMetrics {
	e.mu.Lock()
	primary := e.primary
	secondary := e.secondary
	e.mu.Unlock()

	var pm, sm engine.EngineMetrics
	if primary != nil {
		pm = primary.Metrics()
	}
	if secondary != nil {
		sm = secondary.Metrics()
	}
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
