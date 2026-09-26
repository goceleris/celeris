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
// (see buildStandby + performSwitch). Under the default policy the start engine
// is epoll, so the io_uring standby is built lazily — and only if a sustained
// high-concurrency ramp promotes new conns to it; an engine that never switches
// never constructs its standby, so that heap never exists. newFromEngines
// (tests) populates BOTH slots eagerly, exercising the standby-already-exists
// switch path.
//
// Lifecycle: the engine starts on epoll under the default policy and promotes
// NEW connections to io_uring once a sustained high-concurrency ramp develops
// (established connections are pinned and never migrate). Live switching is the
// most complex path in this package and has historically been the source of
// rare, hard-to-reproduce issues; the SwitchRejectedCount /
// EngineMetrics.AdaptiveSwitches counters exist so a throughput anomaly can be
// correlated with switching activity. CELERIS_ADAPTIVE_START=epoll|iouring
// chooses only the engine it STARTS on (see chooseStartEngine, the variable's
// only reader); the controller still switches afterwards. Operators who need
// fully deterministic behaviour should pin a single engine with Config.Engine
// (Epoll or IOUring) instead. For benchmarking, run the adaptive columns
// multiple times: a rare switch transient can skew a single pass.
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

	// standbyCancels holds the cancel func of each lazily built standby's child
	// context (celeris#656) — at most one per slot, so at most two for the life
	// of the engine. buildAndStartStandby cancels a standby it gives up on
	// itself and appends the cancel of one it hands back, so a standby that is
	// still running always has a handle here and Shutdown does not depend on
	// the Listen context alone to stop it. Guarded by mu, like the slots.
	standbyCancels []context.CancelFunc

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

	// switchesTotal is the monotonic count of SUCCESSFUL epoll⇄io_uring
	// switches (committed via active.Store). Rejected switches (driver FDs
	// live, aborted lazy build) never increment it. Surfaced on Metrics as
	// EngineMetrics.AdaptiveSwitches.
	switchesTotal atomic.Uint64
}

// ioUringViable reports whether io_uring is worth running at all on this host:
// the probed tier must be available at all, the kernel must expose the fast
// tier AND RLIMIT_MEMLOCK must be able to fund the requested worker count.
//
//   - Availability: iouring.New refuses to build an engine when the probed
//     IOUringTier is None ("io_uring not available on this system"), and the
//     probe reports None when CELERIS_MAX_IOURING_TIER caps it there (or when
//     io_uring is missing or blocked). The kernel-version test below cannot see
//     that: the cap clears the feature flags but not KernelMajor/KernelMinor, so
//     on a 6.10+ kernel the "bundles era" branch alone used to call io_uring
//     viable, and every promotion then failed to build it and backed off with
//     a WARN (celeris#679). Checked first, with iouring.New's own predicate, so
//     the two cannot disagree.
//
// The other two are the t0-knowable disqualifiers from the epoll-vs-io_uring
// sweep:
//
//   - Kernel/feature: io_uring loses to epoll on old kernels (missing the
//     fast-path setup flags); require the "bundles" era (>6.10) OR the 6.1+
//     fast tier (DEFER_TASKRUN + SINGLE_ISSUER + MULTISHOT_RECV + PROVIDED_BUFFERS).
//   - Memlock: io_uring's provided-buffer rings need locked pages per worker
//     (minMemlockPerWorker). If RLIMIT_MEMLOCK can't fund the requested workers,
//     io_uring caps to a fraction of them and its throughput collapses; epoll
//     does not memlock buffer rings, so it keeps all workers. In that case
//     io_uring is never the right engine.
func ioUringViable(p engine.CapabilityProfile, cfg resource.Config) bool {
	if !p.IOUringTier.Available() {
		return false
	}
	bundlesEra := p.KernelMajor > 6 || (p.KernelMajor == 6 && p.KernelMinor >= 10)
	fastTier := p.DeferTaskrun && p.SingleIssuer && p.MultishotRecv && p.ProvidedBuffers
	if !bundlesEra && !fastTier {
		return false
	}
	wantWorkers := cfg.Resources.Resolve().Workers
	if maxW := maxWorkersForMemlock(); maxW != -1 && maxW < wantWorkers {
		return false
	}
	return true
}

// maxWorkersForMemlock is the io_uring memlock worker-ceiling probe behind a var
// so tests can inject a low cap without mutating the process RLIMIT_MEMLOCK.
var maxWorkersForMemlock = iouring.MaxWorkersForMemlock

// chooseStartEngine selects which sub-engine the adaptive meta-engine starts
// (and builds eagerly), from facts knowable at Listen() time only.
//
// THE PINNING CONSTRAINT: an established connection cannot migrate between
// epoll and io_uring, so the START engine decides keep-alive throughput; the
// runtime switch can only route NEW connections. And the workload's
// concurrency — the thing that actually decides which engine wins — is
// unknowable here (no connections exist yet). So the start decision is gated
// only on t0-knowable disqualifiers, with a safe default:
//
//  1. env override CELERIS_ADAPTIVE_START=iouring|epoll (operator escape hatch).
//  2. io_uring not viable (old kernel / missing fast tier / memlock too low) → epoll.
//  3. configured Protocol == H2C → epoll (io_uring's win is h1-small-payload only;
//     h2c never benefits — its framing/HPACK cost dwarfs the engine delta).
//  4. explicit operator WorkloadHint == HighConcurrency → io_uring (the ONLY
//     input that can express a high-concurrency expectation up front).
//  5. DEFAULT → epoll. Every server ramps from zero connections, i.e. the
//     low-concurrency regime where epoll wins on throughput AND tail latency;
//     the runtime switch then promotes new conns to io_uring if sustained
//     high load develops.
//
// This flips the previous default (io_uring on modern kernels): io_uring now
// wins the start only on an explicit high-concurrency hint, because the
// benchmark-shaped "saturating burst at t0" is the only case where defaulting
// io_uring helps, and it costs the common low/mid-conc + latency cases.
func chooseStartEngine(p engine.CapabilityProfile, cfg resource.Config) engine.EngineType {
	switch os.Getenv("CELERIS_ADAPTIVE_START") {
	case "iouring":
		return engine.IOUring
	case "epoll":
		return engine.Epoll
	case "auto", "":
		// fall through to the policy below
	default:
		// Unknown value: fall through to auto rather than fail hard.
	}

	if !ioUringViable(p, cfg) {
		return engine.Epoll
	}
	if cfg.Protocol == engine.H2C {
		return engine.Epoll
	}
	if cfg.Resources.WorkloadHint == resource.WorkloadHighConcurrency {
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
// transparent: the address is pinned to a concrete host:port up front and the
// lazily-built standby reuses it. Building only the start engine eliminates
// the parked standby's GC-rooted heap — on a modern kernel that starts on
// io_uring and never reverts, the epoll standby is never constructed (≈0
// standby tax).
//
// Pre-bound listeners (Server.StartWithListener, socket activation, graceful
// restart) are handed to the START sub-engine, which owns them: epoll and
// io_uring both close the supplied listener in Listen and rebind their own
// SO_REUSEPORT sockets on its address. The standby is built later, from the
// address alone, and joins that group. See the address-resolution block below.
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

	// Both sub-engines must serve the SAME concrete host:port — the switch is
	// only transparent because the standby joins the active engine's
	// SO_REUSEPORT group on that exact address — and each sub-engine's every
	// worker binds cfg.Addr independently (epoll.createListenSocket per loop,
	// io_uring per worker), so a ":0" that reached them would scatter workers
	// across different ephemeral ports. The address therefore has to be
	// decided here, and there are two mutually exclusive ways to decide it.
	//
	// A pre-bound Listener has ALREADY decided it. Running resolvePort in that
	// case invented a SECOND, different port, wrote it into cfg.Addr, and the
	// sub-engine constructor then rejected the pair it had just been handed
	// ("ambiguous configuration: Addr=... but Listener is bound to ...") — so
	// the default engine could not start via Server.StartWithListener at all
	// (celeris#614). The listener is the source of truth; resolvePort must not
	// run.
	switch {
	case cfg.Listener != nil:
		lnAddr, err := reusePortAddr(cfg.Listener)
		if err != nil {
			return nil, err
		}
		cfg.Addr = lnAddr
	case cfg.Addr != "":
		resolved, err := resolvePort(cfg.Addr)
		if err == nil {
			cfg.Addr = resolved
		}
	}

	// probe.Probe() reads kernel version + io_uring setup feature bits WITHOUT
	// constructing an engine, so it is cheap enough for the start decision.
	profile := probe.Probe()
	startType := chooseStartEngine(profile, cfg)

	sampler := newLiveSampler(cpuMon)
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Only ONE listener ever arrives, so only ONE sub-engine may receive it:
	// the START engine, which consumes it in Listen (closes it and rebinds its
	// own SO_REUSEPORT sockets on the same address, making it the group's
	// first member). The standby is built later — possibly minutes later, on
	// the first switch — and binds by address, joining the group the start
	// engine created.
	//
	// Clearing it for the standby is deliberate, not incidental. A *net.TCPListener
	// happens to survive the alternative (its Addr() still answers after Close
	// and a second Close is a no-op error), but that is an accident of the
	// stdlib type, and net.Listener is an interface: a socket-activation or
	// inherited-fd wrapper is under no obligation to keep answering Addr()
	// after it has been closed, and the standby would be asking it minutes
	// after the start engine closed it. The standby has the address it needs;
	// it must not reach for a socket that is not its own.
	//
	// This is also why the caller's listener does NOT need SO_REUSEPORT set:
	// it is never in the group. It is closed before the start engine binds,
	// and the sockets that form the group are all created by the sub-engines
	// with SO_REUSEPORT (see epoll/iouring createListenSocket). The only
	// listener shape adaptive genuinely cannot serve is a non-TCP one, and
	// reusePortAddr above rejects that at New() rather than at switch time.
	startCfg := cfg
	standbyCfg := cfg
	standbyCfg.Listener = nil

	// Constructors for each slot. The standby's constructor is stored on the
	// Engine and only invoked on the first switch. The io_uring constructor
	// does not take cpuMon (iouring.New has no such parameter); cpuMon already
	// feeds the shared sampler via newLiveSampler above.
	buildEpoll := func(c resource.Config) (engine.Engine, error) {
		eng, err := epoll.New(c, handler)
		if err != nil {
			return nil, fmt.Errorf("epoll sub-engine: %w", err)
		}
		return eng, nil
	}
	buildIOUring := func(c resource.Config) (engine.Engine, error) {
		eng, err := iouring.New(c, handler)
		if err != nil {
			return nil, fmt.Errorf("io_uring sub-engine: %w", err)
		}
		return eng, nil
	}

	e := &Engine{
		// standbyCfg, not startCfg: cfg is only read for logging, and holding
		// the listener here would pin a closed socket for the engine's life.
		cfg:       standbyCfg,
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
		eng, err := buildIOUring(startCfg)
		if err != nil {
			// iouring.New failed before Listen, so it never touched the
			// listener: epoll still gets it as the start engine.
			logger.Warn("io_uring start engine unavailable, falling back to epoll start", "error", err)
			e.startType = engine.Epoll
			eng, err = buildEpoll(startCfg)
			if err != nil {
				return nil, err
			}
			startEngine = eng
			e.primary = eng
			e.buildStandby = func() (engine.Engine, error) { return buildIOUring(standbyCfg) }
		} else {
			startEngine = eng
			e.secondary = eng
			e.buildStandby = func() (engine.Engine, error) { return buildEpoll(standbyCfg) }
		}
	} else {
		// epoll is the eager start; io_uring is the lazy standby.
		eng, err := buildEpoll(startCfg)
		if err != nil {
			return nil, err
		}
		startEngine = eng
		e.primary = eng
		e.buildStandby = func() (engine.Engine, error) { return buildIOUring(standbyCfg) }
	}

	// The controller needs BOTH engine TYPES to decide switch direction even
	// while the standby engine is nil, but it only ever dereferences the
	// ACTIVE engine (activeEngine()). Pass the start engine for the active slot
	// and nil for the lazy standby slot — newController stores them; activeIsPrimary
	// records which slot the start engine occupies (primary==epoll).
	e.ctrl = newController(e.primary, e.secondary, sampler, logger)
	e.ctrl.state.activeIsPrimary = e.startType == engine.Epoll
	// Re-enable the conns-per-worker UP switch ONLY on the epoll-start path with
	// io_uring viable and a non-h2c protocol. Rationale from the sweep:
	//   - When we START on epoll (the new default), a sustained high-concurrency
	//     ramp should promote NEW connections to io_uring (it wins ≥~24 conns/
	//     worker for h1 small payloads). The switch routes new SYNs only —
	//     pinned conns stay on epoll — so it helps ramps/churn, and is inert for
	//     a pure keep-alive burst (which is fine; that case wants WorkloadHint).
	//   - When we START on io_uring there is nothing better to switch UP to, and
	//     a load-driven DOWN-revert would only strand pinned io_uring conns — so
	//     leave switching OFF there (the always-on error-revert still applies).
	//   - h2c never benefits from io_uring, so never switch up for it.
	// The controller's load-driven DOWN-revert is disabled regardless (pinning);
	// only the always-on io_uring error-revert can move us back to epoll.
	e.ctrl.connSwitchEnabled = e.startType == engine.Epoll &&
		ioUringViable(profile, cfg) &&
		cfg.Protocol != engine.H2C
	// Load-driven down-revert is always off in production (pinning makes it
	// harmful); only the always-on io_uring error-revert can return us to epoll.
	e.ctrl.loadDownRevert = false

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
	var stopped bool
bindLoop:
	for active.Addr() == nil {
		select {
		case startErr = <-errCh:
			break bindLoop
		case <-innerCtx.Done():
			// Shutdown, or the caller's context, ended the engine before the
			// active sub-engine published an address. Without this case the
			// wait runs to its own 20s deadline no matter what: Server.Shutdown
			// cancels this context and then JOINS this Listen, so a Listen
			// parked here burns the caller's whole shutdown deadline and then
			// keeps Start parked for the rest of the 20s — the celeris#595
			// contract, broken on the one path that fix did not reach
			// (celeris#638).
			stopped = true
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
	if stopped {
		// An ordinary stop, not a startup failure: unwind and report no
		// error, exactly as a Listen that had finished starting would.
		innerCancel()
		wg.Wait()
		return nil
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
			if adaptiveDebugEnabled {
				e.logTickDebug(now, shouldSwitch)
			}
			e.switchMu.Unlock()
			if shouldSwitch {
				e.performSwitch()
			}
		}
	}
}

// adaptiveDebugEnabled gates the per-tick controller trace added for issue #396
// (Adaptive never promotes to io_uring at 1024c). Set CELERIS_ADAPTIVE_DEBUG=1
// to make every eval tick's gate state directly observable — frozen / cooldown
// / oscillation-lock / conns-per-worker / up-ticks — instead of inferred from
// the rps-only timeseries. Off by default = zero production overhead.
var adaptiveDebugEnabled = os.Getenv("CELERIS_ADAPTIVE_DEBUG") != ""

// logTickDebug emits the full per-tick decision context. The caller holds
// switchMu, so the controller-state reads below are consistent with the
// evaluate() call that just ran on the same tick.
func (e *Engine) logTickDebug(now time.Time, shouldSwitch bool) {
	act := e.ctrl.activeEngine()
	m := act.Metrics()
	cpw := float64(m.ActiveConnections) / float64(max(m.Workers, 1))
	cdRemain := time.Duration(0)
	if !e.ctrl.state.lastSwitch.IsZero() {
		if d := e.ctrl.cooldown - now.Sub(e.ctrl.state.lastSwitch); d > 0 {
			cdRemain = d
		}
	}
	e.ctrl.logger.Info("adaptive_tick",
		"active", act.Type().String(),
		"frozen", e.frozen.Load(),
		"driver_fds", e.driverFDs.Load(),
		"cooldown_freezes", e.cooldownFreezes.Load(),
		"cooldown_remaining", cdRemain.String(),
		"locked", e.ctrl.state.locked,
		"active_conns", m.ActiveConnections,
		"workers", m.Workers,
		"cpw", cpw,
		"bytes_total", m.BytesRead+m.BytesWritten,
		"up_ticks", e.ctrl.state.upTicks,
		"down_ticks", e.ctrl.state.downTicks,
		"standby_build_failures", e.ctrl.state.buildFailures,
		"standby_build_retry_in", max(e.ctrl.state.buildRetryAt.Sub(now), 0).String(),
		"should_switch", shouldSwitch,
	)
}

// buildAndStartStandby constructs the lazy standby sub-engine, launches its
// Listen goroutine under a child of the ctx Listen captured and in the same
// wait group (so Shutdown joins it), and waits — bounded — for it to bind the
// shared SO_REUSEPORT port. The caller holds e.mu. wantType is purely for error
// messages. On any failure it returns an error, stops the standby it started,
// and leaves the engine state untouched (no slot stored), so the current
// active keeps serving.
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

	// celeris#656: the standby gets its own child context so that one this
	// function gives up on is stopped. It used to run under the Listen context
	// itself, so a standby that bound after the deadline below kept running:
	// in the SO_REUSEPORT group, accepting, and in no slot, where no later
	// switch would pause it and Shutdown would never name it. A standby that is
	// returned keeps running, and its cancel is kept in e.standbyCancels (the
	// caller holds mu) so Shutdown can stop it by name.
	ctx, cancel := context.WithCancel(e.listenCtx)
	started := false
	defer func() {
		if !started {
			cancel()
		}
	}()
	wg := e.listenWG
	listenErr := make(chan error, 1)
	wg.Go(func() {
		lerr := built.Listen(ctx)
		if lerr != nil {
			e.logger.Warn("lazy standby Listen returned error",
				"standby", built.Type().String(), "error", lerr)
		}
		listenErr <- lerr
	})

	// Wait (bounded) for the standby to bind the shared port — once Addr() is
	// non-nil it has joined the SO_REUSEPORT group and is accepting, so the
	// resume-before-pause overlap is real and connections are never dropped.
	//
	// celeris#656: a Listen that returns has failed to start, and it will
	// never publish an address. The wait used to ignore that and poll Addr()
	// for the full 5 s regardless, holding e.mu, on every retry of the switch.
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for built.Addr() == nil {
		select {
		case lerr := <-listenErr:
			if lerr == nil {
				lerr = errors.New("Listen returned before binding")
			}
			return nil, fmt.Errorf("%s standby failed to start: %w", wantType.String(), lerr)
		case <-ctx.Done():
			return nil, fmt.Errorf("%s standby stopped before it bound: %w", wantType.String(), ctx.Err())
		case <-deadline.C:
			return nil, fmt.Errorf("%s standby failed to bind within 5s", wantType.String())
		case <-tick.C:
		}
	}
	started = true
	e.standbyCancels = append(e.standbyCancels, cancel)
	return built, nil
}

// abortStandbyBuild records a failed lazy standby build so the controller backs
// off before recommending the switch again, and logs the abandoned switch.
//
// celeris#656: this path used to record nothing. The load that triggered the
// switch was still there on the next tick, so the controller recommended the
// same build again, and every attempt that failed the way an io_uring engine
// with a failing worker does put that engine's sockets on the serving port
// again. The backoff is kept apart from recordSwitch: nothing switched, so
// activeIsPrimary, the cooldown and the oscillation lock must not move.
// ForceSwitch calls performSwitch directly and is not held back by it.
func (e *Engine) abortStandbyBuild(standby engine.EngineType, err error) {
	e.switchMu.Lock()
	retryIn := e.ctrl.recordStandbyBuildFailure(time.Now())
	failures := e.ctrl.state.buildFailures
	e.switchMu.Unlock()
	e.logger.Warn("aborting switch: lazy standby build failed; staying on current active",
		"standby", standby.String(), "error", err,
		"consecutive_failures", failures, "retry_in", retryIn.String())
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
				e.abortStandbyBuild(engine.IOUring, err)
				return
			}
			// Publish the built engine to both the Engine and controller
			// slots under switchMu so a concurrent evaluate (ForceSwitch
			// racing the eval loop) never reads a torn controller slot.
			e.switchMu.Lock()
			e.secondary = built
			e.ctrl.secondary = built
			e.ctrl.recordStandbyBuilt()
			e.switchMu.Unlock()
			freshlyBuilt = true
		}
	} else {
		// secondary (active) → primary (standby).
		if e.primary == nil {
			built, err := e.buildAndStartStandby(engine.Epoll)
			if err != nil {
				e.abortStandbyBuild(engine.Epoll, err)
				return
			}
			e.switchMu.Lock()
			e.primary = built
			e.ctrl.primary = built
			e.ctrl.recordStandbyBuilt()
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
	// celeris#657 P11 (B0). The engine about to become active may still
	// carry the drain it was given as a SOURCE at the previous switch,
	// pointing at the engine this switch is about to make standby. Stop it
	// FIRST: between ResumeAccept here and applyTransplant below, every
	// connection this engine accepts is at a clean HTTP/1 boundary the
	// moment it has answered its first request, so the stale drain hands it
	// straight to the engine being switched away from — measured as a
	// ping-pong of 80 connections in 1 of 150 switches. applyTransplant
	// keeps its own StopTransplant, which is idempotent.
	if src, ok := newActive.(interface{ StopTransplant() }); ok {
		src.StopTransplant()
	}
	if ac, ok := newActive.(engine.AcceptController); ok {
		_ = ac.ResumeAccept()
	}

	eng := newActive
	e.active.Store(&eng)
	e.switchesTotal.Add(1)
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

	// #383: when promoting to io_uring, drain epoll's pinned keep-alives onto it
	// so they actually benefit from io_uring instead of being stranded on the
	// now-standby epoll — the hollow-promotion forfeit measured in #396. Only
	// HTTP/1 conns at a clean, flushed request boundary are moved (see
	// epoll.tryTransplant); H2/h2c/mid-upgrade/detached/driver conns are never
	// touched, so this is safe to run unconditionally.
	if switchWindowHook != nil {
		switchWindowHook(newActive, newStandby)
	}
	e.applyTransplant(newActive, newStandby)

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
	standbyCancels := e.standbyCancels
	e.standbyCancels = nil
	e.mu.Unlock()

	// celeris#656: stop the lazily built standbys by name. Cancelling the Listen
	// context above already unwinds them — their contexts are its children — but
	// holding the handle means a standby this engine started is never reachable
	// only through a context someone else owns.
	for _, cancel := range standbyCancels {
		cancel()
	}

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
	// The STANDBY's share of the two connection-lifecycle aggregates
	// (celeris#624). ActiveConnections and CloseCount stay sums — the sum
	// is the public contract and the controller divides it by Workers —
	// but a promotion leaves every pre-switch keep-alive pinned on the
	// standby until the transplant drain moves it, so only the split says
	// which sub-engine a live-gauge step came from. Read from the active
	// pointer rather than the controller's activeIsPrimary, which is
	// switchMu-guarded; a switch concurrent with this call attributes the
	// halves to the other side for one sample, which no cumulative or
	// gauge value depends on. Zero while the lazy standby is unbuilt.
	standby := standbyMetrics(e.active.Load(), primary, secondary, pm, sm)
	// EVERY field below is listed in the order [engine.EngineMetrics]
	// declares them, and every new field must be added here too. This
	// literal silently dropped ten of them (celeris#627): a field absent
	// from it is not "inherited", it is reported as zero, and the adaptive
	// column of nightly 34893230678 duly published engine_workers=0,
	// bytes_read=0 and bytes_written=0 on a cell serving 101 live
	// connections. TestMetricsCarriesEveryFieldReflectively enforces the
	// rule without naming fields, so the next one added cannot be dropped
	// the same way.
	return engine.EngineMetrics{
		RequestCount:      pm.RequestCount + sm.RequestCount,
		ActiveConnections: pm.ActiveConnections + sm.ActiveConnections,
		ErrorCount:        pm.ErrorCount + sm.ErrorCount,
		// The celeris#645 cause split. Every bucket is cumulative and
		// engine-local, so every one of them adds — and because each
		// sub-engine derives its own ErrorCount as the sum of its
		// buckets, summing the buckets here keeps the adaptive total
		// equal to the adaptive parts as well.
		ErrorAcceptFDLimit:    pm.ErrorAcceptFDLimit + sm.ErrorAcceptFDLimit,
		ErrorAcceptCancelled:  pm.ErrorAcceptCancelled + sm.ErrorAcceptCancelled,
		ErrorAcceptOther:      pm.ErrorAcceptOther + sm.ErrorAcceptOther,
		ErrorConnTableCap:     pm.ErrorConnTableCap + sm.ErrorConnTableCap,
		ErrorConnRegister:     pm.ErrorConnRegister + sm.ErrorConnRegister,
		ErrorListenerRecreate: pm.ErrorListenerRecreate + sm.ErrorListenerRecreate,
		ErrorTransplantAdopt:  pm.ErrorTransplantAdopt + sm.ErrorTransplantAdopt,
		ErrorSendPeerGone:     pm.ErrorSendPeerGone + sm.ErrorSendPeerGone,
		ErrorSend:             pm.ErrorSend + sm.ErrorSend,
		ErrorRequestBody:      pm.ErrorRequestBody + sm.ErrorRequestBody,
		ErrorHandler:          pm.ErrorHandler + sm.ErrorHandler,
		Throughput:            pm.Throughput + sm.Throughput,
		AsyncRoutes:           asyncRoutes,
		AsyncPromotedConns:    pm.AsyncPromotedConns + sm.AsyncPromotedConns,
		// Workers is summed, not taken from the active sub-engine: both
		// exist simultaneously (the standby keeps its loops up and keeps
		// serving its pinned keep-alives), so the sum is the divisor that
		// matches the summed ActiveConnections above. It is 0 before Listen
		// and while the lazy standby is unbuilt contributes nothing.
		Workers: pm.Workers + sm.Workers,
		// Cumulative connection-lifecycle and byte totals, additive across
		// a switch exactly like AsyncPromotedConns: each event is
		// attributed to whichever sub-engine owned the connection at the
		// time. CloseCount in particular is what makes engine_closed vs
		// hook_closed comparable on the only engine that can lose a
		// connection to a hand-off (celeris#624).
		AcceptCount:  pm.AcceptCount + sm.AcceptCount,
		CloseCount:   pm.CloseCount + sm.CloseCount,
		BytesRead:    pm.BytesRead + sm.BytesRead,
		BytesWritten: pm.BytesWritten + sm.BytesWritten,
		// The adaptive engine's own counter, not the sub-engines' (neither
		// of them switches).
		AdaptiveSwitches: e.switchesTotal.Load(),
		// The celeris#586 recv-arming witnesses. io_uring-only and
		// cumulative. RecvDoubleArmed and RecvCQEUnaccounted are
		// must-stay-zero defect witnesses, so dropping them here made the
		// adaptive engine report "clean" unconditionally (celeris#627).
		RecvResumeWhileCancelPending: pm.RecvResumeWhileCancelPending + sm.RecvResumeWhileCancelPending,
		RecvResumeWhileRecvInFlight:  pm.RecvResumeWhileRecvInFlight + sm.RecvResumeWhileRecvInFlight,
		RecvArmDeclined:              pm.RecvArmDeclined + sm.RecvArmDeclined,
		RecvDoubleArmed:              pm.RecvDoubleArmed + sm.RecvDoubleArmed,
		RecvCQEUnaccounted:           pm.RecvCQEUnaccounted + sm.RecvCQEUnaccounted,
		// Both are io_uring-only and additive: a detached conn lives on
		// exactly one sub-engine, and window closes are cumulative events.
		DetachedConnections: pm.DetachedConnections + sm.DetachedConnections,
		DetachWindowCloses:  pm.DetachWindowCloses + sm.DetachWindowCloses,
		// io_uring-only and cumulative, so additive across a switch: the
		// epoll sub-engine contributes zero and a ZC send / notif / byte
		// is attributed to whichever sub-engine issued it (celeris#591).
		ZCSendsSubmitted: pm.ZCSendsSubmitted + sm.ZCSendsSubmitted,
		ZCNotifs:         pm.ZCNotifs + sm.ZCNotifs,
		InlineBytes:      pm.InlineBytes + sm.InlineBytes,
		RingBytes:        pm.RingBytes + sm.RingBytes,
		// The standby's share — the only fields here that are NOT sums
		// (celeris#624, celeris#645).
		StandbyActiveConnections: standby.ActiveConnections,
		StandbyCloseCount:        standby.CloseCount,
		// The third of the deliberately-one-sided fields (celeris#645).
		// The buckets above say what went wrong; this says which
		// sub-engine it went wrong on, and only the pair can tell a
		// standby that is losing accepts at the promotion from a
		// promoted engine that is failing sends.
		StandbyErrorCount: standby.ErrorCount,
		// The #383 hand-off ledger. Both halves of a transplant are
		// cumulative and land on opposite sub-engines, so summing them is
		// what makes TransplantDetached - TransplantAdopted the count of
		// conns currently in flight between the two (celeris#624).
		TransplantAdopted:           pm.TransplantAdopted + sm.TransplantAdopted,
		TransplantDetached:          pm.TransplantDetached + sm.TransplantDetached,
		TransplantAdoptSlotOccupied: pm.TransplantAdoptSlotOccupied + sm.TransplantAdoptSlotOccupied,
		// The four silent drop points the residual above used to hide.
		// Cumulative, one per lost-or-recovered hand-off, and a hand-off
		// has exactly one source and one target, so summing the pair is
		// the count of events — never a double count (celeris#624).
		TransplantHandoffRefused: pm.TransplantHandoffRefused + sm.TransplantHandoffRefused,
		TransplantDrainStopped:   pm.TransplantDrainStopped + sm.TransplantDrainStopped,
		TransplantStranded:       pm.TransplantStranded + sm.TransplantStranded,
		TransplantAdoptRefused:   pm.TransplantAdoptRefused + sm.TransplantAdoptRefused,
		CloseMissingConnState:    pm.CloseMissingConnState + sm.CloseMissingConnState,
		// The celeris#657 hand-off loss witnesses. io_uring-only and
		// cumulative, and each event (a stale data CQE, a hand-off) happens
		// on exactly one sub-engine, so the sum counts events once. After a
		// switch the stale CQEs keep arriving on the sub-engine that made
		// the hand-off, which is now the standby: dropping the standby's
		// half would hide exactly the loss these exist to report.
		StaleRecvDataClosed:       pm.StaleRecvDataClosed + sm.StaleRecvDataClosed,
		StaleRecvDataTransplanted: pm.StaleRecvDataTransplanted + sm.StaleRecvDataTransplanted,
		StaleRecvDataUnattributed: pm.StaleRecvDataUnattributed + sm.StaleRecvDataUnattributed,
		TransplantHandoffInFlight: pm.TransplantHandoffInFlight + sm.TransplantHandoffInFlight,
		// The fd-lifetime rule's counters (celeris#657 PR-2), io_uring-only
		// and cumulative. A hold, a reap and a refused double claim each
		// happen on the one sub-engine making the hand-off, so the sum
		// counts each once; like the witnesses above, the standby's half
		// is where a revert's hand-offs are made.
		TransplantHeld:        pm.TransplantHeld + sm.TransplantHeld,
		TransplantReaps:       pm.TransplantReaps + sm.TransplantReaps,
		TransplantReapMisses:  pm.TransplantReapMisses + sm.TransplantReapMisses,
		TransplantHoldRescued: pm.TransplantHoldRescued + sm.TransplantHoldRescued,
		TransplantDoubleClaim: pm.TransplantDoubleClaim + sm.TransplantDoubleClaim,
		// The same rule for the refusals and fallbacks around them: each
		// is an event on the one sub-engine attempting the hand-off.
		TransplantClaimDeferred:   pm.TransplantClaimDeferred + sm.TransplantClaimDeferred,
		TransplantReapFailed:      pm.TransplantReapFailed + sm.TransplantReapFailed,
		TransplantReapUnsupported: pm.TransplantReapUnsupported + sm.TransplantReapUnsupported,
		// The post-switch sweep (celeris#657 PR-3). Both sub-engines sweep,
		// in opposite directions, and only the one draining runs passes at
		// all, so the pass count sums as a rate. The residual entries are
		// GAUGES, and the sum is the whole adaptive engine's residue: after
		// a switch settles, the outgoing side's is what did not follow it
		// and the incoming side's is zero.
		TransplantSweepPasses:       pm.TransplantSweepPasses + sm.TransplantSweepPasses,
		TransplantResidualDetached:  pm.TransplantResidualDetached + sm.TransplantResidualDetached,
		TransplantResidualH2:        pm.TransplantResidualH2 + sm.TransplantResidualH2,
		TransplantResidualPinned:    pm.TransplantResidualPinned + sm.TransplantResidualPinned,
		TransplantResidualUnstarted: pm.TransplantResidualUnstarted + sm.TransplantResidualUnstarted,
		TransplantResidualBusy:      pm.TransplantResidualBusy + sm.TransplantResidualBusy,

		// The celeris#607 recv-stall and linked-recv ledger. io_uring-only,
		// so the epoll half contributes zero and a switch simply moves which
		// side is counting.
		//
		// Counts and total durations are cumulative, so they add. The two
		// *MaxNanos fields are NOT: each is the longest SINGLE episode on its
		// sub-engine, and adding two maxima would manufacture an episode
		// nothing observed. That distinction matters here more than most,
		// because #607's whole discriminator is that an episode measured in
		// SECONDS means a connection received nothing while its peer's bytes
		// sat unread — summing two sub-second maxima into a seconds-long one
		// would fabricate exactly the signal the field exists to detect.
		RecvSQFull:                pm.RecvSQFull + sm.RecvSQFull,
		RecvStallEpisodes:         pm.RecvStallEpisodes + sm.RecvStallEpisodes,
		RecvStallNanos:            pm.RecvStallNanos + sm.RecvStallNanos,
		RecvStallMaxNanos:         max(pm.RecvStallMaxNanos, sm.RecvStallMaxNanos),
		RecvLinkedArms:            pm.RecvLinkedArms + sm.RecvLinkedArms,
		RecvLinkedBlockedNanos:    pm.RecvLinkedBlockedNanos + sm.RecvLinkedBlockedNanos,
		RecvLinkedBlockedMaxNanos: max(pm.RecvLinkedBlockedMaxNanos, sm.RecvLinkedBlockedMaxNanos),
	}
}

// standbyMetrics returns the snapshot belonging to the sub-engine that is NOT
// currently active (celeris#624). active is the pointer published by e.active;
// pm and sm are the snapshots already taken from primary and secondary, so the
// split costs no second Metrics() call and no second round of atomic loads —
// and both halves are guaranteed to come from the same pair of snapshots the
// sums were computed from.
//
// Returns the zero snapshot when the active slot is unpublished or matches
// neither sub-engine, which is also what an unbuilt lazy standby yields: a
// standby that does not exist holds no connections.
func standbyMetrics(active *engine.Engine, primary, secondary engine.Engine,
	pm, sm engine.EngineMetrics) engine.EngineMetrics {
	if active == nil {
		return engine.EngineMetrics{}
	}
	switch *active {
	case primary:
		return sm
	case secondary:
		return pm
	}
	return engine.EngineMetrics{}
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

// reusePortAddr returns the address a caller-supplied listener owns, and is
// the ONLY source of the bind address on the pre-bound-listener path.
//
// It also fails EARLY — at New(), not at the first switch — for a listener
// whose address the two sub-engines cannot both bind. The adaptive engine
// needs to put two independent sets of SO_REUSEPORT sockets on one address,
// which is a TCP-only arrangement; a Unix-socket listener would start fine on
// the start engine and only blow up later, as a 5-second bind timeout inside
// buildAndStartStandby on the first promotion. Refusing it here turns a latent
// switch-time failure into a startup error the caller can act on.
func reusePortAddr(ln net.Listener) (string, error) {
	a := ln.Addr()
	if a == nil {
		return "", fmt.Errorf("supplied Listener has no address")
	}
	switch a.Network() {
	case "tcp", "tcp4", "tcp6":
		return a.String(), nil
	default:
		return "", fmt.Errorf(
			"adaptive engine needs a TCP listener, got %s listener %q: both sub-engines must bind the same address with SO_REUSEPORT so the switch is transparent; use the std engine for this listener",
			a.Network(), a.String())
	}
}

// resolvePort resolves ":0" to a concrete ":PORT" by briefly binding a
// listener. Both sub-engines need the same port for SO_REUSEPORT switching,
// and each of their workers binds cfg.Addr independently, so the port cannot
// be left at 0.
//
// This is a time-of-check-to-time-of-use window: the port is bound, closed,
// and only bound for real when the start engine Listens, so another process
// can take it in between. The window is accepted rather than closed here
// because (a) the callers that most need a stable port — graceful restart,
// socket activation — supply a pre-bound listener and never reach this
// function, (b) closing it means holding a bound-but-not-listening
// SO_REUSEPORT socket across New→Listen, i.e. owning an fd whose lifetime no
// current Engine method covers, and (c) the failure it leaves is a loud
// EADDRINUSE at startup, not a silent misbind. It is a separate defect from
// the pre-bound-listener fix and deliberately out of that fix's scope; do not
// widen the window (in particular, do not move this call earlier).
func resolvePort(addr string) (string, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return addr, err
	}
	resolved := ln.Addr().String()
	_ = ln.Close()
	return resolved, nil
}
