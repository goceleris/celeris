//go:build linux

package epoll

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/ctxkit"
	"github.com/goceleris/celeris/internal/deferlinger"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/engine/internal/bindiag"
	"github.com/goceleris/celeris/internal/engine/internal/errclass"
	"github.com/goceleris/celeris/internal/platform"
	"github.com/goceleris/celeris/internal/protocol/detect"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
	"github.com/goceleris/celeris/internal/sockopts"
	"github.com/goceleris/celeris/internal/wakefd"
)

// connTableSize is the HARD CAP on slots in the flat connection array, and
// thus on concurrent connections per worker: an accepted fd >= connTableSize
// cannot be indexed into l.conns and is rejected (closed) by acceptAll with
// an ErrorConnTableCap bump + a rate-limited Warn. The kernel hands out the lowest
// free fd, so this is reached only when a worker is genuinely holding ~65 K
// live conns (plus the listen/epoll/event/timer fds). Raise this if a single
// worker must sustain more.
//
// connTableInitSize is the slice's INITIAL length. The table previously
// allocated all 65536 pointer slots (512 KiB) eagerly per worker even when a
// worker only ever sees a handful of conns — pure peak-RSS overhead. It now
// starts small and grows on demand (growConns) up to connTableSize as higher
// fds arrive. The grow is power-of-two and rare (only on a new high-water
// fd), so it never touches the steady hot path.
const (
	connTableSize     = 65536
	connTableInitSize = 4096
)

// maxEpollEvents caps the per-worker epoll_wait events array independently of
// resource.MaxEvents (8192). The array only needs to hold the max FDs that can
// be simultaneously ready in one epoll_wait return; the documented bench grid
// maxes at 1024 concurrent conns, so 2048 is comfortably above what a single
// epoll_wait can usefully batch while shedding ~96 KiB of eager per-worker RSS
// (8192→2048 × 16 B/event). MaxEvents is consumed ONLY here in the epoll
// engine (verified across engines), so clamping locally leaves iouring and the
// resource defaults untouched. A ready FD that doesn't fit one batch simply
// surfaces on the next epoll_wait — edge-/level-triggered readiness is not
// lost, only spread across an extra wait, which is irrelevant below the grid
// ceiling.
const maxEpollEvents = 2048

// errPeerClosed is returned via H1State.OnError when the peer closes the
// connection cleanly (read returns 0 bytes / EOF).
var errPeerClosed = fmt.Errorf("celeris: peer closed connection: %w", io.EOF)

// shutdownSendDrainFloor is the least time shutdown's send drain gives the
// responses still queued to reach the kernel before the conns are closed
// (celeris#760): io_uring's bound, shutdownSendDrainNanos (celeris#595). The
// budget of the Engine.Shutdown call that stopped the engine extends it to
// that budget's deadline; a peer that stops reading holds it no longer.
const shutdownSendDrainFloor = 250 * time.Millisecond

// shutdownSendDrainPoll caps one wait of the drain for a writable socket, so
// the drain notices a budget that ends early (a cancelled Shutdown ctx).
const shutdownSendDrainPoll = 20 * time.Millisecond

// h2PoolDrainPollMs caps one epoll_wait while the loop waits out the HTTP/2
// pool handlers at shutdown (celeris#759).
const h2PoolDrainPollMs = 10

// Loop is an epoll-based event loop worker.
type Loop struct {
	id       int
	cpuID    int
	epollFD  int
	listenFD int
	// wakeFD owns the wakeup eventfd. Producers on other goroutines (adopt,
	// detached WS/SSE, the H2 write queue) signal through the handle, so none
	// of them can write the descriptor number after shutdown closed it, and
	// the lazy re-creation below is no longer an unsynchronised write to a
	// field other goroutines read (celeris#655).
	wakeFD    *wakefd.WakeFD
	timerFD   int // timerfd for kernel-enforced checkTimeouts cadence (-1 if disabled)
	events    []unix.EpollEvent
	conns     []*connState
	connCount int // number of active connections (local, for draining check)
	maxFD     int // upper bound fd for iteration in checkTimeouts/shutdown
	// liveConns is a dense slice of the connections this loop holds.
	// checkTimeouts, the sweep and shutdown iterate it instead of the sparse
	// 0..maxFD range, so their cost is O(active conns) regardless of the FD
	// space (maxFD never shrinks). Maintained by addLiveConn/removeLiveConn
	// on every accept/close path. Worker-thread-only — no synchronization.
	// (#318)
	//
	// It holds connStates, not descriptor numbers (celeris#668). An async
	// hijack leaves its entry here until the dispatch goroutine has exited
	// and handed the conn back, but closes the descriptor at once, so the
	// kernel can reissue the number to the next accept on this loop before
	// the entry goes. Keyed by number, the two entries would alias: the
	// swap-remove fix-up would move the wrong connState's liveIdx and the
	// stale entry could never be found again. Walkers skip an entry whose
	// connState is hijacked.
	liveConns []*connState
	// listenHot is set by acceptAll when it stops with the listen backlog
	// possibly non-empty (per-call cap hit, or EMFILE/ENFILE back-off).
	// The listen socket is edge-triggered, so the queued SYNs won't
	// re-signal; the event loop re-enters acceptAll with a 0ms epoll_wait
	// while this is set to drain the remainder. Worker-thread-only.
	listenHot    bool
	handler      stream.Handler
	resolved     resource.ResolvedResources
	sockOpts     sockopts.Options
	cfg          resource.Config
	logger       *slog.Logger
	ready        chan error
	acceptPaused *atomic.Bool
	wake         chan struct{}
	wakeMu       sync.Mutex
	suspended    atomic.Bool
	// listenFDClosed signals that the loop has no listen FD while
	// acceptPaused=true: its pause linger has run out and the listener is
	// closed, or the loop has exited (shutdown sets it). PauseAccept waits
	// on this so it only returns once the SO_REUSEPORT group has actually
	// shed this listener; the loop notifies the wait (pause.Notify) after
	// it sets the flag. It stays false while the loop lingers
	// (celeris#662): the listener is still open and still accepting then.
	// Reset to false in ResumeAccept so a Pause→Resume→Pause cycle re-arms
	// the signal.
	listenFDClosed atomic.Bool
	// lingerUntil is the deadline, on deferlinger's monotonic clock, of the
	// accept-pause linger in progress, or 0 when none is (celeris#662): the
	// listener's TCP_DEFER_ACCEPT has been cleared and it keeps accepting
	// until then. Loop-thread-only.
	lingerUntil deferlinger.Deadline
	// deferCapable records whether listenFD was created with
	// TCP_DEFER_ACCEPT, which is whether a pause has anything to clear and
	// linger for. A listener created without it (DisableDeferAccept) closes
	// at once. Loop-thread-only.
	deferCapable bool
	// exited (declared here, after deferCapable, to sit in the padding
	// that follows it and leave the layout of the fields below unchanged) is set by Engine.runWatched when the loop goroutine is gone,
	// whether run returned or the goroutine ended without returning (a
	// handler's runtime.Goexit, celeris#848). It is never reset. PauseAccept
	// counts an exited loop as closed whatever listenFDClosed says, because
	// ResumeAccept clears that flag on every loop, a dead one included, and
	// nothing would set it again.
	exited atomic.Bool
	// pause is the engine's record of the pause in progress, written by
	// BeginPauseAccept before it sets acceptPaused. Nil in bare test loops,
	// which deferlinger treats as "no guard, no delay".
	pause *deferlinger.PauseState

	reqCount    *atomic.Uint64
	activeConns *atomic.Int64
	// errs is the engine-wide per-cause ErrorCount breakdown, shared by
	// every loop (celeris#645). There is no aggregate counter beside it:
	// EngineMetrics.ErrorCount is the sum of these buckets.
	errs              *errclass.Counters
	asyncPromoted     *atomic.Uint64 // cumulative inline → dispatch promotions (#300)
	acceptCount       *atomic.Uint64 // cumulative accepts (engine-wide, shared)
	closeCount        *atomic.Uint64 // cumulative closes (engine-wide, shared)
	bytesRead         *atomic.Uint64 // cumulative recv payload bytes (engine-wide, shared)
	bytesWritten      *atomic.Uint64 // cumulative send payload bytes (engine-wide, shared)
	reqBatch          uint64         // batched request count, flushed to reqCount per iteration; WORKER-THREAD-ONLY, 5 increment sites, all on the loop: three in drainRead (async feed, promotion, inline) and two in adopt.go (the transplant replay, sync and async) (celeris#626, celeris#886)
	bytesReadBatch    uint64         // batched recv bytes, flushed to bytesRead per iteration
	bytesWrittenBatch uint64         // batched send bytes, flushed to bytesWritten per iteration
	tickCounter       uint32
	consecutiveEmpty  uint32 // consecutive iterations with no events (for adaptive timeout)
	cachedNow         int64  // cached time.Now().UnixNano(), refreshed once per events return
	lastSweep         int64  // time.Now().UnixNano() at the start of the last checkTimeouts, any caller; the run loop's gate tests it against sweepFloor (celeris#885 item 29)

	// The #383 transplant ledger, engine-wide and shared like the counters
	// above; all of them are nil-safe so a bare test Loop literal can skip
	// them (celeris#624).
	transplantAdopted      *atomic.Uint64 // conns adopted FROM io_uring (no OnConnect fired)
	transplantDetached     *atomic.Uint64 // conns detached FOR io_uring (no OnDisconnect fired)
	transplantSlotOccupied *atomic.Uint64 // adoptions refused on an occupied slot (fd not closed)
	// One bucket per remaining silent drop point on the hand-off path
	// (celeris#624). Each of these used to lose a relinquished connection
	// with no close, no hook and nothing recorded.
	transplantHandoffRefused *atomic.Uint64 // target refused an already-relinquished fd
	transplantDrainStopped   *atomic.Uint64 // drain stopped between detach and hand-off
	transplantStranded       *atomic.Uint64 // transplant-pending conn dropped by the detach drain
	transplantAdoptRefused   *atomic.Uint64 // adoption refused for a reason other than a taken slot

	// runCtx is this loop's run context, published at the top of run and
	// read only on this thread. reclaimTransplant needs one to build a
	// connState with (celeris#624).
	runCtx context.Context

	// drainBudget points at the engine's record of the budget of the last
	// Engine.Shutdown call (celeris#760): shutdown's send drain may run
	// until that ctx is done. nil, or no Shutdown yet, leaves the drain its
	// floor, shutdownSendDrainFloor.
	drainBudget *atomic.Pointer[context.Context]

	// h2DrainStart is when the loop began waiting, its context cancelled,
	// for the HTTP/2 stream handlers running on the shared worker pool
	// (celeris#759; h2PoolSettled). Zero until then. Loop thread.
	h2DrainStart time.Time

	// transplantInFlight counts connections this loop has detached for a
	// transplant whose hand-off is not finished yet — the deferred async
	// path, where tryTransplant detaches and drainDetachQueue completes.
	// Loop-thread-only on BOTH ends, which is what lets the standby
	// suspend gate trust it: the count cannot become nonzero between that
	// gate's test and its park. Without it the gate reads connCount == 0
	// — already decremented by the detach — and parks the loop forever on
	// top of its own unfinished hand-off (celeris#624).
	transplantInFlight int

	// The post-switch sweep (celeris#657 P7, sweep.go). All loop-thread-only
	// but sweepCnt, which points at the engine-wide gauges every loop
	// publishes into. sweepTS is the drain epoch the current cadence belongs
	// to; sweepCursor resumes a budgeted cycle where the last pass stopped;
	// the cycle* fields accumulate one cycle's verdict, and sweepPub is what
	// this loop last published, so the gauge delta is exact.
	// sweepArrived records that a connection JOINED the live set during the
	// cycle in progress; such a connection is appended past the cursor and
	// cannot be reached by this cycle, so a cycle with it set may not go
	// dormant (THE CYCLE RULE in sweep.go, celeris#657 R2).
	sweepTS        *transplantState
	sweepCnt       *sweepCounters
	sweepNext      int64
	sweepIvl       int64
	sweepCursor    int
	sweepDormant   bool
	sweepArrived   bool
	cycleMoved     int
	cycleTransient int
	cycleRes       [numResidual]uint64
	sweepPub       [numResidual]uint64

	// The park-boundary ask queue (celeris#657 P8, ask.go). xferAskMu is a
	// LEAF lock: dispatch goroutines append under it at their park, the loop
	// thread drains it immediately before drainDetachQueue, and nothing else
	// is taken while it is held.
	//
	// xferAskDrain is the batch the loop is walking right now, kept on the
	// Loop (and not in drainTransplantAsks's frame) so dropAsk can reach it:
	// the release belt has to cover the entries already detached from
	// xferAskQ, not only the ones still on it (celeris#657 R2, MINOR-b).
	// xferAskPending is a COUNT, and its invariant is stated on askTransplant.
	xferAskMu      sync.Mutex
	xferAskQ       []*connState
	xferAskSpare   []*connState
	xferAskDrain   []*connState
	xferAskPending atomic.Uint32

	// fdCapDrops counts accepted fds that fell outside the l.conns table
	// (fd >= connTableSize) and were force-closed in acceptAll. Worker-
	// thread-only. fdCapWarned latches so the diagnostic Warn is emitted
	// at most once per worker — the condition is sticky (a worker at the
	// cap stays there), so one log line is enough to make it diagnosable
	// without flooding under sustained overload.
	fdCapDrops  uint64
	fdCapWarned bool

	dirtyHead      *connState // head of intrusive doubly-linked dirty list
	h2Conns        []int      // FDs of H2 connections (for write queue polling)
	h2cfg          conn.H2Config
	detachQueue    []*connState // detached conns with pending writes (goroutine-safe via detachQMu)
	detachQMu      sync.Mutex
	detachQSpare   []*connState // reuse slice to avoid alloc in drainDetachQueue
	detachQPending atomic.Int32 // 1 when detachQueue has entries; gates the hot-path drain
	detachedCount  int          // number of currently-detached conns; gates idle-deadline sweep

	// adoptQueue holds io_uring→epoll transplant hand-offs (#383 reverse
	// direction): real connected fds to adopt onto this loop. Cross-thread:
	// AdoptConn appends + wakes via eventFD; the loop applies them in
	// drainAdoptQueue on its own thread (where epoll_ctl + connState setup are safe).
	adoptQueue    []adoptItem
	adoptQMu      sync.Mutex
	adoptQSpare   []adoptItem
	adoptQPending atomic.Int32
	// adoptQClosed is set under adoptQMu by closeAdoptQueue when the loop
	// shuts down; AdoptConn refuses from then on (celeris#658).
	adoptQClosed bool

	// asyncWG tracks runAsyncHandler goroutines so graceful shutdown
	// can Wait on them before returning. Without this the engine's
	// top-level wg.Wait joins only the worker/Listen goroutines, and
	// dispatch Gs can still be in ProcessH1 against released state
	// when shutdown returns.
	asyncWG sync.WaitGroup

	// Driver integration (EventLoopProvider). The hasDriverConns gate is the
	// ONLY check the HTTP hot path pays when no drivers are registered; it
	// must stay an atomic.Bool load, not a map read.
	driverConns map[int]*driverConn
	driverMu    sync.RWMutex
	// driverGen is the generation of the last driver registration on this
	// loop (protected by driverMu). Each RegisterConn takes the next one, in
	// 1..maxDriverGen: never 0, and never with bit 31 set, which is an HTTP
	// conn's (celeris#771, celeris#885).
	driverGen uint32
	// httpGen is the counter the HTTP conns' generations are drawn from
	// (nextHTTPGen): the last one, without bit 31. Loop thread only.
	httpGen        uint32
	hasDriverConns atomic.Bool
	driverReadBuf  []byte // scratch buffer for driver EPOLLIN drains (worker-local)
	// driverReadQ holds the driver conns whose last read turn used up
	// driverReadBudget with bytes possibly left (loop thread only).
	// Edge-triggered epoll does not report bytes already in the socket again,
	// so the loop gives each of them one more turn per round, after that
	// round's batch (serveDriverReadQ), and does not wait in epoll_wait while
	// one is queued. driverReadQNext is the spare backing array the queue swaps
	// with, so a turn costs no allocation (celeris#930).
	driverReadQ, driverReadQNext []*driverConn
	// ctlMu guards epollFD against the driver goroutines, which issue
	// epoll_ctl on it from outside the loop thread. shutdown takes it as a
	// writer before it closes the descriptor (closeEpollFD), so a driver's
	// epoll_ctl either completes first or is refused — it can never land on
	// a recycled descriptor number. Same rule as the wakeup eventfd, for the
	// other descriptor a driver can reach (celeris#655). A LEAF: held only
	// across the epoll_ctl itself, and taken while holding driverMu or dc.mu.
	ctlMu     sync.RWMutex
	ctlClosed bool

	// async dispatches HTTP1 handlers to spawned goroutines. Set by
	// Config.AsyncHandlers (the canonical server-level default) or
	// auto-enabled in doPrepare when the router has any .Async() routes.
	// Per-route .Async()/.Async(false) overrides further refine dispatch
	// per request via the InlineMode → ErrAsyncDispatch handoff. Checked
	// on every drainRead; keep it a plain bool so the no-async path is a
	// single mov+test.
	async bool

	// transplant (#383) is non-nil while a drain-to-io_uring is in progress.
	// Set by Engine.StartTransplant (controller goroutine) and read on this
	// loop's own thread after each drainRead; the atomic.Pointer load is the
	// only cost the hot path pays when no drain is active (nil). When set, an
	// eligible conn at a request boundary is detached and handed to the target
	// io_uring engine. See docs/design/383-connection-transplant.md.
	transplant atomic.Pointer[transplantState]

	// listenAddr is the address listenFD was bound to, recorded on this
	// loop's thread before it signals ready (celeris#639). Past ready the
	// loop may already have closed listenFD — a context cancelled during
	// startup runs shutdown at once — so Listen publishes this instead of
	// asking the kernel about a descriptor number that may now belong to
	// something else. Written once before ready; read only after it.
	listenAddr net.Addr
}

func newLoop(id, cpuID int, handler stream.Handler,
	resolved resource.ResolvedResources,
	cfg resource.Config, reqCount *atomic.Uint64, activeConns *atomic.Int64, errs *errclass.Counters,
	asyncPromoted *atomic.Uint64, acceptPaused *atomic.Bool,
	acceptCount, closeCount, bytesRead, bytesWritten *atomic.Uint64) *Loop {

	return &Loop{
		id:           id,
		cpuID:        cpuID,
		epollFD:      -1,
		listenFD:     -1,
		wakeFD:       wakefd.New(-1),
		timerFD:      -1,
		events:       make([]unix.EpollEvent, min(resolved.MaxEvents, maxEpollEvents)),
		conns:        make([]*connState, connTableInitSize),
		liveConns:    make([]*connState, 0, 1024),
		handler:      handler,
		resolved:     resolved,
		cfg:          cfg,
		logger:       cfg.Logger,
		acceptPaused: acceptPaused,
		wake:         make(chan struct{}),
		ready:        make(chan error, 1),
		// TCPNoDelay is omitted here: accepted sockets inherit TCP_NODELAY
		// from the listen socket (set in createListenSocket), so per-accept
		// ApplyFD doesn't need to re-issue it.
		sockOpts: sockopts.Options{
			TCPQuickAck: true,
			SOBusyPoll:  50 * time.Microsecond,
			RecvBuf:     resolved.SocketRecv,
			SendBuf:     resolved.SocketSend,
		},
		reqCount:      reqCount,
		activeConns:   activeConns,
		errs:          errs,
		asyncPromoted: asyncPromoted,
		acceptCount:   acceptCount,
		closeCount:    closeCount,
		bytesRead:     bytesRead,
		bytesWritten:  bytesWritten,
		h2cfg: conn.H2Config{
			MaxConcurrentStreams: cfg.MaxConcurrentStreams,
			InitialWindowSize:    cfg.InitialWindowSize,
			MaxFrameSize:         cfg.MaxFrameSize,
			MaxRequestBodySize:   cfg.MaxRequestBodySize,
			WriteTimeout:         cfg.WriteTimeout,
			Logger:               cfg.Logger,
		},
		async: cfg.AsyncHandlers,
	}
}

func (l *Loop) run(ctx context.Context) {
	// Publish the run context for the loop-thread paths that need to build
	// a connState but are not called from this frame — reclaimTransplant
	// (celeris#624). Written and read on this thread only.
	l.runCtx = ctx

	// celeris#905: the loop changes state that belongs to its OS thread, the
	// CPU affinity and the NUMA memory policy below, so it never unlocks the
	// thread. A goroutine that exits locked takes its thread with it: the
	// runtime terminates the thread instead of handing it, still pinned to
	// one CPU, to whatever goroutine runs next. The main thread cannot exit;
	// the runtime parks it for good instead, and a loop often runs on it, so
	// the affinity and the policy are also put back on the way out.
	runtime.LockOSThread()

	// The save reads the whole mask, however many CPUs the kernel has
	// (platform.SaveThreadAffinity). It fails only on a syscall error or a
	// mask past 8 KiB, and the pin below still goes ahead: a main thread the
	// loop ran on is then parked still pinned (celeris#905).
	if prev, err := platform.SaveThreadAffinity(); err == nil {
		defer func() { _ = prev.Restore() }()
	}
	// celeris#909: a loop whose pin failed (the planner plans none unpinned
	// since celeris#1036; a test plan can) has cpuID -1 and no NUMA node to
	// bind to.
	l.pinOwnThread()

	if l.cpuID >= 0 {
		numaNode := platform.CPUForNode(l.cpuID)
		if err := platform.BindNumaNode(numaNode); err == nil {
			defer func() { _ = platform.ResetNumaPolicy() }()
		}
	}

	epollFD, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		l.ready <- fmt.Errorf("loop %d: epoll_create1: %w", l.id, err)
		return
	}
	l.epollFD = epollFD

	listenFD, err := createListenSocket(l.cfg.Addr, !l.cfg.DisableDeferAccept)
	if err != nil {
		_ = unix.Close(epollFD)
		l.ready <- fmt.Errorf("loop %d: listen socket: %w", l.id, err)
		return
	}
	l.listenFD = listenFD
	l.deferCapable = !l.cfg.DisableDeferAccept

	if err := unix.EpollCtl(epollFD, unix.EPOLL_CTL_ADD, listenFD, &unix.EpollEvent{
		Events: unix.EPOLLIN | unix.EPOLLET,
		Fd:     int32(listenFD),
	}); err != nil {
		_ = unix.Close(listenFD)
		_ = unix.Close(epollFD)
		l.ready <- fmt.Errorf("loop %d: epoll_ctl listen: %w", l.id, err)
		return
	}

	efd, efdErr := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
	if efdErr != nil {
		efd = -1
	}
	if efd >= 0 {
		if err := unix.EpollCtl(epollFD, unix.EPOLL_CTL_ADD, efd, &unix.EpollEvent{
			Events: unix.EPOLLIN | unix.EPOLLET,
			Fd:     int32(efd),
		}); err != nil {
			_ = unix.Close(efd)
			efd = -1
		}
	}
	// celeris#655: producers hold the handle, never the number, so the
	// descriptor cannot be written once shutdown has closed it.
	if !l.wakeFD.Set(efd) && efd >= 0 {
		_ = unix.Close(efd)
	}

	// timerfd: forces checkTimeouts to run on a kernel-enforced 25ms
	// cadence regardless of socket-event traffic. Without this, idle
	// or low-traffic workers ran checkTimeouts only every ~800ms in
	// the worst case (gate × adaptive_wait), eating into the
	// slowloris walker's 2s slack budget for slow refapps. Mirrors
	// the kernel-precision that iouring gets from per-conn
	// IORING_OP_TIMEOUT SQEs.
	if l.cfg.ReadHeaderTimeout > 0 {
		tfd, tfdErr := unix.TimerfdCreate(unix.CLOCK_MONOTONIC, unix.TFD_NONBLOCK|unix.TFD_CLOEXEC)
		if tfdErr == nil {
			// sweepFloor periodic (25 ms); first fire at the same.
			spec := unix.ItimerSpec{
				Interval: unix.Timespec{Sec: 0, Nsec: int64(sweepFloor)},
				Value:    unix.Timespec{Sec: 0, Nsec: int64(sweepFloor)},
			}
			if err := unix.TimerfdSettime(tfd, 0, &spec, nil); err != nil {
				_ = unix.Close(tfd)
				tfd = -1
			} else if err := unix.EpollCtl(epollFD, unix.EPOLL_CTL_ADD, tfd, &unix.EpollEvent{
				Events: unix.EPOLLIN,
				Fd:     int32(tfd),
			}); err != nil {
				_ = unix.Close(tfd)
				tfd = -1
			}
			l.timerFD = tfd
		}
	}

	// celeris#639: listenFD is certainly this loop's socket here; after ready
	// a cancelled context closes it in shutdown.
	l.listenAddr = listenAddrOf(l.listenFD)
	l.ready <- nil

	activeTimeoutMs := 1 // 1ms default epoll timeout
	l.cachedNow = time.Now().UnixNano()

	for {
		if ctx.Err() != nil {
			// Accept nothing more (celeris#759): the loop may keep turning
			// below, for the HTTP/2 conns it has, and a conn accepted now
			// would be served and then cut at the budget.
			l.stopAccepting()
			if l.h2PoolSettled() {
				l.shutdown()
				return
			}
		}

		// Cache the atomic load: ACTIVE→LINGERING→DRAINING and
		// SUSPENDED→ACTIVE branches both read it. Saves 1 atomic load per
		// event-loop iteration on the steady-state hot path.
		paused := l.acceptPaused.Load()
		if l.listenFD >= 0 && (paused || l.lingerUntil != 0) {
			// Cold: a pause is starting, lingering, ending, or being
			// withdrawn by a resume (celeris#662). See stepAcceptPause.
			l.stepAcceptPause(ctx, paused)
		}
		// Maintain the listenFDClosed signal that PauseAccept waits on:
		// true exactly while paused with no listen fd. That covers the
		// close stepAcceptPause just did and an fd that was already -1
		// from a prior Pause-Resume cycle that hadn't re-created the
		// socket yet (ResumeAccept clears the flag and toggles
		// paused=false; the loop may be mid-iteration when paused flips
		// back to true). It is false while the loop lingers, because the
		// listener is still open and accepting, and false once paused goes
		// false so a subsequent Pause observes a fresh signal.
		l.listenFDClosed.Store(paused && l.listenFD < 0)

		// SUSPENDED → ACTIVE: re-create listen socket after ResumeAccept;
		// never once shutdown has begun (stopAccepting).
		if l.listenFD < 0 && !paused && ctx.Err() == nil {
			fd, err := createListenSocket(l.cfg.Addr, !l.cfg.DisableDeferAccept)
			l.deferCapable = !l.cfg.DisableDeferAccept
			if err != nil {
				// This loop is about to stop accepting for good, so
				// the bump is not a rate: it is the one record that
				// the engine lost a listener (celeris#645).
				l.errs.ListenerRecreate.Add(1)
				l.logger.Error("re-create listen socket", "loop", l.id, "err", err)
				l.shutdown()
				return
			}
			if err := unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_ADD, fd, &unix.EpollEvent{
				Events: unix.EPOLLIN | unix.EPOLLET,
				Fd:     int32(fd),
			}); err != nil {
				l.errs.ListenerRecreate.Add(1)
				l.logger.Error("epoll_ctl re-add listen", "loop", l.id, "err", err)
				_ = unix.Close(fd)
				l.shutdown()
				return
			}
			l.listenFD = fd
		}

		timeoutMs := l.adaptiveTimeoutMs(activeTimeoutMs)
		if l.listenFD < 0 {
			timeoutMs = 500
		}
		// A sweep pass is owed (celeris#657 P7): cap the wait so it runs on
		// its own cadence and not at the mercy of the next event. Only while
		// a pass IS owed — a dormant sweep, or one with nothing live, returns
		// -1 and the standby loop blocks exactly as it did before.
		if ms := l.sweepWaitMs(); ms >= 0 && (timeoutMs < 0 || ms < timeoutMs) {
			timeoutMs = ms
		}
		// A lingering pause must wake for its deadline (celeris#662). Applied
		// AFTER the sweep cap so the wait is the minimum of both: while the
		// listener is open the wait is already at most a few ms
		// (adaptiveTimeoutMs), so this is defence, not the mechanism.
		if l.lingerUntil != 0 {
			timeoutMs = l.lingerTimeoutMs(timeoutMs)
		}
		// acceptAll stopped with the listen backlog possibly non-empty
		// (per-call cap hit, or EMFILE/ENFILE back-off). The listen socket
		// is edge-triggered, so don't block — poll immediately and re-drain.
		if l.listenHot {
			timeoutMs = 0
		}
		// Waiting out HTTP/2 pool handlers at shutdown (celeris#759): a
		// handler that ends without a write the loop hears of must not
		// leave the loop blocked.
		if !l.h2DrainStart.IsZero() && (timeoutMs < 0 || timeoutMs > h2PoolDrainPollMs) {
			timeoutMs = h2PoolDrainPollMs
		}

		// A driver conn is owed a read turn (celeris#930): collect what is
		// ready and do not wait. Last, after every cap above, because
		// edge-triggered epoll will not report the bytes it is owed again.
		if len(l.driverReadQ) > 0 {
			timeoutMs = 0
			if h := testHookDriverQueueWait; h != nil {
				h(len(l.driverReadQ), timeoutMs)
			}
		}

		n, err := unix.EpollWait(l.epollFD, l.events, timeoutMs)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			l.logger.Error("epoll_wait error", "loop", l.id, "err", err)
			continue
		}
		// The driver conns owed a turn from an earlier round. A conn that
		// uses up its budget while this batch is dispatched waits for the
		// next round (celeris#930).
		owedDriverReads := len(l.driverReadQ)

		var now int64
		if n > 0 {
			// Refresh the cached timestamp on every events-bearing
			// epoll_wait return. The previous &0x3F gate refreshed only
			// every 64th return, so lastActivity (written from this `now`
			// on accept/read) could lag wall-clock by up to ~63 returns
			// while checkTimeouts compares against a FRESH time.Now() —
			// closing still-active conns early. The vDSO cost (~50ns) is
			// negligible amortized over the whole event batch. cachedNow
			// is loop-thread-local, so no synchronization is needed.
			l.cachedNow = time.Now().UnixNano()
			now = l.cachedNow
		}
		for i := range n {
			ev := &l.events[i]
			fd := int(ev.Fd)

			// Eventfd wakeup: drain counter and let the H2 write queue
			// drain pass below handle the actual data.
			if efd := l.wakeFD.FD(); fd == efd && efd >= 0 {
				var buf [8]byte
				_, _ = unix.Read(efd, buf[:])
				continue
			}

			// timerfd wakeup: drain the 8-byte expiration counter and
			// fire checkTimeouts NOW so the slowloris HeaderDeadline
			// hits within the kernel-precise 25ms cadence rather than
			// the sweep gate's worst-case window. Mirrors iouring's
			// per-conn IORING_OP_TIMEOUT precision on the epoll engine.
			if fd == l.timerFD && l.timerFD >= 0 {
				var buf [8]byte
				_, _ = unix.Read(l.timerFD, buf[:])
				l.checkTimeouts()
				continue
			}

			if fd == l.listenFD && l.listenFD >= 0 {
				l.acceptAll(ctx, now)
				continue
			}

			// Driver fast-path: a load of the event's Pad (read with the
			// event: the generation of a driver registration, an HTTP conn's
			// generation with bit 31 set, or 0) and a single atomic load when
			// no drivers are registered (zero-cost for pure-HTTP workloads).
			// The map lookup happens only when one of them says driver.
			// dispatchDriver drops an event that names a registration which
			// has ended (celeris#771).
			pad := uint32(ev.Pad)
			if l.driverCandidate(pad) {
				if l.dispatchDriver(fd, pad, ev.Events) {
					continue
				}
			}
			// An HTTP conn's event carries the generation of its registration
			// (httpGenBit set), and a driver's has been dealt with: what is
			// left is Pad 0 (the listen socket, the timer, a descriptor that
			// is no conn's) or an HTTP generation. An event collected for an
			// HTTP conn that has since closed must not reach the conn on its
			// number now: a handler that is not the event's own (the timer
			// branch's checkTimeouts, a transplant, an h2 pass) may have
			// closed it earlier in this batch, and acceptAll may have put a
			// new conn on the number (celeris#885 item 18). Pad 0 skips the
			// check: the descriptors that carry it (the wake eventfd, the
			// timerfd and the listen socket, all matched by number above,
			// before any generation is looked at, and the re-created listen
			// socket) are not conns. A stale event of a conn that closed
			// earlier in this batch can therefore be taken for one of them
			// when its number has since been reissued to it (a sync-mode
			// Detach creates the wake eventfd inside drainRead): the eventfd
			// read, the sweep or the accept that follows is harmless, as
			// drainDetachQueue runs after the batch regardless.
			if pad != 0 && fd >= 0 && fd < len(l.conns) {
				if cs := l.conns[fd]; cs != nil && cs.gen != pad {
					continue
				}
			}

			// Process EPOLLIN before EPOLLHUP: a peer may send data and
			// immediately close, producing both flags in one event.
			if ev.Events&unix.EPOLLIN != 0 {
				l.drainRead(fd, now)
			}

			// EPOLLOUT: the socket became writable again for a conn that hit
			// write backpressure (armEpollOut). Flush the pending bytes and
			// disarm once drained. drainRead above may have closed the conn,
			// so re-validate the slot. EPOLLOUT is edge-triggered here (EPOLLET
			// covers the whole mask), and a partial flush (a short write or
			// EAGAIN: one write(2) per flush, whose short count means the send
			// buffer was full) leaves it full, so the send buffer gaining room
			// is the next edge that resumes it.
			if ev.Events&unix.EPOLLOUT != 0 {
				if fd >= 0 && fd < len(l.conns) {
					if cs := l.conns[fd]; cs != nil && cs.epollOut {
						l.handleWritable(cs)
					}
				}
			}

			// EPOLLRDHUP: peer half-closed (sent FIN). See onPeerHalfClose.
			if ev.Events&unix.EPOLLRDHUP != 0 {
				l.onPeerHalfClose(fd)
			}

			if ev.Events&(unix.EPOLLERR|unix.EPOLLHUP) != 0 {
				if fd >= 0 && fd < len(l.conns) && l.conns[fd] != nil {
					l.closeConn(fd)
				}
			}

			// #383: if a drain-to-io_uring is in progress, this fd just finished
			// a request cycle (EPOLLIN → drainRead) and may now be at a clean
			// keep-alive boundary — try to transplant it. tryTransplant re-checks
			// the slot, so a close above is harmless. The pointer load is the
			// whole cost when no drain is active.
			if l.transplant.Load() != nil {
				l.tryTransplant(fd)
			}
		}

		// listenHot: acceptAll capped/backed-off last round with the listen
		// backlog possibly non-empty. The edge-triggered listen socket won't
		// re-signal those queued SYNs, and this epoll_wait may have returned
		// for an unrelated fd (or timed out at 0ms with no event), so re-drain
		// explicitly here rather than relying on a listen-fd event. acceptAll
		// clears or re-sets listenHot based on whether the backlog drained.
		if l.listenHot && l.listenFD >= 0 {
			l.acceptAll(ctx, time.Now().UnixNano())
		}

		// Adaptive timeout tracking.
		if n > 0 {
			l.consecutiveEmpty = 0
		} else {
			l.consecutiveEmpty++
		}

		// celeris#657 P7/P8: re-examine what the drain still holds, and the
		// connections whose dispatch goroutines asked at their park. Both run
		// BEFORE drainDetachQueue, so a deferred async hand-off either
		// starts here finishes in this same iteration once the goroutine
		// exits — and, for the ask queue, so no connState is drained from it
		// after drainDetachQueue has returned it to the pool (celeris#654).
		l.sweep()
		l.drainTransplantAsks()

		// Drain detached goroutine writes BEFORE the dirty flush so that
		// data written by goroutines (e.g. WebSocket responses) is flushed
		// in the same event loop iteration.
		l.drainDetachQueue()

		// #383 reverse: adopt any conns transplanted to us from io_uring. Use a
		// fresh timestamp when the batch carried no events (now==0 then).
		if l.adoptQPending.Load() != 0 {
			an := now
			if an == 0 {
				an = l.cachedNow
			}
			l.drainAdoptQueue(ctx, an)
		}

		l.flushDirty()

		// Drain H2 async write queues (drainH2Writes).
		if len(l.h2Conns) != 0 {
			l.drainH2Writes()
		}

		// The read turns driver conns are owed (celeris#930), after this
		// batch's HTTP responses have gone out.
		if owedDriverReads != 0 {
			l.serveDriverReadQ(owedDriverReads)
		}

		// (flushBatchedCounters, at shutdown, repeats these three flushes:
		// a counter added to the batches goes in both.)
		// Flush batched request count to the shared atomic counter. This
		// replaces per-request atomic.Add with one atomic per event loop
		// iteration, eliminating cache-line bouncing under multi-worker
		// contention.
		if l.reqBatch > 0 {
			l.reqCount.Add(l.reqBatch)
			l.reqBatch = 0
		}

		// Flush batched payload-byte counters with the same per-iteration
		// cadence as reqCount, for the same cache-line-contention reason.
		if l.bytesReadBatch > 0 {
			l.bytesRead.Add(l.bytesReadBatch)
			l.bytesReadBatch = 0
		}
		if l.bytesWrittenBatch > 0 {
			l.bytesWritten.Add(l.bytesWrittenBatch)
			l.bytesWrittenBatch = 0
		}

		// Check connection timeouts. Default cadence is every 1024 iterations;
		// when detached conns exist with idle deadlines the gate tightens to
		// every 32 iterations so the WS idle-close fires within its
		// configured budget. An iteration is one epoll_wait return, so the
		// wall time between gate hits depends on the load: on a busy loop it
		// is well under a millisecond, and the timerfd (25 ms) is only the
		// floor on an idle one. So a gate hit sweeps only when sweepFloor has
		// passed since the last sweep, by whoever made it (this gate, or the
		// timerfd's): a busy loop then sweeps about once per sweepFloor
		// instead of thousands of times a second (celeris#885 item 29). The
		// price is that a detached conn's expired idle deadline is reaped up
		// to sweepFloor plus the iterations of one gate stride after it
		// expires, not 32 iterations after; the same 25 ms the timerfd gives
		// a header deadline. A clock that stepped back (d < 0) sweeps, or
		// the sweep would wait for the clock to catch up. When
		// ReadHeaderTimeout is enabled (the v1.4.11 slowloris defence),
		// the gate ALSO tightens to 0x1F — the in-process synthetic
		// reproducer (test/integration/slowloris_synthetic_test.go) showed
		// epoll's HeaderDeadline firing 2s late without this.
		l.tickCounter++
		gate := uint32(0x3FF)
		if l.detachedCount > 0 || l.cfg.ReadHeaderTimeout > 0 {
			gate = 0x1F
		}
		if l.tickCounter&gate == 0 {
			// The clock is read at a gate hit only, one iteration in 32.
			if d := time.Now().UnixNano() - l.lastSweep; d >= int64(sweepFloor) || d < 0 {
				l.checkTimeouts()
			}
		}

		// DRAINING → SUSPENDED: no listen socket, no connections, events
		// processed, and NO CROSS-ENGINE HAND-OFF STILL OWED.
		//
		// A driver conn that is owed a read turn (driverReadQ, celeris#930)
		// keeps the loop awake too: this park is indefinite, and the bytes the
		// turn would read are in the socket already, so they would wait for
		// the next ResumeAccept. On the base one event drained the socket
		// before the loop got here.
		//
		// That last clause is celeris#624. This park is indefinite — the
		// loop leaves epoll_wait entirely and waits on a Go channel that
		// only ResumeAccept closes — so anything still owed to
		// drainDetachQueue or drainAdoptQueue at this point is owed
		// forever. And connCount == 0 does NOT mean nothing is owed:
		// detachFromEpoll decrements it the instant a conn is detached FOR
		// A TRANSPLANT, while the hand-off itself is finished later, by
		// this loop, in drainDetachQueue. So the LAST connection a standby
		// loop drains — connCount hits 0 on the detach, the dispatch
		// goroutine has not enqueued it yet — used to park the loop on top
		// of its own unfinished hand-off: the descriptor stayed open and
		// owned by no engine, io_uring never heard of it, no OnDisconnect
		// fired, and the live gauge stayed one short for the life of the
		// process. One lost connection per loop that finishes its drain.
		//
		// transplantInFlight is the race-free half of the guard: both its
		// increment (tryTransplant) and its decrement
		// (finishTransplantHandoff) run on THIS thread, so it cannot go
		// nonzero between the test and the park. The two queue flags are
		// NOT race-free, and for the adopt queue that matters: AdoptConn
		// arrives from another goroutine with nothing in this loop's table,
		// so it can enqueue between this test and the park, and its eventfd
		// write cannot end a park. That lost wakeup left a relinquished
		// descriptor queued on a standby loop until the next ResumeAccept —
		// forever, if none came (celeris#658). The flags are therefore
		// re-checked under wakeMu below, and AdoptConn kicks a parked loop
		// through wakeIfSuspended; see there for why the pair cannot lose a
		// wakeup. (Every detach-queue enqueue that carries work happens with
		// the conn still counted in connCount — an off-thread hijack's notice
		// included — or under transplantInFlight. The rest name a conn the
		// loop has already let go of: closed, hijack-settled or handed over,
		// whose entry does nothing. So the detach flag needs no kick; the
		// re-check is for symmetry.)
		//
		// A driver conn keeps the loop out of the park as well (celeris#885
		// item 39): connCount counts HTTP conns only, and a parked loop is
		// not in epoll_wait, so the bytes a registered driver conn is sent
		// would wait for a ResumeAccept that may never come (an adaptive
		// sub-engine demoted to standby keeps the conns pinned to it).
		// hasDriverConns is set before the conn is armed (RegisterConn), and
		// is re-checked under wakeMu with the other flags; a conn registered
		// after the loop parked is reached by RegisterConn's
		// wakeIfSuspended, by the same publish-then-lock pairing.
		if l.listenFD < 0 && l.connCount == 0 && l.acceptPaused.Load() &&
			l.transplantInFlight == 0 && len(l.driverReadQ) == 0 &&
			!l.hasDriverConns.Load() &&
			l.detachQPending.Load() == 0 && l.adoptQPending.Load() == 0 {
			// A parked loop holds nothing, and sweep() — whose empty-set
			// retraction is what clears this loop's share of the residual
			// gauges while it runs — does not run again until it wakes. A
			// last connection that left AFTER this iteration's sweep() (the
			// tick-gate checkTimeouts above, the detach queue, the dirty
			// flush, the H2 queue) would otherwise leave its residue
			// standing in TransplantResidual* for as long as the park lasts
			// (celeris#711). Retract here, where the sweep stops.
			if len(l.liveConns) == 0 {
				l.sweepRetract()
			}
			l.wakeMu.Lock()
			if !l.acceptPaused.Load() || l.hasDriverConns.Load() ||
				l.adoptQPending.Load() != 0 || l.detachQPending.Load() != 0 {
				l.wakeMu.Unlock()
				continue
			}
			l.suspended.Store(true)
			wake := l.wake
			l.wakeMu.Unlock()

			select {
			case <-wake:
			case <-ctx.Done():
				l.shutdown()
				return
			}
			continue
		}
	}
}

// onPeerHalfClose handles EPOLLRDHUP: the peer half-closed (sent FIN).
// drainRead's short-read fast path returns without a trailing EAGAIN read,
// so a FIN that rode the same readable edge as the request (client writes
// then immediately closes) leaves the EOF unread and — with EPOLLET — no
// further edge fires. Close here once the response has flushed; if a write
// is still pending (backpressure), defer via cs.peerClosed so the response is
// not truncated. Detached (WS/SSE) conns keep their middleware's close
// lifecycle, but they still have to LEARN the peer is gone — see
// notifyDetachedPeerClosed. Loop thread.
//
// A conn whose close is already under way (asyncClosed) is left alone.
// drainRead's EOF branch, on this same event, may have left the close to a
// dispatch goroutine that is still inside its handler (celeris#669); that
// goroutine is writing the response under detachMu, so csWritePending would
// read its buffers unlocked, and closeConn would only repeat the hand-off.
//
// What the verdict is made of is read under detachMu (celeris#885 items 27 and
// 36): h1State, which switchToH2Local sets to nil on the dispatch goroutine
// under that lock, and the write state, which the goroutine's writeFn appends
// to under it, csWritePending and the count markClosing stamps. They are read
// once, together, and the lock is released before anything is done with them:
// closeConn and notifyDetachedPeerClosed take the same non-reentrant mutex.
//
// The lock is taken with TryLock and never with a plain Lock while a handler
// runs: the goroutine holds it across ProcessH1, so waiting would park the
// loop, and every conn on it, behind one slow handler (celeris#669). EPOLLRDHUP
// is edge-triggered, so the event cannot be skipped either: it would not come
// again. A half-close found while a handler runs is owed instead
// (halfCloseOwed), as the dirty pass and the EPOLLOUT resume owe their flush:
// the conn is given up (relink) and its goroutine hands it back at its next
// park, where drainDetachQueue runs this again (handBackHalfClose). A holder
// that is not a running handler (a detached conn's guarded writeFn, or the
// goroutine between handlers) holds it for one write, so that wait is bounded
// and the lock is waited for, as every other site does (dispatchBusy).
func (l *Loop) onPeerHalfClose(fd int) {
	if fd < 0 || fd >= len(l.conns) {
		return
	}
	cs := l.conns[fd]
	if cs == nil || cs.detachClosed || cs.asyncClosed.Load() {
		return
	}
	mu := cs.detachMu
	if mu != nil && !mu.TryLock() {
		if dispatchBusy(cs, &cs.relinkOwed) {
			cs.halfCloseOwed = true
			l.relink(cs)
			return
		}
		mu.Lock()
	}
	if mu != nil && cs.hijacked.Load() {
		// The run loop read cs from its slot, and then the conn's handler
		// hijacked it (celeris#668): the descriptor is closed and its number
		// may be another file's. This is belt and braces: closeConn
		// re-reads the slot under driverMu and re-validates ownership under
		// the lock, and markClosing touches loop-thread fields only. What
		// the return buys is that a hijacked conn is neither marked closing
		// nor closed from a verdict read before the hijack
		// (TestOnPeerHalfCloseLeavesAHijackedConnAlone885). drainDetachQueue
		// settles the rest.
		mu.Unlock()
		return
	}
	// Only plain and atomic loads from here to the Unlock: nothing can panic
	// or block while the lock is held.
	var (
		detached bool
		queued   bool
		pending  int
	)
	if h1 := cs.h1State; h1 != nil && h1.Detached.Load() {
		detached = true
	} else if csWritePending(cs) {
		queued = true
		pending = csPendingBytes(cs)
	}
	if mu != nil {
		mu.Unlock()
	}
	switch {
	case detached:
		l.notifyDetachedPeerClosed(cs)
	case queued:
		l.markClosing(cs, pending)
	default:
		l.closeConn(fd)
	}
}

// handBackHalfClose is drainDetachQueue's part of a half-close that
// onPeerHalfClose owed (halfCloseOwed): the conn's goroutine has handed it
// back, so the verdict that could not be read under a running handler is made
// now. It reports whether the caller may go on to put cs back on the dirty
// list: false when the conn was closed (cs may already be pooled) or when the
// handler is running again and the half-close is owed again (relink has given
// the conn up once more, and the next hand-back is its next chance). Loop
// thread.
func (l *Loop) handBackHalfClose(cs *connState) (live bool) {
	if !cs.halfCloseOwed {
		return true
	}
	cs.halfCloseOwed = false
	fd := cs.fd
	l.onPeerHalfClose(fd)
	l.driverMu.RLock()
	owned := fd >= 0 && fd < len(l.conns) && l.conns[fd] == cs
	l.driverMu.RUnlock()
	return owned && !cs.halfCloseOwed
}

// notifyDetachedPeerClosed reports an EPOLLRDHUP (peer half-close) to the
// middleware that owns a detached WS/SSE connection. The engine does NOT close
// here — after Detach the close lifecycle belongs to that middleware — it only
// makes sure the middleware finds out, which is the whole gap celeris#494 left
// open on the RDHUP path: the FIN that rode the request's readable edge is
// never read (drainRead's short-read fast path returns first, and EPOLLET
// yields no further edge), and an SSE handler that never writes again never
// earns an EPIPE either, so its handler, heartbeat and release goroutines leak
// for the life of the process.
//
// WebSocket conns are excluded: a WS peer that shuts down its write side after
// a Close frame is still reading our reply, and its own middleware surfaces the
// half-close on the next frame read. WSDataDelivery is the discriminator, and
// reading it here is race-free for the same reason ProcessH1's delivery check
// is: the WS middleware installs it via UpgradeWebSocket BEFORE Context.Detach,
// so a thread that observes Detached==true observes it too.
//
// OnError runs under cs.detachMu, matching every other OnError site in this
// file, so it serialises with the middleware goroutine's guarded writes. Do not
// close the conn from here — closeConn takes the same (non-reentrant) mutex.
func (l *Loop) notifyDetachedPeerClosed(cs *connState) {
	if mu := cs.detachMu; mu != nil {
		mu.Lock()
	}
	// Re-read h1State UNDER the lock: the async dispatch goroutine nils it in
	// switchToH2Local while holding the same mutex (the #256 TOCTOU class).
	// A detached conn is never h2c-upgraded, so this is belt-and-braces — but
	// it is the idiom every cross-goroutine h1State read here uses.
	if h1 := cs.h1State; h1 != nil && h1.WSDataDelivery == nil && h1.OnError != nil {
		h1.OnError(errPeerClosed)
	}
	if mu := cs.detachMu; mu != nil {
		mu.Unlock()
	}
}

// acceptAllCap bounds how many connections acceptAll drains in a single
// call so one worker can't starve its other ready FDs under an accept
// flood. The listen socket is edge-triggered (EPOLLIN|EPOLLET): with ET we
// MUST drain to EAGAIN or we won't get another readiness edge for already-
// queued SYNs. So when the cap is hit with the backlog possibly non-empty
// we set listenHot, which makes the event loop re-enter acceptAll on the
// next iteration with a 0ms epoll_wait — draining the rest without waiting
// for a fresh SYN edge.
const acceptAllCap = 1024

// acceptRetry reports whether an accept4 failure is a transient one that
// acceptAll retries in place: an aborted SYN (ECONNABORTED, a peer that reset
// before accept) or an interrupted syscall (EINTR). The aborted SYN lands in
// AcceptCancelled, as the same errno does when io_uring's accept completes
// with it; EINTR is not counted (errclass.Counters.AcceptRetried is the one
// list of both engines, so the two cannot drift apart). It is called from the
// error switch's default case only, so a drained queue (EAGAIN) pays nothing.
func (l *Loop) acceptRetry(err error) bool {
	errno, ok := err.(unix.Errno)
	return ok && l.errs.AcceptRetried(errno)
}

// acceptStop is why acceptAll returned. The event loop only needs listenHot;
// acceptQueuedOnPause needs to tell a queue that is empty from one that may
// not be (celeris#662).
type acceptStop uint8

const (
	acceptDrained acceptStop = iota // accept4 hit EAGAIN: the queue is empty
	acceptCapped                    // acceptAllCap reached: the queue may hold more
	acceptFDLimit                   // EMFILE/ENFILE: counted in AcceptFDLimit
	acceptFailed                    // any other accept error: counted in AcceptOther
)

func (l *Loop) acceptAll(ctx context.Context, now int64) acceptStop {
	for i := 0; i < acceptAllCap; i++ {
		newFD, sa, err := unix.Accept4(l.listenFD, unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)
		if err != nil {
			switch err {
			case unix.EAGAIN:
				// Listen queue fully drained — ET edge satisfied.
				// (EWOULDBLOCK == EAGAIN on Linux, so one case covers both.)
				l.listenHot = false
				return acceptDrained
			case unix.EMFILE, unix.ENFILE:
				// Out of file descriptors (per-process / system-wide). The
				// SYN at the head of the queue is un-acceptable right now;
				// bare-continuing would busy-loop on the same EMFILE. Back
				// off this round and re-arm so we retry next iteration once
				// an fd may have freed up — with ET we can't just wait for a
				// new edge, the queued SYNs won't re-signal.
				l.errs.AcceptFDLimit.Add(1)
				l.listenHot = true
				return acceptFDLimit
			default:
				if l.acceptRetry(err) {
					// Transient: a SYN was aborted (RST before accept) or the
					// syscall was interrupted. The backlog may still hold
					// other connections, so retry this round rather than
					// returning — a bare return would strand them until the
					// next SYN edge.
					continue
				}
				// Unexpected accept error. Count it and stop this round; the
				// listen fd stays armed (ET) and a fresh edge re-enters.
				l.errs.AcceptOther.Add(1)
				l.listenHot = false
				return acceptFailed
			}
		}

		// Hard-cap check: reject FDs at or above the table's MAX size. An fd
		// >= connTableSize can never be indexed into l.conns, so it would be
		// silently dropped (ErrorConnTableCap++) and stay invisible. Latch
		// a one-shot Warn so a worker that has saturated its 64 K conn
		// table is diagnosable rather than just "connections vanishing".
		if newFD < 0 || newFD >= connTableSize {
			_ = unix.Close(newFD)
			l.errs.ConnTableCap.Add(1)
			l.fdCapDrops++
			if !l.fdCapWarned && l.logger != nil {
				l.fdCapWarned = true
				l.logger.Warn("accepted fd exceeds conn table cap; dropping",
					"loop", l.id, "fd", newFD, "cap", connTableSize,
					"hint", "worker is at its per-worker connection limit (connTableSize)")
			}
			continue
		}

		// Grow-on-demand: the table starts at connTableInitSize and grows
		// toward the connTableSize hard cap as higher fds arrive, so a worker
		// that only ever sees a few conns never pays the full 512 KiB. Rare
		// (only on a new high-water fd) and off the steady hot path.
		if newFD >= len(l.conns) {
			l.growConns(newFD)
		}

		_ = sockopts.ApplyFD(newFD, l.sockOpts)

		// EPOLLRDHUP: report peer half-close (FIN) as a distinct condition. The
		// short-read fast path in drainRead returns without a trailing EAGAIN
		// read, so a FIN piggybacked in the SAME readable edge as the request
		// (a client that writes a request then immediately closes) would
		// otherwise leave the EOF unread — and with EPOLLET no new edge fires
		// for a still-readable-at-EOF socket, stranding the conn until an idle
		// timeout (which may be unset). EPOLLRDHUP rides the same event, so the
		// run loop can close the conn after flushing its response.
		//
		// The registration's generation is drawn before the ADD and rides on
		// every event of the conn (celeris#885 item 18); the connState below
		// keeps it.
		gen := l.nextHTTPGen()
		ev := httpEpollEvent(newFD, gen, unix.EPOLLIN|unix.EPOLLET|unix.EPOLLRDHUP)
		if err := unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_ADD, newFD, &ev); err != nil {
			_ = unix.Close(newFD)
			l.errs.ConnRegister.Add(1)
			continue
		}

		// Tag the per-conn context with this worker's numeric ID so handlers
		// can call celeris.Context.WorkerID() and forward it to driver
		// pools (postgres.WithWorker / redis.WithWorker / memcached.WithWorker)
		// for per-CPU affinity between the HTTP request and any DB/cache
		// calls it makes.
		connCtx := ctxkit.WithWorkerID(ctx, l.id)
		cs := acquireConnState(connCtx, newFD, l.resolved.BufferSize, l.async)
		cs.gen = gen
		cs.remoteAddr = sockaddrString(newFD, sa)
		// Install under driverMu. An async Hijack clears its slot from the
		// dispatch goroutine under this lock and then closes the
		// descriptor, and the kernel's lowest-free-fd rule can hand the
		// number straight back to this accept: the install must be ordered
		// after that write (celeris#668). One uncontended lock per accept,
		// next to accept4, setsockopt and epoll_ctl.
		l.driverMu.Lock()
		l.conns[newFD] = cs
		l.driverMu.Unlock()
		l.addLiveConn(cs)
		l.connCount++
		if newFD > l.maxFD {
			l.maxFD = newFD
		}
		cs.writeFn = l.makeWriteFn(cs)
		l.activeConns.Add(1)
		l.acceptCount.Add(1)

		if l.cfg.OnConnect != nil {
			l.cfg.OnConnect(cs.remoteAddr)
		}

		cs.lastActivity = now

		// H2C + EnableH2Upgrade: defer protocol commit to detectProtocol
		// on first recv. Locking cs.protocol = H2C on accept routed
		// HTTP/1.1 upgrade requests into ProcessH2 instead of the H1
		// parser, so the server emitted its SETTINGS frame without the
		// mandatory 101 Switching Protocols response first. Mirrors the
		// iouring engine fix.
		if l.cfg.Protocol != engine.Auto &&
			(l.cfg.Protocol != engine.H2C || !l.cfg.EnableH2Upgrade) {
			cs.protocol = l.cfg.Protocol
			cs.detected = true
			l.initProtocol(cs)
		} else if l.cfg.ReadHeaderTimeout > 0 {
			// The protocol is decided by the first bytes, so there is no H1
			// state and no header deadline yet (initProtocol arms it at
			// detection). The conn gets the same absolute budget from accept:
			// a client that stays silent, sends "GE" and stalls, or dribbles
			// the HTTP/2 preface is reaped by checkTimeouts instead of living
			// on the activity stamp each of its bytes refreshes, until
			// ReadTimeout or IdleTimeout. io_uring does the same
			// (celeris#974).
			cs.detectDeadline = now + int64(l.cfg.ReadHeaderTimeout)
		}
	}
	// Cap hit without reaching EAGAIN: more SYNs may be queued. With ET we
	// won't get a fresh readiness edge for them, so re-arm to drain the
	// remainder on the next loop iteration (epoll_wait(0) via listenHot).
	l.listenHot = true
	return acceptCapped
}

// pauseAcceptRounds bounds acceptQueuedOnPause at connTableSize accepts, the
// most one loop can hold. The queue itself never holds more than the listen
// backlog; the bound only matters if handshakes keep landing faster than the
// loop takes them, and the listen socket must close regardless.
const pauseAcceptRounds = connTableSize / acceptAllCap

// acceptQueuedOnPause takes every connection still waiting in the listen
// socket's accept queue before a pausing loop closes that socket
// (celeris#662). Their handshakes completed before the pause and their
// clients may already have sent a request; the close would abort them.
//
// It can only reach what the ACCEPT QUEUE holds. While TCP_DEFER_ACCEPT is
// set the kernel keeps a handshake-complete connection that has sent no data
// out of that queue altogether, which is why a listener that has the option
// lingers before it gets here (stepAcceptPause): the option is cleared
// first, and by the deadline the kernel has promoted into this queue every
// connection it deferred before the clear.
//
// They go through acceptAll, the registration every accept uses, because they
// are ordinary connections: AcceptCount and ActiveConnections count them,
// OnConnect fires, sockopts apply, and a descriptor that cannot be registered
// (conn-table cap, EPOLL_CTL_ADD failure) is closed and counted exactly as it
// is there. EMFILE/ENFILE stops the drain (counted in AcceptFDLimit): no
// descriptor frees up while this loop spins, so what is still queued is left
// to the close.
//
// Interactions:
//   - The drained connections hold connCount above zero, so the paused loop
//     stays out of DRAINING→SUSPENDED and serves them, then parks once they
//     have closed or, on the adaptive engine, been transplanted — exactly as
//     for a connection accepted a moment before the pause.
//   - A pause that lands mid-iteration is seen at the top of the next one;
//     until then the loop accepts through acceptAll as usual.
//   - A ResumeAccept that races the drain loses nothing: the drain only
//     registers connections, the loop still closes the listener it read as
//     paused, and the next iteration re-creates it, as it did before.
//   - Not covered: a handshake still in progress at the close, or one that
//     completes between the final EAGAIN and the close. That is inherent to
//     closing a listen socket; only the kernel's tcp_migrate_req moves those
//     to another listener in the SO_REUSEPORT group.
//   - Not covered either: a connection deferred before the clear whose
//     promotion comes after the linger's deadline, which takes a lost
//     retransmitted SYN-ACK or a client that does not answer it.
func (l *Loop) acceptQueuedOnPause(ctx context.Context) {
	for range pauseAcceptRounds {
		if l.acceptAll(ctx, time.Now().UnixNano()) != acceptCapped {
			return
		}
	}
}

// stepAcceptPause runs one step of the accept pause on the loop thread, at
// the top of an iteration, while the listener is open and the engine is
// paused or this loop is lingering (celeris#662, celeris#675).
//
//   - ACTIVE → LINGERING (paused, no linger yet): clear TCP_DEFER_ACCEPT on
//     this loop's listener (and apply the tcp_synack_retries=0 guard when
//     the pause asked for it), then linger until deferlinger.Linger after
//     that clear. The listener stays registered, so connections keep being
//     accepted through acceptAll: a connection deferred before the clear is
//     promoted by the kernel about a second after its SYN and raises an
//     edge like any other, and one that arrives after the clear enters the
//     accept queue at once. A listener created without the option, or a
//     zero linger, closes at once instead: nothing on it can be deferred.
//   - LINGERING, before the deadline: nothing.
//   - LINGERING → CLOSED at the deadline: the drain-and-close.
//   - LINGERING → ACTIVE (a resume arrived first): the option goes back on
//     the same descriptor, which is never closed.
func (l *Loop) stepAcceptPause(ctx context.Context, paused bool) {
	switch {
	case !paused:
		deferlinger.Leave(l.listenFD, l.deferCapable, l.pause, l.logger, "loop", l.id)
		l.lingerUntil = 0
	case l.lingerUntil == 0:
		if !l.pause.Observed() {
			return // test hook only: model a loop that sees the pause late
		}
		if l.lingerUntil = deferlinger.Enter(l.listenFD, l.deferCapable, l.pause, l.logger, "loop", l.id); l.lingerUntil == 0 {
			l.closeListenerAfterDrain(ctx)
		}
	case l.lingerUntil.Passed():
		l.closeListenerAfterDrain(ctx)
	}
}

// closeListenerAfterDrain is the pause's close: serve every connection still
// in the accept queue (acceptQueuedOnPause), then take the listener out of
// epoll and close it, which removes it from the SO_REUSEPORT group. It is
// called only while the engine is paused, so it sets listenFDClosed itself --
// the value the iteration's own store sets right after -- before it wakes a
// PauseAccept waiting for it.
func (l *Loop) closeListenerAfterDrain(ctx context.Context) {
	// Connections still in the kernel accept queue completed their
	// handshake before the close and may already have sent a request.
	// Accept them through the normal path and serve them before the listen
	// socket goes: the close would otherwise abort them (celeris#662).
	l.acceptQueuedOnPause(ctx)
	_ = unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_DEL, l.listenFD, nil)
	_ = unix.Close(l.listenFD)
	l.listenFD = -1
	// No listen fd to drain — clear the re-arm flag so it doesn't pin
	// epoll_wait at 0ms (busy-spin) while paused/suspended.
	l.listenHot = false
	l.lingerUntil = 0
	deferlinger.NoteClose()
	l.listenFDClosed.Store(true)
	l.pause.Notify()
}

// lingerTimeoutMs caps an epoll_wait timeout at the time left to the pause
// linger's deadline, rounded up so the wait does not return just short of it
// and spin. A negative ms (block until an event) is capped too, so a
// lingering loop never sleeps through its deadline.
func (l *Loop) lingerTimeoutMs(ms int) int {
	left := l.lingerUntil.Left()
	if left <= 0 {
		return 0
	}
	if lm := int((left + time.Millisecond - 1) / time.Millisecond); ms < 0 || lm < ms {
		return lm
	}
	return ms
}

// growConns enlarges l.conns so that index fd is in range, doubling the
// length (power-of-two growth) until it covers fd, clamped to connTableSize.
// Caller (acceptAll) has already verified 0 <= fd < connTableSize.
//
// RACE INVARIANT: l.conns slots and header are read on the worker goroutine
// UNLOCKED (the hot path: drainRead, handleWritable, the run loop's flush
// passes) — same-goroutine as this grow, so those never tear. The
// cross-goroutine accessors are (1) RegisterConn (driver path) under
// l.driverMu, reading `fd < len(l.conns) && l.conns[fd] != nil`, and (2)
// hijackConn, which in async mode runs ON a dispatch goroutine and both reads
// the header and nils l.conns[fd]. growConns therefore performs BOTH the
// full-slot copy and the header swap under l.driverMu, and hijackConn takes
// l.driverMu for its l.conns read + slot write — so the grow's copy never
// reads a slot a hijack is concurrently niling, and no reader observes a torn
// header (base + len). Allocation stays outside the lock to keep the critical
// section to a copy + pointer store; growth is rare (only on a new high-water
// fd), so the lock is well off the steady hot path. The slot INSTALLS
// (acceptAll, attachAdoptedFD) take l.driverMu too, so installing a descriptor
// number a hijack has just released is ordered after the hijack's clear
// (celeris#668).
func (l *Loop) growConns(fd int) {
	newLen := len(l.conns)
	for newLen <= fd {
		newLen *= 2
	}
	if newLen > connTableSize {
		newLen = connTableSize
	}
	grown := make([]*connState, newLen)
	l.driverMu.Lock()
	copy(grown, l.conns)
	l.conns = grown
	l.driverMu.Unlock()
}

func (l *Loop) drainRead(fd int, now int64) {
	if fd < 0 || fd >= len(l.conns) {
		return
	}
	cs := l.conns[fd]
	if cs == nil {
		return
	}

	for {
		// Zero-copy body recv: when H1 is in a partial-body state we read
		// directly into the tail of H1State.bodyBuf — skipping the
		// cs.buf → bodyBuf memcpy that would otherwise happen on every
		// read for a multi-read POST. Disabled in async mode: the
		// dispatch goroutine owns h1State and the worker cannot safely
		// observe NextRecvBuf without synchronization.
		// While the protocol is undetected, bytes already received and too
		// few to decide on (detectN, at most detect.PrefaceLen-1) stay at the
		// head of cs.buf and this read lands after them (celeris#870).
		recvBuf := cs.buf[cs.detectN:]
		intoBody := false
		if !l.async && cs.h1State != nil {
			if b := cs.h1State.NextRecvBuf(); b != nil {
				recvBuf = b
				intoBody = true
			}
		}
		n, err := unix.Read(fd, recvBuf)
		if err != nil {
			if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
				return
			}
			// Surface read failure to detached middleware (e.g. WS).
			l.closeOnReadEnd(fd, cs, err)
			return
		}
		if n == 0 {
			// Surface peer-close (EOF) to detached middleware.
			l.closeOnReadEnd(fd, cs, errPeerClosed)
			return
		}

		cs.lastActivity = now
		// n > 0 here (the err and EOF cases returned above): bytes read on
		// this iteration, into either cs.buf or the zero-copy bodyBuf.
		l.bytesReadBatch += uint64(n)

		// Direct-into-bodyBuf completion path: we read straight into
		// H1State.bodyBuf; dispatch the handler if the body is full,
		// otherwise continue the recv loop for the next chunk.
		if intoBody {
			complete := cs.h1State.ConsumeBodyRecv(n)
			if !complete {
				continue
			}
			rest, verdict := cs.h1State.DispatchBufferedBody(cs.ctx, l.handler, cs.writeFn)
			if verdict == nil && len(rest) > 0 {
				// Tail bytes after the body: feed them back through the
				// normal parser for the pipelined request(s). Their verdict
				// is handled exactly as the body's own (an h2c upgrade
				// request pipelined behind the body is the case that was
				// once closed instead, celeris#885).
				verdict = conn.ProcessH1(cs.ctx, rest, cs.h1State, l.handler, cs.writeFn)
			}
			if verdict != nil {
				if errors.Is(verdict, conn.ErrHijacked) {
					return
				}
				if errors.Is(verdict, conn.ErrUpgradeH2C) {
					// The 101 and the preface go out here; the loop goes on
					// to read the HTTP/2 frames that follow.
					if !l.finishH1Upgrade(cs, fd, cs.writeFn) {
						return
					}
					continue
				}
				l.endH1Verdict(cs, verdict)
				return
			}
			if mu := cs.detachMu; mu != nil {
				mu.Lock()
			}
			queued := 0
			if cs.writeSince != 0 {
				queued = csPendingBytes(cs) // before this flush (celeris#885)
			}
			if err := l.flushWrites(cs, true); err != nil {
				if mu := cs.detachMu; mu != nil {
					mu.Unlock()
				}
				l.closeConn(fd)
				return
			}
			// Resync pendingBytes, as the inline flush below does: the write
			// hooks add every response to it, and only a flush point brings
			// it back to what is still queued. Left alone here it grew by
			// each response answered on this path, until the hooks refused
			// the writes of a conn that had nothing queued (celeris#761).
			dirty := csWritePending(cs)
			if dirty {
				cs.pendingBytes = csPendingBytes(cs)
				cs.noteWriteProgress(queued, cs.pendingBytes)
			} else {
				cs.pendingBytes = 0
				cs.endWrite()
			}
			refused := cs.writeRefused
			if mu := cs.detachMu; mu != nil {
				mu.Unlock()
			}
			if refused {
				l.closeWhenFlushed(cs)
				return
			}
			if dirty {
				// Send buffer full after the body-recv response flush. This
				// path is sync-only (intoBody is gated on !l.async), so the
				// conn is never detached — arm EPOLLOUT to avoid busy-polling.
				l.armEpollOut(cs)
			}
			continue
		}

		// data is everything received so far that no protocol has consumed:
		// this read plus, until detection succeeds, the bytes held back by an
		// earlier one.
		data := cs.buf[:int(cs.detectN)+n]

		if !cs.detected {
			proto, detectErr := detect.Detect(data)
			if detectErr == detect.ErrInsufficientData {
				// A first segment shorter than detection needs ("GE", or an
				// h2c preface cut before byte 24) is kept in place and the
				// next read appended to it. Reading every segment at the start
				// of cs.buf overwrote it, so GE + "T /x HTTP/1.1" parsed as
				// the method "T" and was answered 405, and a split preface
				// was lost. Detect decides within detect.PrefaceLen bytes, so
				// the held prefix is shorter than that and always leaves room
				// in cs.buf (BufferSize >= resource.MinBufferSize).
				cs.detectN = uint8(len(data))
				continue
			}
			if detectErr != nil {
				l.closeConn(fd)
				return
			}
			cs.detectN = 0
			cs.protocol = proto
			cs.detected = true
			// initProtocol arms the H1 header deadline at what is left of the
			// accept-time budget (detectDeadline). A deciding read that comes
			// after the budget finds the sweep, at most one timerfd period
			// (25 ms) behind, about to close the conn.
			l.initProtocol(cs)
			cs.detectDeadline = 0
		}

		// Async handler dispatch — goroutine-per-conn with an input buffer.
		//
		// The worker appends this read's bytes to cs.asyncInBuf. If a
		// dispatch goroutine is already draining the buffer for this
		// conn, it will pick up the new bytes on its next iteration. If
		// not, we spawn one. The goroutine holds cs.detachMu while
		// inside ProcessH1 so writeBuf/writeFn mutations serialize with
		// worker-initiated flush paths.
		//
		// This shape preserves HTTP/1.1 pipelining order guarantees —
		// ProcessH1's fast path drains multiple pipelined requests from
		// a single `data` slice in order, and the per-conn goroutine
		// processes any buffered batch monotonically. Matches
		// net/http's goroutine-per-conn model.
		//
		// Gated by Config.AsyncHandlers (or auto-enabled by hasAsyncRoutes
		// in doPrepare). The inline path below is unchanged and zero-cost
		// when async is off.
		// Per-handler async (celeris #300): only PROMOTED conns go straight
		// to the dispatch goroutine. A fresh async-mode HTTP1 conn first
		// tries the inline fast path below (InlineMode); ProcessH1 bails
		// with ErrAsyncDispatch on the first async route, promoting the
		// conn — so sync routes run inline on the event loop on a server
		// that mixes sync + async handlers.
		if l.async && cs.asyncPromoted && cs.protocol == engine.HTTP1 {
			cs.asyncInMu.Lock()
			// Backpressure: drop the conn if the dispatch goroutine is
			// falling behind. A client that pipelines faster than we
			// can process would otherwise grow asyncInBuf without bound.
			if len(cs.asyncInBuf)+len(data) > maxPendingInputBytes {
				cs.asyncInMu.Unlock()
				l.closeConn(fd)
				return
			}
			// Append directly into asyncInBuf — the goroutine swaps it
			// out via double-buffer under the same mutex before invoking
			// ProcessH1, so the worker's next read into cs.buf can't
			// overwrite in-flight bytes. Zero allocation on steady state.
			cs.asyncInBuf = append(cs.asyncInBuf, data...)
			starting := !cs.asyncRun
			if starting {
				cs.asyncRun = true
			}
			cs.asyncInMu.Unlock()
			if starting {
				l.asyncWG.Add(1)
				go l.runAsyncHandler(cs)
			} else {
				// Goroutine is parked in asyncCond.Wait — wake it.
				cs.asyncCond.Signal()
			}
			// celeris#626: count this recv. The dispatch path `continue`s,
			// so it never reaches the inline reqBatch++ below — before this
			// line an async-promoted conn stopped being counted entirely and
			// RequestCount (plus the adaptive controller's throughput and
			// BytesPerReq, which divides by it) silently flat-lined on the
			// busiest conns. Counted HERE, on the loop thread, so reqBatch
			// stays worker-thread-only exactly like bytesReadBatch and
			// addWrittenBytes(_, true); the dispatch goroutine must never
			// touch it. Mirrors iouring's asyncFeed site (worker.go).
			l.reqBatch++
			continue
		}

		// writeFn is read here — NOT before the async-dispatch branch above.
		// A promoted-async conn is owned by its dispatch goroutine, which can
		// rewrite cs.writeFn during Context.Detach (the OnDetach callback at
		// initProtocol). The async branch does not use this local, so reading
		// it only on the inline path keeps the event loop from racing that
		// write (the pre-existing loop.go:989-vs-1458 data race).
		writeFn := cs.writeFn

		var processErr error
		// Stash worker-local "now" on H1State so populateCachedStream can
		// copy it to the stream — HandleStream skips a per-request
		// time.Now() vDSO call.
		if cs.h1State != nil {
			cs.h1State.NowNs = now
		}
		// Per-handler async (celeris #300): on an async-mode HTTP1 conn not
		// yet promoted, run ProcessH1 inline in InlineMode so it bails
		// (ErrAsyncDispatch) on the first async route.
		tryInline := l.async && !cs.asyncPromoted && cs.h1State != nil &&
			cs.protocol == engine.HTTP1
		switch cs.protocol {
		case engine.HTTP1:
			if tryInline {
				cs.h1State.InlineMode = true
			}
			processErr = conn.ProcessH1(cs.ctx, data, cs.h1State, l.handler, writeFn)
			// A handler that ran inline and hijacked has had cs released to
			// the pool inside the Hijack call (hijackConn's inline branch):
			// cs.h1State is nil, and cs may already be another accept's
			// connection. So cs is not touched again once ProcessH1 reports
			// the hijack; the ErrHijacked return below is the only way out.
			if tryInline && !errors.Is(processErr, conn.ErrHijacked) {
				cs.h1State.InlineMode = false
			}
			if errors.Is(processErr, conn.ErrAsyncDispatch) {
				// Async route: promote the conn and hand the stashed
				// request (+ any pipelined bytes) to the dispatch
				// goroutine. Any response already written inline (a
				// preceding pipelined sync request) stays in cs.writeBuf
				// and is flushed in order by the dispatch path.
				cs.asyncPromoted = true
				l.asyncPromoted.Add(1)
				stashed := cs.h1State.TakeBufferedBytes()
				cs.asyncInMu.Lock()
				cs.asyncInBuf = append(cs.asyncInBuf, stashed...)
				starting := !cs.asyncRun
				if starting {
					cs.asyncRun = true
				}
				cs.asyncInMu.Unlock()
				if starting {
					l.asyncWG.Add(1)
					go l.runAsyncHandler(cs)
				} else {
					cs.asyncCond.Signal()
				}
				// celeris#626: count the promoting recv exactly once, here.
				// This branch `continue`s, so the inline reqBatch++ below is
				// NOT also reached — no double count at the promotion
				// boundary. The stashed bytes are replayed by the dispatch
				// goroutine, which does not count; every later recv on this
				// conn is counted by the asyncFeed site above. Mirrors
				// iouring's promoteConnToAsync.
				l.reqBatch++
				continue
			}
			if errors.Is(processErr, conn.ErrUpgradeH2C) {
				// H1→H2 promotion. switchToH2 consumes the upgrade info,
				// installs H2 state, and feeds any preface bytes synchronously.
				// Flush the buffered 101 Switching Protocols response and any
				// H2 server preface + stream 1 response bytes that switchToH2
				// may have queued. The normal inline-flush block below is
				// skipped by the `continue` so we must flush explicitly here,
				// otherwise the client blocks forever waiting for the 101.
				if !l.finishH1Upgrade(cs, fd, writeFn) {
					return
				}
				// Fall through to continue the loop; subsequent reads go
				// through the H2 path via cs.protocol switch above.
				continue
			}
		case engine.H2C:
			// Async-mode conns: serialize inline ProcessH2 against the
			// runAsyncHandler goroutine that owns cs.writeBuf until its
			// H1→H2 upgrade-flush (flushWrites) completes. Without the
			// lock, a new recv arriving while the goroutine is mid-
			// flush runs ProcessH2 → writeFn → cs.writeBuf manipulation
			// concurrent with the goroutine's flushWrites — a data
			// race caught by matrixBenchStrict (mirrors the iouring
			// fix in internal/engine/iouring/worker.go).
			if cs.detachMu != nil {
				cs.detachMu.Lock()
				processErr = conn.ProcessH2(cs.ctx, data, cs.h2State, l.handler, writeFn, l.h2cfg)
				cs.detachMu.Unlock()
			} else {
				processErr = conn.ProcessH2(cs.ctx, data, cs.h2State, l.handler, writeFn, l.h2cfg)
			}
		}

		// Per-handler async (celeris #300): inline ProcessH1 handled the
		// request(s). If it left partial state (buffered headers /
		// accumulating body), promote so the continuation runs on the
		// dispatch goroutine — the partial-state parse paths must not run
		// inline (only the fresh-parse site honors the async check).
		if tryInline && processErr == nil && cs.h1State.HasPendingData() {
			cs.asyncPromoted = true
			l.asyncPromoted.Add(1)
		}

		l.reqBatch++

		// lastActivity already set above; timeout checked in checkTimeouts.

		if processErr != nil {
			if errors.Is(processErr, conn.ErrHijacked) {
				return // FD already detached — do not close or flush
			}
			// Flush any pending writes (e.g. error responses) before closing,
			// and tell a detached middleware: endH1Verdict.
			l.endH1Verdict(cs, processErr)
			return
		}

		// Inline flush: send response immediately after the handler returns,
		// before the next unix.Read (which returns EAGAIN on non-pipelined
		// connections). This sends the response one event-batch earlier than
		// the dirty list pass, reducing per-request latency by up to N×2µs
		// (where N is the number of other events in the same epoll_wait batch).
		if mu := cs.detachMu; mu != nil {
			mu.Lock()
		}
		if csWritePending(cs) {
			queued := 0
			if cs.writeSince != 0 {
				queued = csPendingBytes(cs) // before this flush (celeris#885)
			}
			if fErr := l.flushWrites(cs, true); fErr != nil {
				if cs.h1State != nil && cs.h1State.OnError != nil {
					cs.h1State.OnError(fErr)
				}
				if mu := cs.detachMu; mu != nil {
					mu.Unlock()
				}
				if cs.dirty {
					l.removeDirty(cs)
				}
				l.closeConn(fd)
				return
			}
			if !csWritePending(cs) {
				// Fully flushed (including any sendfile body) — no dirty
				// list / EPOLLOUT needed.
				cs.pendingBytes = 0
				cs.endWrite()
				if cs.dirty {
					l.removeDirty(cs)
				}
				l.disarmEpollOut(cs)
			} else {
				// Partial write — the kernel send buffer is full (write
				// backpressure). Sync pendingBytes and arm EPOLLOUT
				// instead of busy-retrying via the dirty list, which
				// would spin epoll_wait(0)→write(EAGAIN) at 100% CPU, a
				// truly-detached WS/SSE conn included (celeris#885 item 42).
				// A pending sendfile is resumed by handleWritable on the next
				// EPOLLOUT edge.
				cs.pendingBytes = csPendingBytes(cs)
				// A client that sends while it reads (an HTTP/2 PING or
				// WINDOW_UPDATE, a pipelined request) raises an EPOLLIN event
				// per segment, and this flush takes the room it made: that is
				// progress for the write clock, and no EPOLLOUT edge may come
				// to show it (celeris#885 items 35 and 45).
				cs.noteWriteProgress(queued, cs.pendingBytes)
				l.armEpollOut(cs)
			}
		}
		// Read writeRefused inside the lock so the check below is safe
		// against concurrent goroutine writes via the guarded writeFn.
		refused := cs.writeRefused
		if mu := cs.detachMu; mu != nil {
			mu.Unlock()
		}

		// Back-pressure close: a write hook refused bytes because the
		// backlog before them was over writeCap, a peer that stopped
		// reading while it kept sending requests. A backlog over the cap
		// by itself is not a reason to close: one response larger than the
		// cap puts it there, and so does a sendfile body. Closing on it cut
		// such a response off mid-body (celeris#761).
		if refused {
			l.closeWhenFlushed(cs)
			return
		}

		// Detached WS/SSE backpressure: if the middleware signaled pause
		// during this iteration (recvPauseDesired toggled by chanReader
		// crossing high-water during Append), apply EPOLL_CTL_MOD inline
		// and stop reading. Without this, a large single message can
		// over-fill the channel within one drainRead pass — pause would
		// only land on the NEXT iteration via drainDetachQueue, too late
		// to prevent overflow → chanReader drop → ErrReadLimit.
		if cs.detachMu != nil && cs.recvPauseDesired.Load() && !cs.recvPaused {
			l.setRecvPaused(cs, true)
			return
		}

		// Short read: socket is provably drained (read returned fewer bytes
		// than the buffer can hold). Skip the EAGAIN-producing read that would
		// otherwise cost one wasted syscall per request. Edge-triggered epoll
		// will notify when new data arrives on this fd.
		if n < len(recvBuf) {
			return
		}
	}
}

// drainH2Writes drains the H2 async write queues. Handler goroutines enqueue
// response frame bytes; the loop drains them into writeBuf and flushes to the
// wire. Extracted from run so the pass can be driven by a test (celeris#889
// item 1). Loop thread.
//
// By index, from the end: a close below swap-removes the conn from h2Conns,
// which a range would then skip one entry past.
func (l *Loop) drainH2Writes() {
	for i := len(l.h2Conns) - 1; i >= 0; i-- {
		fd := l.h2Conns[i]
		cs := l.conns[fd]
		if cs != nil && cs.h2State != nil && cs.h2State.WriteQueuePending() {
			if cs.writeRefused {
				// A frame was refused earlier and the conn is closing
				// (closeWhenFlushed): what its handlers go on queueing is
				// dropped, as io_uring's write hook does on a closing
				// conn, not appended behind a backlog that has fallen
				// below the cap, where it would go out after the cut
				// frames and the drain clock would count it as the peer
				// taking more. The close is requested once: a second
				// closeWhenFlushed per frame was an EPOLL_CTL_MOD each
				// (celeris#885 item 38).
				cs.h2State.DrainWriteQueue(discardWrite)
				if cs.closeSince == 0 {
					l.closeWhenFlushed(cs)
				}
				continue
			}
			cs.h2State.DrainWriteQueue(cs.writeFn)
			if cs.writeRefused {
				// A frame refused on back-pressure is lost, and the
				// connection's framing with it: close (celeris#761).
				l.closeWhenFlushed(cs)
				continue
			}
			if cs.writePos < len(cs.writeBuf) {
				queued := 0
				if cs.writeSince != 0 {
					queued = csPendingBytes(cs) // before this flush (celeris#885)
				}
				if fErr := l.flushWrites(cs, true); fErr != nil {
					l.removeDirty(cs)
					l.closeConn(fd)
					continue
				} else if cs.writePos < len(cs.writeBuf) {
					// Send buffer full: arm EPOLLOUT instead of the dirty
					// list so we don't busy-poll the backpressured H2 conn.
					cs.noteWriteProgress(queued, csPendingBytes(cs))
					l.armEpollOut(cs)
				} else {
					cs.endWrite()
				}
			}
			// Resync pendingBytes, as every other flush point does: the
			// write hook added every frame drained above to it, and left
			// alone it outgrew what is really queued until the hook
			// refused frames of a conn that had nothing queued
			// (celeris#761).
			cs.pendingBytes = csPendingBytes(cs)
		}
	}
}

// testHookCloseWhenFlushed, when set, runs at the start of every
// closeWhenFlushed, on the loop's goroutine. The H2 closing test counts the
// requests a conn makes: each one past the first is an EPOLL_CTL_MOD
// (celeris#885 item 38). An atomic pointer, as testHookCheckTimeouts is; a load
// and a nil test per close request, which is never a hot path.
var testHookCloseWhenFlushed atomic.Pointer[func(*connState)]

// discardWrite is the write function the run loop drains the H2 write queue of
// a conn that is closing on a refused write into: the frames go nowhere.
func discardWrite([]byte) {}

// closeWhenFlushed closes cs as closeConn does, but only once the response
// bytes it still has queued have reached the kernel: closeConn's SHUT_WR
// commits only what the kernel has already taken, so a response larger than
// the socket buffers lost its tail when the conn was closed right after it
// (Connection: close, a request error, a refused write; celeris#761). It
// stops reading cs, so no request is parsed or answered on a conn that is
// closing, arms EPOLLOUT and marks the close deferred (peerClosed), which
// handleWritable and the dirty pass carry out once the conn has drained. A
// peer that never reads again is reaped by checkTimeouts, WriteTimeout after
// the close was asked or after it last took some of the response
// (closingDrainBound, celeris#876). A conn with nothing left to send, and a
// truly-detached one, whose middleware owns its close, are closed at once.
//
// Loop thread, with no handler of cs's running: the inline path after its
// handler returned, or drainDetachQueue after the dispatch goroutine exited.
func (l *Loop) closeWhenFlushed(cs *connState) {
	if h := testHookCloseWhenFlushed.Load(); h != nil {
		(*h)(cs)
	}
	if (cs.h1State != nil && cs.h1State.Detached.Load()) || !csWritePending(cs) {
		l.closeConn(cs.fd)
		return
	}
	l.markClosing(cs, csPendingBytes(cs))
	l.removeDirty(cs)
	issued, err := l.modEpollOut(cs, unix.EPOLLOUT|unix.EPOLLET|unix.EPOLLRDHUP)
	if !issued {
		return // hijacked: drainDetachQueue settles the conn
	}
	if err != nil {
		l.closeConn(cs.fd)
		return
	}
	cs.epollOut = true
}

// markClosing defers cs's close until its queued response has gone out
// (peerClosed) and starts the clock of that drain (celeris#876). Both callers,
// closeWhenFlushed and the EPOLLRDHUP branch of onPeerHalfClose, have bytes
// queued; neither reads cs again. The wall clock, not cachedNow: a loop with no
// events leaves cachedNow as old as its last event, and a clock stamped with it
// would be as stale as the lastActivity it replaces.
//
// The clock starts once. Both callers can run again on a conn that is already
// closing, with nothing sent in between: an H2 conn whose write was refused
// stays on h2Conns with writeRefused set, and the run loop's pass used to
// re-enter closeWhenFlushed for every frame a handler went on queueing (it now
// drops those frames and asks for the close once, celeris#885 item 38); an
// EPOLLET RDHUP bit is reported again with each later EPOLLOUT edge. A restart
// there would let a peer that takes nothing hold the conn for as long as a
// handler keeps writing (an SSE stream: for ever). Only evidence that the peer
// is taking the response moves the clock (noteClosingProgress,
// noteClosingAcked). Loop thread.
//
// pending is the number of bytes queued for cs now (csPendingBytes), the
// baseline noteClosingProgress compares the next flush against. The caller
// counts it: where a dispatch goroutine can be writing, that is under detachMu
// (onPeerHalfClose, celeris#885 item 36), and markClosing reads nothing of cs
// that the lock guards.
func (l *Loop) markClosing(cs *connState, pending int) {
	cs.peerClosed = true
	if cs.closeSince != 0 {
		return
	}
	cs.closeSince = time.Now().UnixNano()
	cs.closePending = pending
}

// noteClosingProgress restarts the drain clock of a conn markClosing deferred
// when pending, what is queued for it after a flush, is less than it was: the
// peer is taking the response, and the bound is how long it may take nothing,
// not how long the drain may run, as for io_uring's closing drain
// (celeris#761, celeris#805). Called from the two flush sites that return to
// EPOLLOUT or the dirty list with bytes left, never from flushWrites. Loop
// thread.
//
// This is one of two kinds of evidence that the peer is taking bytes, and the
// coarser: the userspace queue shrinks only at a flush, and a flush follows an
// EPOLLOUT edge, which a socket raises once about half of its send queue has
// drained. A client taking 64 KiB/s of a response behind a send queue of MiB
// goes a minute between two edges. noteClosingAcked is the other, and sees
// those bytes go.
func (cs *connState) noteClosingProgress(pending int) {
	if pending < cs.closePending {
		cs.closeSince = time.Now().UnixNano()
	}
	cs.closePending = pending
}

// noteWriteProgress restarts the write clock (writeSince) of a live conn when
// a flush took bytes of what was queued: pending, what is queued after it, is
// less than queued, what was queued just before it, counted by the caller in
// the same locked region (celeris#885 items 35 and 45). Not against the
// previous flush's figure, as noteClosingProgress does: an async handler or a
// pipelined request appends between two flushes, and a peer that is taking the
// response would then look stalled. The peer taking bytes, not the response
// ending, is what WriteTimeout bounds on a native engine (std's WriteTimeout is
// an absolute deadline for the whole response). queued is 0 for a conn without
// the clock, which makes this a no-op. Called from the partial branches of
// every flush that leaves bytes queued and parks the conn: handleWritable,
// flushDirty, the HTTP/2 queue drain, drainRead's inline flush and its
// body-recv flush, finishH1Upgrade and flushCarriedReplay (the last two
// before armEpollOut, which keeps a clock that is set). Never from flushWrites,
// which every response's first flush goes through. Loop thread.
func (cs *connState) noteWriteProgress(queued, pending int) {
	if cs.writeSince != 0 && (pending < queued || cs.goMoved) {
		cs.writeSince = time.Now().UnixNano()
	}
	cs.goMoved = false
}

// resyncPending is the dispatch goroutine's resync of pendingBytes after its
// own flush (serveAsync, under detachMu): pendingBytes becomes what is
// really queued. When the flush took bytes and left the rest queued it also
// notes goMoved, which the loop's next partial flush of the conn counts as
// progress for the write clock (noteWriteProgress): without it, bytes the
// goroutine's flush put into the socket after the peer took some are invisible
// to the clock, the loop's flush that follows finds the socket full, and the
// sweep closes a conn whose client read a moment ago (celeris#885 items 35 and 45,
// review). pendingBytes before the resync is what was queued before the flush
// (the write function adds each response to it).
func (cs *connState) resyncPending() {
	if cs.writePos >= len(cs.writeBuf) && len(cs.bodyBuf) == 0 {
		// Emptied: a queue the clock was running on is as much progress as
		// one that is partly sent, and the loop would not see it (the sweep
		// ends the clock of an empty queue, but a clocked queue that the
		// goroutine emptied and the next response refilled looks unmoved).
		if cs.pendingBytes > 0 {
			cs.goMoved = true
		}
		cs.pendingBytes = 0
		return
	}
	pending := len(cs.writeBuf) - cs.writePos + len(cs.bodyBuf)
	if pending < cs.pendingBytes {
		cs.goMoved = true
	}
	cs.pendingBytes = pending
}

// endWrite ends the write clock of a conn whose queue is empty (celeris#885
// items 35 and 45), at the drain sites that know it. It folds the clock into
// lastActivity first: ReadTimeout and IdleTimeout of the next request are
// measured from the end of the response, as std does after it, not from the
// request before it, whose read may be longer ago than the timeouts when the
// response was large. The guard is all that is inlined into the drain sites
// (a load and a branch, for the common conn that never parked); the body is
// endWriteSlow. Loop thread (the dispatch goroutine's own flushes do not call
// it; resyncPending notes them for the clock, and checkTimeouts ends the clock
// of a conn whose queue it finds empty).
func (cs *connState) endWrite() {
	if cs.writeSince != 0 {
		cs.endWriteSlow()
	}
}

// endWriteSlow is endWrite's body, out of line so that endWrite's guard
// inlines at its call sites (the drain paths of every response).
func (cs *connState) endWriteSlow() {
	cs.writeSince = 0
	cs.lastActivity = time.Now().UnixNano()
}

// noteClosingAcked is the sweep's evidence of progress on a closing conn: the
// number of bytes the peer has acknowledged (tcp_info bytes_acked, which only
// grows) moved since the last sample. It restarts the drain clock the same way
// a flush that sent bytes does (noteClosingProgress). The first sample of a
// conn is its baseline and counts as no progress, unless the conn is first met after its bound had already passed (a loop whose
// sweeps are further apart than a short WriteTimeout): there is no baseline to
// compare with, and the conn is given one more bound rather than cut on no
// evidence.
//
// Not on the hot path: checkTimeouts calls it for a closing conn, at most
// every quarter of the bound (closeSampled) and once more before it reaps one,
// and it costs one getsockopt. A
// descriptor that is not a TCP socket yields no evidence, which leaves the
// userspace queue's rule alone. Loop thread.
func (l *Loop) noteClosingAcked(cs *connState, now int64) {
	cs.closeSampled = now
	ti, err := unix.GetsockoptTCPInfo(cs.fd, unix.IPPROTO_TCP, unix.TCP_INFO)
	if err != nil {
		return
	}
	known, prev := cs.closeAckedKnown, cs.closeAcked
	cs.closeAcked, cs.closeAckedKnown = ti.Bytes_acked, true
	var progressed bool
	if known {
		progressed = ti.Bytes_acked > prev
	} else {
		progressed = now-cs.closeSince > int64(l.closingDrainBound())
	}
	if progressed {
		cs.closeSince = now
	}
}

// closingDrainBound is how long a conn markClosing deferred may go without the
// peer taking a byte of its response: WriteTimeout, the bound a live conn
// stalled on a write gets (checkTimeouts). "Taking a byte" is the userspace
// queue shrinking at a flush (noteClosingProgress) or the peer acknowledging
// more bytes (noteClosingAcked); a reader the kernel is still draining to is
// never the stalled peer this bounds. WriteTimeout 0 is "no bound" for a live
// conn, but a closing conn has no handler and no request to wait for, so one
// whose peer reads nothing would hold its descriptor for ever; it gets
// closingDrainFloor, as io_uring's drain does.
func (l *Loop) closingDrainBound() time.Duration {
	if wt := l.cfg.WriteTimeout; wt > 0 {
		return wt
	}
	return closingDrainFloor
}

// closeOnReadEnd is drainRead's read-error and EOF branch: flush what is
// queued, surface err to a detached middleware (OnError, under detachMu, as
// every OnError site in this file runs), and close.
//
// When detachMu is held across a handler (dispatchBusy), waiting for the
// lock parked the loop — and every connection on it — until the handler
// returned (celeris#669); the common shape is a client that gives up on a
// slow handler and disconnects. So the flush and the notification ride on
// the close instead: closeConn leaves it to the dispatch goroutine, which
// exits at its next check, and then runs it on this thread with the lock
// free, delivering closeErr first.
func (l *Loop) closeOnReadEnd(fd int, cs *connState, err error) {
	mu := cs.detachMu
	if mu != nil && !mu.TryLock() {
		if dispatchBusy(cs, nil) {
			// The first error is the conn's story (celeris#867): a later one
			// is the same death seen again, and must not replace it.
			if cs.closeErr == nil {
				cs.closeErr = err
			}
			l.closeConn(fd)
			return
		}
		mu.Lock()
	}
	_ = l.flushWrites(cs, true)
	// An error parked by an earlier read-end that met the running handler is
	// the conn's story, even when the handler has since returned and its
	// hand-back is not drained yet: deliver it, once, in place of err, which
	// is that death seen again (celeris#867). Left parked, closeConn would
	// deliver it after err.
	if p := cs.closeErr; p != nil {
		err = p
		cs.closeErr = nil
	}
	if cs.h1State != nil && cs.h1State.OnError != nil {
		cs.h1State.OnError(err)
	}
	if mu != nil {
		mu.Unlock()
	}
	l.closeConn(fd)
}

// dispatchBusy reports whether cs's dispatch goroutine may be holding
// cs.detachMu across a user handler: it is alive (asyncRun), not parked in
// asyncCond.Wait (asyncParked), and has not released the lock for good at a
// Detach (asyncDetachUnlocked). All three are read under asyncInMu. Loop
// thread.
//
// It is how a loop-thread site that found cs.detachMu held tells the holders
// apart (celeris#669). The dispatch goroutine holds the lock across
// ProcessH1, i.e. for as long as the handler runs, and it is running
// whenever it does. Every other holder — a detached conn's guarded writeFn,
// the goroutine's own asyncClosed re-check — holds it for one write or less.
// So a site that finds the lock held while this reports true must not wait,
// and one that finds it held while this reports false may wait as it always
// has: that wait is bounded.
//
// After Detach the goroutine never takes the lock across ProcessH1 again, so
// it is excluded even while it runs — a handler may keep streaming inline
// after Detach, and a close left to it would wait for that handler, which in
// turn waits for the close's OnDetachClose to learn it should stop.
//
// If owe is non-nil and the result is true, *owe is set in the same critical
// section: the goroutine reads it under asyncInMu on its way to its next park
// or its exit, so it cannot miss it. Both owed hand-backs (closeOwed,
// relinkOwed) are recorded this way.
//
// A running goroutine is not necessarily the holder, and every caller acts on
// a true only by leaving work the goroutine hands back; a false positive
// costs a hand-back, never a lost write or close. Parked is exact: asyncParked
// is set and cleared under asyncInMu in the park loop's own critical section,
// so a goroutine seen parked can leave Wait only by re-taking asyncInMu after
// this read.
func dispatchBusy(cs *connState, owe *bool) bool {
	if cs.asyncCond.L == nil {
		return false // no async machinery: sync mode, no dispatch goroutine
	}
	cs.asyncInMu.Lock()
	busy := cs.asyncRun && !cs.asyncParked && !cs.asyncDetachUnlocked
	if busy && owe != nil {
		*owe = true
	}
	cs.asyncInMu.Unlock()
	return busy
}

// testHookHijackReleased, when set, runs in hijackConn's inline branch on the
// loop goroutine, right after releaseConnState has put cs back in the pool
// and before the Hijack call returns to the handler: the window in which
// another accept can take cs (celeris#885 item 6, #890 item 16). A
// test acquires the pooled state there, normally on the P that just put it
// (a preemption between Put and Get can move the goroutine to another P, and
// sync.Pool then misses; the tests count their hits), to force that reuse
// instead of waiting for it. The hook must not keep using cs as released
// memory: it is the pool's now, so the test compares it with what the pool
// hands back. An atomic pointer, as testHookCloseWhenFlushed is; one load per
// inline hijack, which already makes an EPOLL_CTL_DEL and a net.FileConn
// (itself a dup).
var testHookHijackReleased atomic.Pointer[func(l *Loop, cs *connState)]

func (l *Loop) hijackConn(fd int) (net.Conn, error) {
	// l.conns access is guarded by driverMu: in async mode this runs on a
	// dispatch goroutine, so the read of the header/slot must be serialized
	// against growConns' concurrent copy + header swap (see growConns).
	l.driverMu.Lock()
	cs := l.conns[fd]
	l.driverMu.Unlock()
	if cs == nil {
		return nil, errors.New("celeris: connection not found")
	}
	// Which thread is this? A dispatch goroutine is alive for this conn only
	// once the conn was promoted to one, and from then on the handler — and
	// so this call — runs ON that goroutine (inside ProcessH1 → ErrHijacked).
	// Otherwise the call is inline on the loop thread: sync mode, or an
	// async-mode conn not yet promoted running its first request inline.
	// asyncRun is read under asyncInMu, the lock the goroutine's lifetime is
	// published under.
	offThread := false
	if cs.detachMu != nil {
		cs.asyncInMu.Lock()
		offThread = cs.asyncRun
		cs.asyncInMu.Unlock()
	}
	// Detach the conn from the engine: drop it from epoll and the conn table,
	// and move the public gauges.
	//
	// WHAT RUNS WHERE (celeris#668). Off-thread, this may touch only what is
	// serialized against the loop:
	//
	//   - EPOLL_CTL_DEL is a kernel call and needs no lock; after it the
	//     worker sees no further EPOLLIN for this fd.
	//   - the l.conns[fd] write takes driverMu, so it cannot race growConns'
	//     copy + header swap, RegisterConn's occupancy check, or closeConn's
	//     capture and ownership re-check (celeris#654).
	//   - activeConns and closeCount are atomics, and so is hijacked.
	//
	// The live set and connCount are the loop's (see the Loop fields), and so
	// are the sweep's dormancy fields that removeLiveConn writes. Changing
	// them from here raced the index walks of checkTimeouts, the sweep and
	// shutdown, and connCount-- could lose an update against acceptAll's
	// connCount++, after which the connCount == 0 DRAINING→SUSPENDED gate
	// never passes on this loop again. So off-thread this enqueues cs for
	// drainDetachQueue's hijacked branch, which does both on the loop at its
	// next iteration — not when this goroutine exits, which for a handler
	// that serves the hijacked conn itself is the end of that session.
	// Until then the entry stays in the live set and the walkers skip it on
	// hijacked, which is stored BEFORE the descriptor is released: by the
	// time the kernel can reissue the number to another accept, the entry
	// already reads as not the loop's.
	_ = unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_DEL, fd, nil)
	if offThread {
		cs.hijacked.Store(true)
	} else {
		l.removeLiveConn(cs)
	}
	l.driverMu.Lock()
	l.conns[fd] = nil
	l.driverMu.Unlock()
	if !offThread {
		l.connCount--
	}
	l.activeConns.Add(-1)
	l.closeCount.Add(1)

	f := os.NewFile(uintptr(fd), "tcp")
	c, err := net.FileConn(f)
	_ = f.Close()

	// CRITICAL (#3.1): off-thread, never release cs to the pool. When the
	// handler calls Hijack from inside the dispatch goroutine's ProcessH1, the
	// goroutine STILL touches cs after ProcessH1 returns ErrHijacked — it
	// Unlocks cs.detachMu, resyncs cs.pendingBytes, sets cs.asyncClosed,
	// clears cs.asyncInBuf, and enqueues cs on detachQueue — and detach-queue
	// entries made before the hijack (a partial flush of a pipelined
	// response) may still name it. Recycling cs would hand pooled-and-reissued
	// memory to all of them, so it is left to the garbage collector, as
	// closeConn leaves every conn a goroutine may still reference
	// (celeris#668). The enqueue below is the loop's notice. Not pooling
	// leaves no sendfile dup open: only an async conn is hijacked off-thread,
	// and initProtocol installs the sendfile hook in sync mode only, so
	// cs.sendfile is always nil here.
	//
	// Inline, drainRead returns immediately on ErrHijacked without touching
	// cs again and nothing will enqueue cs, so release synchronously here —
	// gating only on detachMu would leak the connState in the inline-async
	// case.
	if offThread {
		l.enqueueDetach(cs)
	} else {
		// Unlink before the pool release. releaseConnState clears cs's own
		// dirty links but never repairs l.dirtyHead or a predecessor's
		// dirtyNext, so handing back a still-linked connState leaves the
		// loop's dirty pass walking into pooled, reissued memory
		// (celeris#654). This branch is the inline one — sync mode, or an
		// async conn not yet promoted — so it is on the loop thread and may
		// touch the list; the deferred branch is unlinked in
		// drainDetachQueue, which also runs there.
		l.removeDirty(cs)
		l.dropAsk(cs) // celeris#657 P8: never pool a connState an ask still names
		// The request's strings are views of cs.buf (celeris#733). The
		// handler is still running, and a hijacking handler typically keeps
		// the path, params and headers for the goroutine that will serve
		// the connection, so the buffer must not go back to the pool with
		// cs: the next connection to take cs from the pool would receive its
		// request into it, on any worker, even before this handler returns.
		// Dropping it costs one buffer allocation per hijack, when that
		// connState is next acquired.
		cs.buf = nil
		releaseConnState(cs)
		if h := testHookHijackReleased.Load(); h != nil {
			(*h)(l, cs)
		}
	}
	return c, err
}

func (l *Loop) initProtocol(cs *connState) {
	switch cs.protocol {
	case engine.HTTP1:
		cs.h1State = conn.NewH1State()
		cs.h1State.RemoteAddr = cs.remoteAddr
		cs.h1State.MaxRequestBodySize = l.cfg.MaxRequestBodySize
		cs.h1State.OnExpectContinue = l.cfg.OnExpectContinue
		cs.h1State.EnableH2Upgrade = l.cfg.EnableH2Upgrade
		cs.h1State.WorkerID = int32(l.id)
		cs.h1State.WorkerIDSet = true
		// Slowloris defence: configure + arm the read-header deadline.
		// ProcessH1 clears on successful parse and rearms on return-nil
		// (waiting for next request); checkTimeouts closes any conn
		// that exceeds the deadline.
		//
		// A conn that had to be detected carries the deadline it was given at
		// accept (detectDeadline): the first request's budget counts from
		// accept, so the H1 deadline is armed at what is left of it and
		// detection does not start it again. 0 for a conn whose protocol was
		// fixed at accept, or adopted, which arms the full budget.
		cs.h1State.ReadHeaderTimeoutNs = int64(l.cfg.ReadHeaderTimeout)
		cs.h1State.ArmHeaderDeadlineAt(cs.detectDeadline)
		// Per-handler async (celeris #300): wire the route resolver so
		// ProcessH1, when run inline on the event loop (InlineMode), can
		// detect an async route and bail to the dispatch goroutine. Only
		// meaningful in async mode; gated on HasAsyncRoutes so pure-sync
		// servers running with Config.AsyncHandlers=true don't pay the
		// per-recv resolver call.
		if l.async {
			if r, ok := l.handler.(stream.AsyncRouteResolver); ok && r.HasAsyncRoutes() {
				cs.h1State.RouteAsync = r.RouteAsync
			}
		}
		if !l.cfg.EnableH2Upgrade {
			cs.h1State.DisableH2CDetect()
		}
		// Back-pressure for HTTP/1 is held per request (celeris#761): a
		// request that finds the conn's unsent responses over the limit is
		// not served, and the conn is closed once they have gone out.
		cs.h1State.WriteBacklogged = cs.overBacklogH1
		// The streaming write-error query (celeris#884 item 45, #885 item
		// 30): a StreamWriter's Write, Flush and Close ask it after the
		// write hook, which has no error return. Detach swaps in one that
		// takes detachMu.
		cs.h1State.SetWriteErrQuery(cs)
		// Scatter-gather body writer: handler hands large bodies to the
		// engine as a zero-copy slice, which it writes at once with
		// writev(2) of [headers, body], saving the respBuf → writeBuf
		// memcpy (see makeWriteBodyFn). Disabled in async mode because
		// cs.bodyBuf access would race with the dispatch goroutine
		// without a mutex.
		if !l.async {
			cs.h1State.SetWriteBodyFn(l.makeWriteBodyFn(cs))
			// Zero-copy sendfile(2) for large static-file responses: the
			// H1 adapter's WriteFileResponse routes file bodies ≥ threshold
			// through this hook. Disabled in async mode (the dispatch
			// goroutine would race the worker on cs.sendfile / cs.writeBuf).
			cs.h1State.SetSendFileFn(l.makeSendFileFn(cs))
		}
		cs.h1State.OnDetach = func() {
			// In async mode cs.detachMu was already allocated at
			// acquireConnState time. Reuse it so the dispatch
			// goroutine (which closes over cs.detachMu) and the
			// WebSocket middleware's guarded writeFn share one
			// mutex — otherwise installing a fresh mu here leaves
			// the dispatch goroutine holding a lock nobody else
			// sees.
			// What a panic out of this hook must put back (see the deferred rollback
			// below): the hook runs on a conn that goes on after the panic unless it
			// is on the promoted async path.
			h1 := cs.h1State
			prevWrite, prevMu, prevPending := cs.writeFn, cs.detachMu, cs.asyncDetachPending
			var prevRaw func([]byte)
			var prevPause, prevResume func()
			if h1 != nil {
				prevRaw, prevPause, prevResume = h1.RawWriteFn, h1.PauseRecv, h1.ResumeRecv
			}
			counted, finished := false, false
			var mu *sync.Mutex
			if cs.detachMu != nil {
				mu = cs.detachMu
			} else {
				mu = &sync.Mutex{}
				cs.detachMu = mu
			}
			// Sync mode runs OnDetach on the event-loop thread, so
			// the worker-owned bookkeeping is safe inline. Async
			// mode defers to drainDetachQueue via asyncDetachPending
			// — l.detachedCount races with adaptiveTimeout's read on
			// the event-loop thread, and the eventfd allocation +
			// EPOLL_CTL_ADD touches the loop's epoll set.
			if !l.async {
				l.detachedCount++
				cs.detachCounted = true
				counted = true
			} else {
				cs.asyncDetachPending = true
			}
			orig := cs.writeFn
			guarded := func(data []byte) {
				mu.Lock()
				if cs.detachClosed {
					mu.Unlock()
					return
				}
				orig(data)
				// Inline egress fast path (WS/SSE): issue the send on THIS
				// dispatch goroutine instead of funnelling every detached-conn
				// write through the single event-loop thread. detachMu (held)
				// is the SAME lock the loop-thread dirty-flush takes around
				// flushWrites, and closeConn takes it before tearing the fd
				// down (loop.go:2371) — so the write here can neither race the
				// loop's flush nor touch a closed fd. writeBuf is one ordered
				// buffer flushed from writePos, so dispatch-side and loop-side
				// flushWrites can never reorder a conn's bytes. Reconcile
				// pendingBytes exactly as the dirty-flush does (loop.go:568/587).
				// This parallelises the write(2) across all cores like the std
				// engine, lifting the single-loop-thread broadcast ceiling. On
				// full drain we return WITHOUT enqueuing — the loop never touches
				// the conn. On partial/EAGAIN/error we fall through to the
				// existing enqueue path so the loop finishes the remainder
				// (a detached WS conn's goes to EPOLLOUT, celeris#885 item 42) or tears it down.
				if cs.writeRefused {
					// The write hook refused bytes: the backlog before them was
					// over maxPendingBytesDetached, a peer that stopped reading.
					// Nothing read the flag for a detached conn, so the bytes were
					// dropped and the conn left open, its stream cut in the middle
					// (celeris#761). Close it, through the detach queue as a flush
					// error does; the stream's next Write reports it
					// (detachedStreamErr, celeris#884 item 45, #885 item 30).
					cs.asyncClosed.Store(true)
				} else if err := l.flushWrites(cs, false); err != nil {
					// Surface the specific I/O error (EPIPE/ECONNRESET/…) to the
					// detached middleware before teardown, matching the loop-thread
					// dirty-flush (loop.go:558) and handleWritable. Without this the
					// handler would see a generic io.EOF/ErrWriteClosed from the
					// asyncClosed→OnDetachClose path instead of the real errno.
					if cs.h1State != nil && cs.h1State.OnError != nil {
						cs.h1State.OnError(err)
					}
					cs.asyncClosed.Store(true) // teardown via drainDetachQueue
				} else if !csWritePending(cs) {
					cs.pendingBytes = 0
					mu.Unlock()
					return
				} else {
					cs.pendingBytes = csPendingBytes(cs)
				}
				mu.Unlock()
				// Signal the event loop to flush the remainder / tear down. Do
				// NOT call markDirty from this goroutine — dirtyHead is
				// event-loop-local.
				l.detachQMu.Lock()
				l.detachQueue = append(l.detachQueue, cs)
				// Edge-triggered wakeup: only the enqueue that takes the detach
				// queue empty->non-empty writes the wakeup eventfd. This Swap and
				// the drain's detachQPending.Store(0) both run under detachQMu, so
				// a racing enqueue is either captured by the drain's swap or
				// observes pending==0 and re-arms — never both missed (see
				// drainDetachQueue). Coalesces the per-message wakeup-syscall storm
				// under a hot broadcast fan-out. The detachMu-guarded writeBuf
				// mutation is untouched, so the WS-write-vs-flushWrites ordering
				// invariant (celeris#284) is preserved.
				wasEmpty := l.detachQPending.Swap(1) == 0
				l.detachQMu.Unlock()
				if wasEmpty {
					l.wakeFD.Signal()
				}
			}
			// Async mode (HTTP1): the dispatch goroutine took detachMu
			// around ProcessH1 so writeBuf access serialises with the
			// event loop's flushWrites. Once `guarded` is installed (which
			// re-acquires detachMu on every call), keeping the lock held would deadlock the very next write
			// — including the middleware-emitted 101 / SSE headers
			// that immediately follow Detach. Release the lock here
			// and have the dispatch goroutine observe
			// asyncDetachUnlocked to skip its symmetric Unlock when
			// ProcessH1 returns. See celeris#273.
			//
			// Gate on cs.asyncPromoted: detachMu is Locked around
			// ProcessH1 ONLY by runAsyncHandler (the dispatch goroutine),
			// which runs exclusively for PROMOTED conns. With per-handler
			// async (celeris#300/#302) an async-mode conn (l.async==true,
			// forced on whenever any route opts into .Async()) runs its
			// FIRST request INLINE on the event-loop thread when the conn
			// is not yet promoted (tryInline at drainRead). A SYNC route
			// — e.g. the WebSocket /ws upgrade or the SSE /events stream,
			// which are not themselves .Async() — does not bail with
			// ErrAsyncDispatch, so its handler runs inline and calls
			// Context.Detach() → OnDetach while detachMu was NEVER Locked
			// (the inline path locks it only at the post-handler flush,
			// loop.go:896). Unlocking here under the old `l.async`-only
			// guard then faulted with "sync: unlock of unlocked mutex",
			// fatally crashing the process on the first /ws or /events
			// request. The asyncPromoted gate restricts the Unlock to the
			// dispatch-goroutine path that actually holds the lock; the
			// inline flush owns its own Lock/Unlock around flushWrites, so
			// the guarded writeFn (which re-locks) still serialises with
			// the event loop. See celeris#309.
			// Do NOT release detachMu here — defer it to AFTER Detached.Store(
			// true) below. A concurrent closeConn (peer RST mid-upgrade)
			// acquires detachMu before CloseH1 and re-reads Detached under it;
			// holding the lock across the Store guarantees closeConn observes
			// the ownership handoff and skips CloseH1, instead of recycling the
			// Context/stream this WS/SSE middleware goroutine is still using —
			// the loop.go:2465-vs-websocket.go:243 use-after-recycle that
			// nil-derefs WSRawWriteFn under a peer RST mid-upgrade.
			unlockDetachMu := l.async && cs.asyncPromoted && cs.detachMu != nil && !cs.asyncDetachUnlocked
			// completed: this closure ran to its end. Nothing is owed when this
			// goroutine holds no lock here.
			completed := !unlockDetachMu
			// flagged: asyncDetachUnlocked has been published by this closure.
			flagged := false
			if !unlockDetachMu {
				// A conn that is not on the promoted async path goes on after a panic
				// out of this hook: Context.Detach has not marked the Context detached,
				// the router answers 500, and the next request is served on the same
				// conn. Put back everything installed above and below, so that it is
				// exactly as if Detach had not run: a guarded writeFn left behind
				// locks detachMu again on the first write of the H2C branch and of
				// serveAsync, which hold it across theirs (the loop thread, or the
				// dispatch goroutine, deadlocks on itself), a count left behind is
				// made again by the next Detach, and closeConn undoes one. The
				// promoted path closes the conn instead (the deferred hook below).
				// Assignments only, under no lock: this goroutine owns the conn's
				// state here, as it did when the hook installed it (#889 item 34).
				defer func() {
					if finished {
						return
					}
					cs.writeFn = prevWrite
					if h1 != nil {
						h1.UpdateWriteFn(prevWrite)
						h1.SetWriteErrQuery(cs)
						h1.RawWriteFn, h1.PauseRecv, h1.ResumeRecv = prevRaw, prevPause, prevResume
					}
					cs.detachMu = prevMu
					cs.asyncDetachPending = prevPending
					if counted {
						l.detachedCount--
						cs.detachCounted = false
					}
				}()
			}
			if unlockDetachMu {
				// Registered before guarded is installed (celeris#844): from the moment
				// cs.writeFn is guarded, a write through the response adapter locks
				// detachMu, which this goroutine still holds, so a panic anywhere after
				// the install that the handler recovers (the router does) and answers
				// would block it on itself, and shutdown behind it. The lock has to be
				// gone on every way out of here, and so has the belief that it is held:
				// asyncDetachUnlocked tells abortAsyncHandler, serveAsync and the loop
				// that this goroutine no longer holds it. A panic before the flag is
				// published sets it first, so that an abort that sees the panic
				// propagate does not release a second time (a fatal "unlock of unlocked
				// mutex", celeris#309); one between the flag and the release used to
				// leave the lock held with every reader of the flag believing
				// otherwise, and the next closeConn parked the loop on it. The release
				// follows the publish of Detached, which it also makes if a panic came
				// first, so that a closeConn that wins the lock finds Detached set and
				// leaves the stream to the conn's owner. A panic ends the conn: nothing
				// that follows can finish the detach (the handler may have recovered the
				// panic, as the router does, and returned normally), so it is closed
				// through the detach queue.
				defer func() {
					if !flagged {
						cs.asyncInMu.Lock()
						cs.asyncDetachUnlocked = true
						cs.asyncInMu.Unlock()
					}
					cs.h1State.Detached.Store(true)
					cs.detachMu.Unlock()
					if !completed {
						cs.asyncClosed.Store(true)
						l.enqueueDetach(cs)
					}
				}()
			}
			cs.writeFn = guarded
			// Also update the response adapter so StreamWriter writes
			// go through the guarded path (not the stale pre-Detach writeFn).
			cs.h1State.UpdateWriteFn(guarded)
			cs.h1State.SetWriteErrQuery(&detachedStreamErr{cs: cs, mu: mu})
			if unlockDetachMu {
				if f := detachPreFlagHook.Load(); f != nil {
					(*f)() // test seam, celeris#844 item 2 (before the flag)
				}
			}
			// Expose raw write for WebSocket (bypasses chunked encoding).
			cs.h1State.RawWriteFn = guarded
			// Install pause/resume callbacks for WebSocket backpressure.
			// These set a desired state and wake the loop; the actual
			// EPOLL_CTL_MOD is applied in drainDetachQueue (event-loop thread).
			cs.h1State.PauseRecv = func() {
				if cs.recvPauseDesired.Swap(true) {
					return // already requested
				}
				l.detachQMu.Lock()
				l.detachQueue = append(l.detachQueue, cs)
				// Coalesce the wakeup on the detach queue's empty->non-empty
				// edge — see the write closure above and drainDetachQueue.
				wasEmpty := l.detachQPending.Swap(1) == 0
				l.detachQMu.Unlock()
				if wasEmpty {
					l.wakeFD.Signal()
				}
			}
			cs.h1State.ResumeRecv = func() {
				if !cs.recvPauseDesired.Swap(false) {
					return // already not paused
				}
				l.detachQMu.Lock()
				l.detachQueue = append(l.detachQueue, cs)
				// Coalesce the wakeup on the detach queue's empty->non-empty
				// edge — see the write closure above and drainDetachQueue.
				wasEmpty := l.detachQPending.Swap(1) == 0
				l.detachQMu.Unlock()
				if wasEmpty {
					l.wakeFD.Signal()
				}
			}
			if !unlockDetachMu {
				if f := detachPostInstallHook.Load(); f != nil {
					(*f)() // test seam, #889 item 34 (a conn that is not promoted)
				}
			}
			// Ensure eventfd is available for wakeup. Sync mode is
			// safe to create it inline (event-loop thread).
			// Async mode defers to drainDetachQueue (worker thread)
			// via asyncDetachPending — see the field's comment.
			if !l.async && l.wakeFD.FD() < 0 {
				efd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
				if err == nil {
					// celeris#655: Set refuses once shutdown closed the handle,
					// and then this descriptor is ours to close.
					if l.wakeFD.Set(efd) {
						_ = unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_ADD, efd, &unix.EpollEvent{
							Events: unix.EPOLLIN | unix.EPOLLET,
							Fd:     int32(efd),
						})
					} else {
						_ = unix.Close(efd)
					}
				}
			}
			// (The release of detachMu and the gate on asyncPromoted are set up before
			// guarded is installed, above.)
			if unlockDetachMu {
				// Under asyncInMu: dispatchBusy reads it there, and from here
				// on this goroutine never holds detachMu across a handler
				// again, so the loop must go back to waiting out the (bounded)
				// holders of this conn's lock (celeris#669). detachMu is held
				// here, and detachMu → asyncInMu is the order hijackConn uses.
				cs.asyncInMu.Lock()
				cs.asyncDetachUnlocked = true
				cs.asyncInMu.Unlock()
				flagged = true
				// From here the release is the deferred one registered before guarded
				// was installed (celeris#844).
				if f := detachWindowHook.Load(); f != nil {
					(*f)() // test seam, celeris#844 item 2
				}
			}
			// Async mode: enqueue cs so drainDetachQueue picks up the
			// deferred bookkeeping (asyncDetachPending). The first
			// guarded() write will also enqueue+signal, but the
			// explicit signal here covers the rare case where the
			// middleware returns without writing.
			if l.async {
				l.detachQMu.Lock()
				l.detachQueue = append(l.detachQueue, cs)
				// Coalesce the wakeup on the detach queue's empty->non-empty
				// edge — see the write closure above and drainDetachQueue.
				wasEmpty := l.detachQPending.Swap(1) == 0
				l.detachQMu.Unlock()
				if wasEmpty {
					l.wakeFD.Signal()
				}
			}
			// Publish barrier: Store(true) LAST so a worker that observes
			// Detached.Load()==true is guaranteed (atomic happens-before) to
			// also see every detach side effect set above — detachMu install,
			// guarded writeFn/RawWriteFn, pause/resume callbacks, async
			// bookkeeping. The worker reads Detached on the hot path
			// (writeCap, drainRead, WS delivery); publishing it first would
			// let the worker act on a half-installed detach. See the data-race
			// fix making Detached atomic.
			cs.h1State.Detached.Store(true)
			completed = true
			finished = true
			// detachMu is released as this closure returns (the defer above),
			// now that Detached is published, so the guarded writeFn the
			// middleware calls next (the 101 / SSE headers) can re-acquire it.
			// A closeConn blocked on detachMu now proceeds and reads
			// Detached==true, so it skips CloseH1 and leaves teardown to the
			// middleware goroutine.
		}
		cs.h1State.HijackFn = func() (net.Conn, error) {
			return l.hijackConn(cs.fd)
		}
	case engine.H2C:
		cs.h2State = conn.NewH2State(l.handler, l.h2cfg, cs.writeFn, l.wakeFD)
		cs.h2State.SetRemoteAddr(cs.remoteAddr)
		l.h2Conns = append(l.h2Conns, cs.fd)
	}
}

// switchToH2 promotes an H1 connection to H2 mid-stream (RFC 7540 §3.2).
// Called immediately after ProcessH1 returns ErrUpgradeH2C. The H1 state
// is released; a new H2 state is built with the upgrade info already
// applied (server preface written, client settings absorbed, stream 1
// injected). Any residual bytes from the recv buffer (which may include
// the H2 client preface and initial SETTINGS frame) are fed through
// ProcessH2 synchronously so no data is dropped at the protocol boundary.
func (l *Loop) switchToH2(cs *connState, writeFn func([]byte)) error {
	if err := l.switchToH2Local(cs, writeFn); err != nil {
		return err
	}
	l.h2Conns = append(l.h2Conns, cs.fd)
	return nil
}

// switchToH2Local does every part of switchToH2 except the l.h2Conns
// append, which must happen on the worker goroutine (the slice is
// worker-local and iterated without locks on the hot path). The async
// dispatch goroutine uses this under cs.detachMu and then asks the
// worker to finish via asyncH2Promoted + detachQueue.
func (l *Loop) switchToH2Local(cs *connState, writeFn func([]byte)) error {
	info := cs.h1State.UpgradeInfo
	h2State, err := conn.NewH2StateFromUpgrade(l.handler, l.h2cfg, writeFn, l.wakeFD, info)
	if err != nil {
		cs.h1State.UpgradeInfo = nil
		conn.ReleaseUpgradeInfo(info)
		return err
	}
	cs.h1State.UpgradeInfo = nil
	conn.CloseH1(cs.h1State)
	cs.h1State = nil
	cs.h2State = h2State
	cs.h2State.SetRemoteAddr(cs.remoteAddr)
	cs.protocol = engine.H2C

	var processErr error
	if len(info.Remaining) > 0 {
		processErr = conn.ProcessH2(cs.ctx, info.Remaining, cs.h2State, l.handler, writeFn, l.h2cfg)
	}
	conn.ReleaseUpgradeInfo(info)
	return processErr
}

func (l *Loop) makeWriteFn(cs *connState) func([]byte) {
	return func(data []byte) {
		// Back-pressure (an HTTP/2 or detached conn; see writeCap): refuse
		// the write only when the backlog before it is over the cap, and
		// say so, so the conn is closed rather than left waiting for bytes
		// that will not come (celeris#761).
		if cs.pendingBytes > cs.writeCap() {
			cs.writeRefused = true
			return
		}
		// A sendfile is staged, and the flush sends writeBuf before it:
		// bytes written after it (the next pipelined response) would go
		// out ahead (celeris#802).
		if cs.sendfile != nil && !unstage(cs) {
			cs.writeRefused = true
			return
		}
		cs.writeBuf = append(cs.writeBuf, data...)
		cs.pendingBytes += len(data)
		// Don't markDirty here — drainRead's inline flush handles the
		// happy path. Only markDirty if the inline flush partially
		// completes, avoiding linked-list overhead per request.
	}
}

// makeWriteBodyFn returns the zero-copy body writer of the H1 response
// adapter, for bodies of 8 KiB or more: the body goes to the kernel straight
// from the handler's memory, one writev(2) of [writeBuf, body], skipping the
// respBuf → writeBuf memcpy. The write is made here, in the call, and what
// the kernel does not take is copied into writeBuf before it returns: the
// body belongs to the handler, which may reuse it as soon as its write
// returns. c.JSON puts its buffer back in a pool at once, where the next
// handler to encode takes it, and a buffered response's body lives on a
// pooled Context; a body staged for a flush after the handler went out as
// the next request's bytes, or another connection's (celeris#817). The
// syscall is the one the flush after the handler would have made. Runs on
// the loop thread: the hook is installed only in sync mode.
func (l *Loop) makeWriteBodyFn(cs *connState) func([]byte) {
	return func(body []byte) {
		// A staged sendfile goes out before this body (celeris#802).
		if cs.sendfile != nil && !unstage(cs) {
			cs.writeRefused = true
			return
		}
		cs.bodyBuf = body
		queued := 0
		if cs.writeSince != 0 {
			queued = csPendingBytes(cs) // before this flush (celeris#885)
		}
		err := l.flushWrites(cs, true)
		unstage(cs) // what the kernel did not take; no sendfile is staged
		cs.pendingBytes = csPendingBytes(cs)
		if cs.pendingBytes == 0 {
			// The writev took the queue whole: a write clock the response
			// before it left running ends here, or the next response's park
			// would inherit its stamp (celeris#885 items 35 and 45).
			cs.endWrite()
		} else {
			// It took some of a queue that was parked, and the inline flush
			// that follows (drainRead) counts from what is left.
			cs.noteWriteProgress(queued, cs.pendingBytes)
		}
		if err != nil {
			// The response is lost with the conn: close it.
			cs.writeRefused = true
		}
	}
}

// makeSendFileFn returns the zero-copy sendfile(2) hook installed on the
// H1 response adapter via SetSendFileFn. It is invoked synchronously from
// the handler (inside ProcessH1 on the event-loop thread) when
// WriteFileResponse serves a large file body. It stages the transfer in
// cs.sendfile; the post-handler inline flush in drainRead (and, on
// backpressure, the EPOLLOUT resume in handleWritable) drives it to
// completion — exactly the same machinery as a buffered write, so partial
// sends, ordering with prior pipelined bytes, and timeout tracking all
// work without a separate code path.
//
// The engine dups the caller's descriptor so the file's lifetime is
// independent of the handler's `defer f.Close()` (the handler returns
// long before the kernel finishes a backpressured transfer). The dup is
// closed by flushSendfile on completion or by closeConn / releaseConnState
// on teardown.
//
// There is one sendfile slot. A file response pipelined behind another one
// in a single recv (cs.sendfile already set) first moves the staged one into
// writeBuf as a buffered copy (unstage), so it stays ahead, and then takes
// the slot; handing the new one the buffered copy instead sent it before the
// staged file (celeris#802). A rare dup failure falls back to a buffered
// copy of this response.
func (l *Loop) makeSendFileFn(cs *connState) func(header []byte, file *os.File, offset, length int64) error {
	return func(header []byte, file *os.File, offset, length int64) error {
		if cs.sendfile != nil && !unstage(cs) {
			cs.writeRefused = true // the staged response is cut: close
			return errUnstageSendfile
		}
		dupfd, err := unix.Dup(int(file.Fd()))
		if err != nil {
			return bufferedFileFallback(cs, header, file, offset, length)
		}
		df := os.NewFile(uintptr(dupfd), file.Name())
		st, err := newSendfileState(df, offset, length, header)
		if err != nil {
			_ = df.Close()
			return err
		}
		cs.sendfile = st
		cs.pendingBytes = csPendingBytes(cs)
		return nil
	}
}

// bufferedFileFallback copies a file response (header block + the file
// slice [offset, offset+length)) into cs.writeBuf for the normal write
// path. Used when the zero-copy sendfile slot is unavailable (a prior
// sendfile still draining on a pipelined conn, or a dup failure). The
// caller still owns and closes the *os.File. Correctness over zero-copy:
// the response is delivered byte-exact, just without the syscall savings.
func bufferedFileFallback(cs *connState, header []byte, file *os.File, offset, length int64) error {
	// This response is appended to writeBuf, which goes out before a
	// staged sendfile (celeris#802).
	if cs.sendfile != nil && !unstage(cs) {
		cs.writeRefused = true // the staged response is cut: close
		return errUnstageSendfile
	}
	if length <= 0 {
		fi, err := file.Stat()
		if err != nil {
			return err
		}
		length = fi.Size() - offset
		if length < 0 {
			length = 0
		}
	}
	cs.writeBuf = append(cs.writeBuf, header...)
	start := len(cs.writeBuf)
	cs.writeBuf = append(cs.writeBuf, make([]byte, length)...)
	if err := readFullAt(file, cs.writeBuf[start:start+int(length)], offset); err != nil {
		// Roll back the reserved body region; keep the header (a partial
		// header-only response would corrupt framing, so surface the error
		// and let the caller close the conn).
		cs.writeBuf = cs.writeBuf[:start-len(header)]
		return err
	}
	cs.pendingBytes = csPendingBytes(cs)
	return nil
}

// readFullAt reads len(buf) bytes from file starting at offset, looping
// over short reads. Uses ReadAt so it does not disturb the file's seek
// offset (the caller may reuse the descriptor).
func readFullAt(file *os.File, buf []byte, offset int64) error {
	total := 0
	for total < len(buf) {
		n, err := file.ReadAt(buf[total:], offset+int64(total))
		total += n
		if err != nil {
			if err == io.EOF && total == len(buf) {
				return nil
			}
			return err
		}
	}
	return nil
}

// runAsyncHandler is the dispatch goroutine for an HTTP1 conn when
// Config.AsyncHandlers is enabled: serveAsync, plus the teardown for a
// serveAsync that does not return (celeris#791).
//
// A goroutine the engine spawns must not let a panic crash the process
// (#240). celeris.Server's router recovers a handler panic itself, on this
// path as on the inline one, and answers 500. What reaches here is a panic
// that escapes the router (a stream.Handler used directly, or the engine's
// own request handling) and a runtime.Goexit (t.FailNow in a test handler,
// for one), which no recover stops. Either can unwind serveAsync from inside
// ProcessH1, where this goroutine holds cs.detachMu, or from the park loop,
// where it holds cs.asyncInMu; abortAsyncHandler releases what it still holds
// and gives the conn the teardown a handler error gets.
func (l *Loop) runAsyncHandler(cs *connState) {
	defer l.asyncWG.Done()
	// holds: the locks this goroutine holds (see dispatchHolds). returned:
	// serveAsync returned, so neither a panic nor a Goexit is unwinding
	// through here.
	var holds dispatchHolds
	var returned bool
	defer func() {
		if !returned {
			l.abortAsyncHandler(cs, recover(), &holds)
		}
	}()
	l.serveAsync(cs, &holds)
	returned = true
}

// dispatchHolds is what a dispatch goroutine holds at a given instant, for
// abortAsyncHandler: the teardown cannot otherwise tell, and a lock it took
// that the goroutine already holds deadlocks the goroutine on itself, while a
// lock it skipped that the goroutine still holds wedges whoever takes it next
// (celeris#791, celeris#844).
//
// Both fields are owned by the dispatch goroutine's stack (runAsyncHandler);
// nothing else reads them.
type dispatchHolds struct {
	// detachMu: the goroutine holds cs.detachMu (serveAsync sets it after each
	// Lock and clears it before each Unlock). It stays set across a Detach
	// inside ProcessH1, which releases the lock on this goroutine's behalf and
	// records that in cs.asyncDetachUnlocked instead.
	detachMu bool
	// asyncInMu: the goroutine holds cs.asyncInMu in the park loop at the top
	// of serveAsync, where it calls into code that can panic with the lock
	// held (askAtPark: the engine's own code, so a panic there is an engine bug or a
	// runtime fault). Set after each Lock there and cleared before each
	// Unlock. It stays set across asyncCond.Wait, which returns with the lock
	// held again, and nothing can unwind inside Wait.
	asyncInMu bool
}

// abortAsyncHandler tears cs down after its dispatch goroutine's handler
// panicked (r is the recovered value) or called runtime.Goexit (r is nil, and
// the goroutine ends when this returns). Deferred by runAsyncHandler, on the
// dispatch goroutine.
//
// It first releases cs.asyncInMu if the panic unwound out of the park loop
// holding it (holds.asyncInMu): the teardown below takes that lock to end the
// dispatch, and taking it again would deadlock this goroutine on itself, which
// then never exits (Loop.shutdown waits for it) while the loop blocks on the
// same lock the next time it feeds this conn (celeris#844).
//
// It then formats the panic value, takes cs.fd and the stack, and releases
// cs.detachMu, if this goroutine still holds it: holds.detachMu, and not
// already released by a Detach inside ProcessH1 (asyncDetachUnlocked,
// celeris#273; Unlocking that again would be a fatal "unlock of unlocked
// mutex", cf. celeris#309; OnDetach releases the lock on every way out once
// guarded is installed, a panic included, and sets the flag itself if it
// unwinds before it has, celeris#844). The log record is written after the
// release, with those copies: the application's slog
// handler may be slow, while the loop's shutdown and the guarded writes of a
// detached conn take the lock unconditionally; but once the lock is free a
// closeConn that meets cs (closeOnReadEnd on a peer FIN, the timeout reap,
// shutdown) takes it and closes the conn, fd included, and CloseH1 releases
// the stream. A panic value that shares the stream's memory (s.Headers, a view
// of the body buffer) formatted after that reads zeroed memory or another
// conn's bytes, and cs.fd may name a reissued descriptor (celeris#844; on the path where a
// panic unwound OnDetach, that release and the close it queues happened before
// this function ran, so the fd attribute may already be stale there: the stream
// is safe, Detached makes closeConn leave it alone, and only the number in the
// log can be). The
// formatting runs the value's String or Error method under the lock, so a
// slow one holds up shutdown, which waits for the lock, and not the loop: a
// closeConn that finds the lock held while the goroutine runs a handler
// leaves the close to the goroutine (closeOwed).
//
// The rest is the handler-error teardown in serveAsync: asyncClosed,
// endDispatch under asyncInMu, then the hand-back through the detach queue,
// whose asyncClosed branch closes the conn on the loop thread (once what it
// has staged has gone out, celeris#761). Like that path it never holds two of
// detachMu, asyncInMu and detachQMu at once.
//
// cs stays valid while the log runs: closeConn never pools a conn that has a
// detachMu (!detached), and the drain skips the hand-back of a conn already
// detachClosed. endDispatch clears asyncRun after the release, so a loop that
// finds the lock held while the goroutine reads as gone (dispatchBusy false)
// waits only for a bounded holder.
func (l *Loop) abortAsyncHandler(cs *connState, r any, holds *dispatchHolds) {
	// The conn ends here whatever happens next. Say so before the lock the park
	// loop holds is let go: until asyncClosed is set, a transplant sweep that finds
	// the goroutine parked and idle may take the conn for a hand-off, and a refused
	// hand-off is reclaimed as a new, healthy conn that outlives its panic.
	cs.asyncClosed.Store(true)
	if holds.asyncInMu {
		holds.asyncInMu = false
		cs.asyncInMu.Unlock()
	}
	fd := cs.fd
	var stack, msg string
	if l.logger != nil {
		stack = string(debug.Stack())
		if r != nil {
			msg = panicText(r)
		}
	}
	if holds.detachMu && !cs.asyncDetachUnlocked {
		holds.detachMu = false
		cs.detachMu.Unlock()
	}
	if l.logger != nil {
		if r != nil {
			l.logger.Error("async handler panicked",
				"panic", msg,
				"stack", stack,
				"fd", fd,
			)
		} else {
			l.logger.Error("async handler exited without returning (runtime.Goexit)",
				"stack", stack,
				"fd", fd,
			)
		}
	}
	cs.asyncClosed.Store(true)
	cs.asyncInMu.Lock()
	cs.asyncInBuf = cs.asyncInBuf[:0]
	cs.endDispatch() // enqueued below: that is the hand-back
	cs.asyncInMu.Unlock()
	// Signal worker via detachQueue + eventfd (never close the
	// fd from this goroutine — races with drainRead on the
	// worker's stale l.conns slot).
	l.enqueueDetach(cs)
}

// panicText formats a panic value for the abort's log record. fmt recovers
// a panic out of the value's Error or String method, but only one level deep: a
// method that panics with a value whose own method panics is re-raised out of
// Sprint. The abort runs that under detachMu inside a deferred function nothing
// recovers, so the second level is recovered here, and the type name stands in
// for the text (celeris#844).
func panicText(r any) (msg string) {
	defer func() {
		if recover() != nil {
			msg = fmt.Sprintf("%T (its Error or String method panicked)", r)
		}
	}()
	return fmt.Sprint(r)
}

// serveAsync is the dispatch goroutine's loop (see runAsyncHandler). It
// takes the currently-buffered bytes via a double-buffer swap, runs
// ProcessH1 under detachMu (so write paths serialize with worker-initiated
// flushes), flushes the response, and parks on asyncCond.Wait when the
// buffer is empty — keeping the goroutine alive across keep-alive requests.
// The worker calls asyncCond.Signal after each append, and
// asyncCond.Broadcast in closeConn so the parked goroutine exits cleanly.
//
// This preserves HTTP/1.1 pipelining order guarantees: a pipelined
// burst arrives in one worker read, ProcessH1's offset loop drains
// every request from that slice in order, and responses appear in the
// same order on cs.writeBuf before the flush. The "one goroutine at a
// time per conn" invariant is enforced by cs.asyncRun.
//
// holds tracks its Lock and Unlock of cs.detachMu, and of cs.asyncInMu in the
// park loop, for abortAsyncHandler (see dispatchHolds).
func (l *Loop) serveAsync(cs *connState, holds *dispatchHolds) {
	for {
		cs.asyncInMu.Lock()
		holds.asyncInMu = true
		if cs.relinkOwed {
			// The loop gave this conn up while our handler held detachMu
			// (celeris#669: the dirty pass or the EPOLLOUT resume); hand it
			// back now that the handler's writes are flushed as far as they
			// go, so the loop re-examines it. asyncInMu → detachQMu: nothing
			// takes asyncInMu under detachQMu.
			cs.relinkOwed = false
			l.enqueueDetach(cs)
		}
		cs.asyncParked = true
		for len(cs.asyncInBuf) == 0 && !cs.asyncClosed.Load() && !cs.asyncQuiesce.Load() {
			// Park until the worker appends more bytes or the conn is
			// being torn down. Stays alive across keep-alive requests
			// so we don't pay the ~1.5µs goroutine spawn cost per
			// request — profile showed this accounted for ~3% of
			// epoll+async CPU on high-rps redis workloads.
			//
			// celeris#657 P8: while a drain is set, ask the loop to
			// examine this conn now that it is parked with nothing
			// buffered — the only state in which the hand-off accepts
			// it, and for a slow handler a window a periodic sweep
			// almost never catches. One ask per park (the CAS), only
			// for a conn the hand-off can actually accept (askAtPark's
			// pre-check), and the goroutine only asks: tryTransplant,
			// on the loop thread, decides.
			l.askAtPark(cs)
			cs.asyncCond.Wait()
		}
		cs.asyncParked = false
		if cs.asyncClosed.Load() {
			// The one exit that does not enqueue cs: a close requested while
			// this goroutine ran may have been left to it (celeris#669), and
			// then this is where it is handed back.
			owed := cs.endDispatch()
			holds.asyncInMu = false
			cs.asyncInMu.Unlock()
			if owed {
				l.enqueueDetach(cs)
			}
			return
		}
		// #383: transplant quiesce requested and input drained — exit cleanly
		// (do NOT close the conn) and signal the loop to finish the hand-off to
		// io_uring. tryTransplant only sets asyncQuiesce on a parked, flushed
		// conn whose fd it has already detached, so asyncInBuf is empty here.
		if cs.asyncQuiesce.Load() && len(cs.asyncInBuf) == 0 {
			cs.endDispatch() // enqueued below: that is the hand-back
			holds.asyncInMu = false
			cs.asyncInMu.Unlock()
			l.enqueueDetach(cs)
			return
		}
		// Double-buffer swap: hand asyncInBuf to the goroutine, reuse
		// the emptied asyncOutBuf as the worker's new asyncInBuf. No
		// allocation on steady state — append(out[:0], ...) grows into
		// the retained backing array.
		cs.asyncInBuf, cs.asyncOutBuf = cs.asyncOutBuf[:0], cs.asyncInBuf
		data := cs.asyncOutBuf
		holds.asyncInMu = false
		cs.asyncInMu.Unlock()

		// Post-detach iterations (WS / SSE): ProcessH1's only job is to
		// deliver `data` to state.WSDataDelivery (writes flow through
		// the guarded writeFn, which acquires cs.detachMu on its own).
		// Re-Locking here on every torture-frame delivery and then
		// skipping the symmetric Unlock in the asyncDetachUnlocked
		// branch leaks the mutex, which deadlocks the WS handler's
		// writeCloseProtocol → writeCloseFrame → guarded → Lock chain
		// (celeris#284: per-worker WS handler hang manifesting as
		// "first WS upgrade succeeds, every subsequent /ws upgrade on
		// the same worker fails with EOF on status line"). The fix
		// mirrors iouring's runAsyncHandler.
		acquiredDetachMu := false
		if !cs.asyncDetachUnlocked {
			cs.detachMu.Lock()
			acquiredDetachMu = true
			holds.detachMu = true
		}
		// Re-check asyncClosed under detachMu when we acquired it;
		// closeConn sets asyncClosed BEFORE tearing down cs.h1State.
		// Mirrors the iouring fix.
		if cs.asyncClosed.Load() {
			if acquiredDetachMu {
				holds.detachMu = false
				cs.detachMu.Unlock()
			}
			// Nor does this one; see the loop-top exit.
			cs.asyncInMu.Lock()
			owed := cs.endDispatch()
			cs.asyncInMu.Unlock()
			if owed {
				l.enqueueDetach(cs)
			}
			return
		}
		processErr := conn.ProcessH1(cs.ctx, data, cs.h1State, l.handler, cs.writeFn)
		// H1→H2 upgrade on the async path. ProcessH1 has written the
		// 101 Switching Protocols response into cs.writeBuf and stashed
		// the upgrade info on cs.h1State. Promote cs-local state here
		// (safe — holding detachMu), flush the 101 + server preface,
		// then hand the conn back to the worker to (a) finish the
		// promotion via l.h2Conns append and (b) resume recv in H2
		// mode. The goroutine exits because from now on this conn uses
		// the inline H2 path.
		if errors.Is(processErr, conn.ErrUpgradeH2C) {
			promoteErr := l.switchToH2Local(cs, cs.writeFn)
			if promoteErr == nil && cs.writePos < len(cs.writeBuf) {
				// flushWrites may partially complete; residual bytes
				// stay in cs.writeBuf and the worker retries via
				// markDirty once drainDetachQueue picks us up.
				// Dispatch-goroutine call site: not the loop thread, so
				// byte accounting must hit the shared atomic, not the
				// per-loop batch (false → atomic path).
				if err := l.flushWrites(cs, false); err != nil {
					promoteErr = err
				}
			}
			holds.detachMu = false
			cs.detachMu.Unlock()
			if promoteErr != nil {
				cs.asyncClosed.Store(true)
				cs.asyncInMu.Lock()
				cs.asyncInBuf = cs.asyncInBuf[:0]
				cs.endDispatch() // enqueued below: that is the hand-back
				cs.asyncInMu.Unlock()
			} else {
				cs.asyncInMu.Lock()
				cs.asyncInBuf = cs.asyncInBuf[:0]
				cs.endDispatch() // enqueued below: that is the hand-back
				cs.asyncInMu.Unlock()
				cs.asyncH2Promoted.Store(true)
			}
			l.enqueueDetach(cs)
			return
		}
		// celeris#273: a user handler may have called c.Detach() inside
		// ProcessH1 (websocket or sse middleware). OnDetach released
		// detachMu so subsequent guarded writeFn calls don't deadlock.
		// The dispatch goroutine no longer owns the lock — skip the
		// inline-flush path (the bytes were already enqueued via
		// guarded → detachQueue/eventfd, the event loop will flush
		// them) and skip the symmetric Unlock below.
		if cs.asyncDetachUnlocked {
			// ErrHijacked is a valid post-Detach return: the H1 parser
			// considers a hijacked conn "done with the request". Treat
			// it like nil here — the middleware now owns the conn.
			if processErr != nil && !errors.Is(processErr, conn.ErrHijacked) {
				cs.asyncClosed.Store(true)
				cs.asyncInMu.Lock()
				cs.asyncInBuf = cs.asyncInBuf[:0]
				cs.endDispatch() // enqueued below: that is the hand-back
				cs.asyncInMu.Unlock()
				l.enqueueDetach(cs)
				return
			}
			// Post-Detach: loop back to wait for more recv bytes (WS
			// frames delivered via WSDataDelivery, or SSE conn-close
			// detection). The handler runs in its own goroutine
			// spawned by the middleware; this dispatch goroutine just
			// shuttles RX bytes into ProcessH1 → WSDataDelivery.
			continue
		}
		var flushErr error
		// Flush any pending response bytes BEFORE the close-path check
		// below. Gating this on `processErr == nil` is wrong for
		// errConnectionClose (HTTP/1.1 Connection: close) — the handler
		// staged a valid response in cs.writeBuf, ProcessH1 returned an
		// internal "close after this response" sentinel, and falling
		// through to the asyncClosed→closeConn→fastClose path without
		// flushing means the response bytes never leave the kernel send
		// buffer before unix.Close fires. Wireshark confirms: under
		// epoll + AsyncHandlers + Connection: close, the server sends
		// only FIN — no HTTP response data — and curl reports
		// code=000 with a sub-ms time. The sync (non-async) dispatcher
		// already does this unconditional flush at line ~783.
		//
		// Only ErrHijacked stays gated: a hijacked conn's bytes belong
		// to the middleware's goroutine now and re-flushing here would
		// race the middleware's own write path.
		if !errors.Is(processErr, conn.ErrHijacked) &&
			(cs.writePos < len(cs.writeBuf) || len(cs.bodyBuf) > 0) {
			// Dispatch-goroutine call site (false → shared-atomic byte path).
			flushErr = l.flushWrites(cs, false)
		}
		// Resync pendingBytes with actual buffer state. makeWriteFn uses
		// pendingBytes to enforce writeCap backpressure; without this
		// reset, it grows by len(response) on every request and after
		// ~writeCap bytes of cumulative responses every subsequent
		// makeWriteFn call silently drops its payload — the client then
		// blocks forever on the missing response. Mirror the inline-flush
		// path (drainRead) and dirty-loop accounting.
		if flushErr == nil {
			cs.resyncPending()
		}
		partial := flushErr == nil && (cs.writePos < len(cs.writeBuf) || len(cs.bodyBuf) > 0)
		// A refused write (celeris#761) ends the conn as a request error
		// does: this goroutine exits and the loop closes the conn once what
		// was staged has gone out (drainDetachQueue, closeWhenFlushed).
		refused := cs.writeRefused
		holds.detachMu = false
		cs.detachMu.Unlock()

		if partial {
			l.enqueueDetach(cs)
		}

		if processErr != nil || flushErr != nil || refused {
			// Signal the worker to tear down the conn from its own
			// goroutine. Never close cs.fd directly here — the worker
			// goroutine still has l.conns[fd] pointing at cs, and a
			// concurrent drainRead on the stale slot would race with
			// fd reuse after close. Instead set asyncClosed, enqueue
			// on detachQueue, and write eventfd; the worker's
			// drainDetachQueue pass will observe asyncClosed and run
			// closeConn thread-safely.
			cs.asyncClosed.Store(true)
			cs.asyncInMu.Lock()
			cs.asyncInBuf = cs.asyncInBuf[:0]
			cs.endDispatch() // enqueued below: that is the hand-back
			cs.asyncInMu.Unlock()
			l.enqueueDetach(cs)
			return
		}
	}
}

// endDispatch marks cs's dispatch goroutine as gone and reports whether a
// close was left to it (closeOwed, celeris#669). The goroutine calls it on
// every exit path, holding cs.asyncInMu. A path that enqueues cs on its way
// out ignores the result: that enqueue is the hand-back, and
// drainDetachQueue's asyncClosed branch runs the close. The two paths that
// exit WITHOUT enqueuing must enqueue when it reports true, or the close is
// lost and the conn stays open, owned by no goroutine.
func (cs *connState) endDispatch() (closeOwed bool) {
	cs.asyncRun = false
	// A relink owed at exit needs no hand-back of its own: the conn is being
	// closed (asyncClosed), hijacked, handed to the other engine, or its exit
	// enqueues cs anyway (the H2 upgrade), and each of those settles it.
	cs.relinkOwed = false
	closeOwed = cs.closeOwed
	cs.closeOwed = false
	return closeOwed
}

// enqueueDetach hands cs to the loop thread's drainDetachQueue and wakes the
// loop. Dispatch goroutine.
func (l *Loop) enqueueDetach(cs *connState) {
	l.detachQMu.Lock()
	l.detachQueue = append(l.detachQueue, cs)
	l.detachQPending.Store(1)
	l.detachQMu.Unlock()
	l.wakeFD.Signal()
}

func (l *Loop) drainDetachQueue() {
	if l.detachQPending.Load() == 0 {
		return
	}
	l.detachQMu.Lock()
	l.detachQSpare, l.detachQueue = l.detachQueue, l.detachQSpare[:0]
	l.detachQPending.Store(0)
	l.detachQMu.Unlock()
	for _, cs := range l.detachQSpare {
		// #383 transplant: tryTransplant detached the fd from epoll (at a
		// flushed, clean boundary) and asked the dispatch goroutine to
		// quiesce. Finish the hand-off to io_uring once it has exited.
		// Checked before EVERY other branch — including the already-closed
		// guard below — so a quiescing conn is migrated, not dropped. The old
		// ordering put detachClosed first, which would strand such a conn: no
		// hand-off, no close, no hook, no counter, and the live gauge already
		// decremented (celeris#624). finishTransplantHandoff re-checks
		// detachClosed and counts the coincidence.
		//
		// Every entry naming cs reaches this branch, not only the
		// goroutine's exit: one it made before it parked — the remainder of
		// its own partial flush, or a relink hand-back (celeris#669) — may be
		// drained after the quiesce was asked. Finishing on such an entry
		// released cs while the goroutine was still waking to exit. So an
		// entry drained while the goroutine lives does nothing (its exit,
		// which always enqueues, finishes the hand-off), and the finish
		// never pools cs (see finishTransplantHandoff): the loop cannot tell
		// whether a later entry still names it. asyncRun is read under
		// asyncInMu, which the exit clears it under.
		if cs.transplantPending {
			cs.asyncInMu.Lock()
			alive := cs.asyncRun
			cs.asyncInMu.Unlock()
			if alive {
				continue
			}
			l.finishTransplantHandoff(cs)
			continue
		}
		if cs.transplanted {
			continue // handed over already; this entry came after the finish
		}
		if cs.detachClosed {
			continue
		}
		// Hijacked conn (#3.1): hijackConn already detached the fd from
		// epoll and the conn table and handed it to the caller as a
		// net.Conn (the original fd is closed; the caller owns a dup). It
		// enqueued cs at once for what it may not do from the dispatch
		// goroutine (celeris#668): take the conn out of the live set,
		// connCount and the dirty list, and withdraw any transplant ask.
		// That is done here, once; the goroutine's own exit and any entry
		// made before the hijack find hijackSettled and do nothing. The
		// connState is never pooled (see hijackConn), so none of those
		// entries can name reissued memory. Skip closeConn — there is no fd
		// to close and l.conns[fd] is already nil.
		if cs.hijacked.Load() {
			if !cs.hijackSettled {
				cs.hijackSettled = true
				l.removeLiveConn(cs)
				l.connCount--
				// Unlink from the dirty list: hijackConn does not, and a
				// conn left on it would have the dirty pass write its
				// queued bytes to a descriptor number that is no longer
				// its own (celeris#654).
				l.removeDirty(cs)
				l.dropAsk(cs) // celeris#657 P8: no ask may name it once it is not ours
			}
			continue
		}
		// Dispatch goroutine signaled close via asyncClosed. Only the
		// worker can safely touch l.conns / dirty list, so we handle
		// the teardown here. Once the goroutine has exited, the response
		// it left queued goes out before the close (celeris#761): closing
		// at once cut off, after Connection: close, any response larger
		// than the socket buffers. While it still runs, closeConn leaves
		// the close to it (celeris#669), as before.
		if cs.asyncClosed.Load() {
			cs.asyncInMu.Lock()
			running := cs.asyncRun
			cs.asyncInMu.Unlock()
			if running {
				l.closeConn(cs.fd)
			} else {
				l.closeWhenFlushed(cs)
			}
			continue
		}
		// Dispatch goroutine promoted the conn to H2 via switchToH2Local.
		// Finish the worker-owned bits of the swap: register the fd on
		// the H2 write-queue poll list. The conn stays alive and
		// subsequent recvs dispatch via the inline H2 path (cs.protocol
		// is already H2C).
		if cs.asyncH2Promoted.Load() {
			cs.asyncH2Promoted.Store(false)
			l.h2Conns = append(l.h2Conns, cs.fd)
			cs.relinkPending = false // celeris#669: back on the dirty list
			// A peer half-close owed while the handler ran (celeris#885): made
			// now, after the h2Conns bookkeeping so a close finds the entry it
			// removes. This branch continues before the tail below.
			if l.handBackHalfClose(cs) {
				l.markDirty(cs)
			}
			continue
		}
		// Async-mode Detach finalisation: OnDetach ran on the
		// dispatch goroutine and cannot touch event-loop-owned state
		// (l.detachedCount or the wakeup eventfd). It set asyncDetachPending
		// and enqueued cs so we land here on the event-loop thread
		// and run those mutations safely. Idempotent — the flag is
		// cleared before the bookkeeping so a second drainDetachQueue
		// pass (from the same enqueue burst) is a no-op.
		if cs.asyncDetachPending {
			cs.asyncDetachPending = false
			// Once per conn, whatever enqueued it again (#889 item 34).
			if !cs.detachCounted {
				l.detachedCount++
				cs.detachCounted = true
			}
			if l.wakeFD.FD() < 0 {
				efd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
				if err == nil {
					// celeris#655: Set refuses once shutdown closed the handle,
					// and then this descriptor is ours to close.
					if l.wakeFD.Set(efd) {
						_ = unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_ADD, efd, &unix.EpollEvent{
							Events: unix.EPOLLIN | unix.EPOLLET,
							Fd:     int32(efd),
						})
					} else {
						_ = unix.Close(efd)
					}
				}
			}
		}
		// Apply any pending pause/resume request from the WS middleware.
		// The MOD drops EPOLLIN from the interest, or restores it (see
		// setRecvPaused). The kernel applies TCP-level backpressure
		// (zero-window) when its recv buffer fills.
		if desired := cs.recvPauseDesired.Load(); desired != cs.recvPaused {
			l.setRecvPaused(cs, desired)
		}
		// A relink hand-back (celeris#669) lands here too: the conn is back
		// on the dirty list, whose next pass sees it as it now is. A peer
		// half-close owed while the handler ran (celeris#885) is made first: a
		// close it makes releases cs, which must not be put on the list.
		cs.relinkPending = false
		if l.handBackHalfClose(cs) {
			l.markDirty(cs)
		}
	}
	// Drop the strong refs before reusing the array (see the io_uring
	// worker's drainDetachQueue and drainPendingRelease for the same
	// guard): [:0] alone leaves every *connState reachable in the
	// backing array until some later drain overwrites its slot.
	clear(l.detachQSpare)
	l.detachQSpare = l.detachQSpare[:0]
}

// relink gives up, for now, a conn whose flush the loop could not do because
// its dispatch goroutine holds detachMu across a handler (celeris#669). The
// caller has already set cs.relinkOwed through dispatchBusy; the goroutine
// hands cs back through the detach queue at its next park — after it has
// flushed what its handler wrote — and drainDetachQueue puts it on the dirty
// list, whose next pass sees the conn as it now is. Until then the conn is on
// neither the dirty list nor EPOLLOUT.
//
// Waiting for the hand-back, rather than for "a remainder", is what keeps the
// actions tied to a completed flush — a deferred peer close (peerClosed), the
// EPOLLOUT disarm — from being lost: a holder whose own flush completes has no
// remainder to hand back, and peerClosed may be set after this call.
// relinkPending keeps tryTransplant off the conn until an entry has put it
// back on the dirty list, i.e. until the loop has seen it as the handler left
// it. (A queue entry naming a moved conn is drainDetachQueue's to make
// harmless, whichever entry clears relinkPending.) Loop thread.
func (l *Loop) relink(cs *connState) {
	l.removeDirty(cs)
	if cs.epollOut {
		l.disarmEpollOut(cs)
	}
	cs.relinkPending = true
}

// flushDirty is the event loop's dirty-list pass, run once per iteration
// after drainDetachQueue: flush every connection with bytes still queued,
// close the ones whose flush failed, and hand a still-partial conn, detached
// or not, to EPOLLOUT. Loop thread only.
func (l *Loop) flushDirty() {
	for cs := l.dirtyHead; cs != nil; {
		next := cs.dirtyNext
		mu := cs.detachMu
		if mu != nil && !mu.TryLock() {
			if dispatchBusy(cs, &cs.relinkOwed) {
				// The conn's dispatch goroutine holds detachMu across a
				// user handler (celeris#669): waiting here parked the loop,
				// and every connection on it, until the handler returned,
				// and retrying it each pass would hold epoll_wait at 0 ms
				// — a spin — for as long. So give the conn up until the
				// goroutine hands it back (relink): it does so at its next
				// park, after it has flushed what its handler wrote.
				l.relink(cs)
				cs = next
				continue
			}
			mu.Lock()
		}
		if mu != nil && cs.hijacked.Load() {
			// An async Hijack took the conn (celeris#668) and closed its
			// descriptor, whose number may already be someone else's:
			// writing the bytes still queued here would put them there.
			// drainDetachQueue settles the rest. (Only an async conn —
			// one with a detachMu — is ever hijacked off-thread, so a
			// sync conn skips the load.)
			mu.Unlock()
			l.removeDirty(cs)
			cs = next
			continue
		}
		queued := 0
		if cs.writeSince != 0 {
			queued = csPendingBytes(cs) // before this flush, under the same lock (celeris#885)
		}
		err := l.flushWrites(cs, true)
		if err != nil {
			// Surface I/O failure to detached middleware before closing.
			if cs.h1State != nil && cs.h1State.OnError != nil {
				cs.h1State.OnError(err)
			}
			if mu := cs.detachMu; mu != nil {
				mu.Unlock()
			}
			l.removeDirty(cs)
			l.closeConn(cs.fd)
		} else if !csWritePending(cs) {
			cs.pendingBytes = 0
			cs.endWrite()
			if mu := cs.detachMu; mu != nil {
				mu.Unlock()
			}
			l.removeDirty(cs)
			// Deferred peer-close (EPOLLRDHUP arrived mid-response): now flushed.
			if cs.peerClosed {
				l.closeConn(cs.fd)
			}
		} else {
			// Partial write: kernel send buffer full. Sync pendingBytes,
			// then hand the conn off to EPOLLOUT (armEpollOut removes it from
			// the dirty list) so the loop stops busy-retrying it. A truly-
			// detached WS/SSE conn too (celeris#885 item 42): left on the
			// list it held adaptiveTimeoutMs at 0, an epoll_wait(0) and a
			// write that returned EAGAIN on every pass for as long as its peer
			// did not read. Who flushes it from here: handleWritable on the
			// EPOLLOUT edge, and the middleware goroutine's guarded writeFn at
			// its next write, both under detachMu; a write that raises a new
			// remainder enqueues the conn again, and this pass hands it back.
			// `next` was captured above, so the removeDirty inside
			// armEpollOut is safe mid-iteration.
			cs.pendingBytes = csPendingBytes(cs)
			if cs.peerClosed {
				cs.noteClosingProgress(cs.pendingBytes)
			}
			cs.noteWriteProgress(queued, cs.pendingBytes)
			if mu := cs.detachMu; mu != nil {
				mu.Unlock()
			}
			l.armEpollOut(cs)
		}
		cs = next
	}
}

func (l *Loop) markDirty(cs *connState) {
	if cs.dirty {
		return
	}
	cs.dirty = true
	cs.dirtyNext = l.dirtyHead
	cs.dirtyPrev = nil
	if l.dirtyHead != nil {
		l.dirtyHead.dirtyPrev = cs
	}
	l.dirtyHead = cs
}

// armEpollOut handles socket write backpressure on an HTTP/H2 conn: a
// flushWrites left bytes pending because the kernel send buffer is full
// (write(2) returned EAGAIN / a short count). Instead of re-flushing into a
// guaranteed EAGAIN on every iteration — which made adaptiveTimeoutMs spin
// at epoll_wait(0) → write(EAGAIN) at 100% CPU under backpressure — we add
// EPOLLOUT to the conn's interest set and DROP it from the
// dirty list. The worker then blocks in epoll_wait until the socket is
// writable again, at which point handleWritable flushes and disarms.
//
// EPOLLET covers the whole event mask, so EPOLLOUT is edge-triggered here,
// like EPOLLIN (driver.go's flushDriverSendLocked registers no EPOLLET, so a
// driver conn's EPOLLOUT is level-triggered). No wakeup is lost: the MOD
// reports EPOLLOUT at once if the socket is already writable, and a partial
// flush (a short write or EAGAIN: flushWrites makes one write(2) and returns
// on a short count, it does not go on to EAGAIN) leaves the send buffer full,
// after which the buffer gaining room is a new edge.
func (l *Loop) armEpollOut(cs *connState) {
	// Backpressure replaces the dirty-list retry; the two must not coexist
	// or adaptiveTimeoutMs would still return 0 and busy-poll.
	l.removeDirty(cs)
	// The response is parked: start its write clock (celeris#885 items 35 and
	// 45), unless it has one already (a conn relinked or adopted with bytes
	// queued keeps the clock of the response it was already taking). The wall
	// clock, not cachedNow, which a loop with no events leaves as old as its
	// last event. Before the epollOut test: the drain sites end the clock
	// (endWrite) without disarming EPOLLOUT, so a conn can be armed with no
	// clock, and its next response's partial flush must start one. A partial
	// flush is the backpressure path, not the common one.
	if cs.writeSince == 0 {
		cs.writeSince = time.Now().UnixNano()
	}
	if cs.epollOut {
		return
	}
	issued, err := l.modEpollOut(cs, cs.epollInterest(true))
	if !issued {
		return // hijacked: drainDetachQueue settles the conn
	}
	if err == nil {
		cs.epollOut = true
	} else {
		// MOD failed (should not happen for a registered fd); fall back to
		// the dirty-list retry so the pending bytes still get flushed.
		l.markDirty(cs)
	}
}

// disarmEpollOut removes the EPOLLOUT interest once a conn's
// pending writes have fully drained, restoring the read-only edge-triggered
// interest so an idle fd doesn't wake the loop on every writable signal.
func (l *Loop) disarmEpollOut(cs *connState) {
	if !cs.epollOut {
		return
	}
	_, _ = l.modEpollOut(cs, cs.epollInterest(false))
	cs.epollOut = false
}

// epollInterest is the interest mask of cs's registration, derived from the
// conn's state, for every EPOLL_CTL_MOD of an open conn (celeris#885 item 42):
// edge-triggered with EPOLLRDHUP always, EPOLLIN unless the middleware paused
// recv (recvPaused), EPOLLOUT as out says. A MOD replaces the whole mask, so
// one written for a single purpose undid the other state: arming EPOLLOUT on a
// paused conn re-enabled the EPOLLIN it had given up, and a pause dropped the
// EPOLLOUT an armed conn waits for, which a detached conn can now be
// (flushDirty hands it to EPOLLOUT). Not for closeWhenFlushed, whose mask
// has no EPOLLIN by design: that conn is closing and read no more. Loop
// thread (recvPaused is its state).
func (cs *connState) epollInterest(out bool) uint32 {
	ev := uint32(unix.EPOLLET | unix.EPOLLRDHUP)
	if !cs.recvPaused {
		ev |= unix.EPOLLIN
	}
	if out {
		ev |= unix.EPOLLOUT
	}
	return ev
}

// setRecvPaused applies the middleware's pause (or resume) of recv to cs's
// registration: recvPaused first, then the MOD with the interest it implies,
// keeping EPOLLOUT if the conn is armed (epollOut) and EPOLLRDHUP either way.
// The MOD is the unguarded one, as it was: a pause is requested by the
// conn's middleware, whose conn is not hijacked meanwhile. Loop thread.
func (l *Loop) setRecvPaused(cs *connState, paused bool) {
	cs.recvPaused = paused
	ev := cs.epollEvent(cs.epollInterest(cs.epollOut))
	_ = unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_MOD, cs.fd, &ev)
}

// modEpollOut sets cs's interest in this loop's epoll set to events, by
// cs.fd, and reports whether it issued the MOD, and the MOD's error.
//
// It does not issue it for a conn an async Hijack has taken (celeris#668).
// Every site that arms or disarms EPOLLOUT for an async conn does so without
// detachMu, or after failing to take it, so the conn's handler can hijack it
// meanwhile. The hijack stores hijacked, clears the slot under driverMu, and
// only then closes the descriptor, whose number another file can take at
// once, one a driver goroutine adds to this epoll set among them. Read under
// driverMu, hijacked false means the descriptor is still the conn's until
// the MOD has run. A hijacked conn is out of the epoll set already. A sync
// conn is only ever hijacked inline, on this thread, so it skips the lock.
//
// The cost of the lock on this send-path call: BenchmarkEPOLLOUTArmDisarm.
func (l *Loop) modEpollOut(cs *connState, events uint32) (issued bool, err error) {
	ev := cs.epollEvent(events)
	if cs.detachMu == nil {
		return true, unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_MOD, cs.fd, &ev)
	}
	l.driverMu.RLock()
	if cs.hijacked.Load() {
		l.driverMu.RUnlock()
		return false, nil
	}
	err = unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_MOD, cs.fd, &ev)
	l.driverMu.RUnlock()
	return true, err
}

// handleWritable resumes a backpressured conn on an EPOLLOUT event: flush
// the pending bytes and, if fully drained, disarm EPOLLOUT and restore the
// read-only interest. A still-partial flush leaves EPOLLOUT armed so the
// next writable edge resumes. Returns false if the conn was closed.
func (l *Loop) handleWritable(cs *connState) {
	mu := cs.detachMu
	if mu != nil && !mu.TryLock() {
		if dispatchBusy(cs, &cs.relinkOwed) {
			// A pipelined request started a handler before the socket
			// drained (celeris#669). Do not wait for it, and do not leave
			// the conn to EPOLLOUT's next edge either: this event spent the
			// last one, and if the handler's own flush drains the socket no
			// other may come, stranding the disarm and a deferred peer
			// close. Give the conn up until its goroutine hands it back
			// (see relink).
			l.relink(cs)
			return
		}
		mu.Lock()
	}
	if mu != nil && cs.hijacked.Load() {
		// The run loop read cs from its slot, and then the conn's handler
		// hijacked it (celeris#668) and returned: the descriptor is closed
		// and its number may already be another file's. Writing the queued
		// bytes, or disarming EPOLLOUT by the number, would reach that file,
		// as in flushDirty. drainDetachQueue settles the rest.
		mu.Unlock()
		return
	}
	queued := 0
	if cs.writeSince != 0 {
		queued = csPendingBytes(cs) // before this flush, under the same lock (celeris#885)
	}
	err := l.flushWrites(cs, true)
	drained := err == nil && !csWritePending(cs)
	if err == nil {
		if drained {
			cs.pendingBytes = 0
			cs.endWrite()
		} else {
			cs.pendingBytes = csPendingBytes(cs)
			if cs.peerClosed {
				cs.noteClosingProgress(cs.pendingBytes)
			}
			cs.noteWriteProgress(queued, cs.pendingBytes)
			if cs.writeSince == 0 {
				// EPOLLOUT is armed and bytes are queued, but the conn has no
				// clock: a drain site ended it and the response that followed
				// was queued without a pass through armEpollOut. Start it here
				// rather than rely on a later flush reaching armEpollOut
				// (celeris#885 items 35 and 45). The backpressure path.
				cs.writeSince = time.Now().UnixNano()
			}
		}
	}
	if err != nil && cs.h1State != nil && cs.h1State.OnError != nil {
		cs.h1State.OnError(err)
	}
	if mu != nil {
		mu.Unlock()
	}
	if err != nil {
		l.closeConn(cs.fd)
		return
	}
	if drained {
		l.disarmEpollOut(cs)
		// Deferred peer-close (EPOLLRDHUP arrived mid-response): the response is
		// now fully flushed, so close the conn rather than strand it.
		if cs.peerClosed {
			l.closeConn(cs.fd)
		}
	}
}

func (l *Loop) removeH2Conn(fd int) {
	for i, f := range l.h2Conns {
		if f == fd {
			l.h2Conns[i] = l.h2Conns[len(l.h2Conns)-1]
			l.h2Conns = l.h2Conns[:len(l.h2Conns)-1]
			return
		}
	}
}

func (l *Loop) removeDirty(cs *connState) {
	if !cs.dirty {
		return
	}
	cs.dirty = false
	if cs.dirtyPrev != nil {
		cs.dirtyPrev.dirtyNext = cs.dirtyNext
	} else {
		l.dirtyHead = cs.dirtyNext
	}
	if cs.dirtyNext != nil {
		cs.dirtyNext.dirtyPrev = cs.dirtyPrev
	}
	cs.dirtyNext = nil
	cs.dirtyPrev = nil
}

// addLiveConn records a newly-accepted fd in the dense liveConns slice and
// stamps cs.liveIdx so removeLiveConn is O(1). Worker-thread-only. (#318)
func (l *Loop) addLiveConn(cs *connState) {
	cs.liveIdx = len(l.liveConns)
	l.liveConns = append(l.liveConns, cs)
	// A dormant sweep judged the set this loop held; it holds a different
	// one now (celeris#657 P7).
	l.wakeSweep()
}

// removeLiveConn removes cs's fd from liveConns in O(1) via swap-with-last,
// using cs.liveIdx rather than the O(N) scan the iouring engine still does.
// The element swapped into cs's old slot has its own liveIdx fixed up so a
// later removeLiveConn for that conn stays correct. Worker-thread-only and
// idempotent (liveIdx<0 means not present). (#318)
func (l *Loop) removeLiveConn(cs *connState) {
	if cs == nil {
		return
	}
	i := cs.liveIdx
	if i < 0 || i >= len(l.liveConns) || l.liveConns[i] != cs {
		return
	}
	last := len(l.liveConns) - 1
	if i != last {
		// The fix-up follows the entry itself, not l.conns[fd]: the
		// entry may be a hijacked conn whose slot is already clear, or
		// whose descriptor number now belongs to another conn
		// (celeris#668).
		moved := l.liveConns[last]
		l.liveConns[i] = moved
		moved.liveIdx = i
	}
	l.liveConns[last] = nil // connStates are pooled: no stale reference past len
	l.liveConns = l.liveConns[:last]
	cs.liveIdx = -1
	// The residue this loop last published counted this conn; it no longer
	// holds it (celeris#657 R2).
	l.sweepNoteDeparture()
}

func (l *Loop) adaptiveTimeoutMs(base int) int {
	// When dirty list has unflushed data, poll immediately.
	if l.dirtyHead != nil {
		return 0
	}
	// When H2 connections exist and no eventfd is available, fall back to
	// 1ms polling for write queue draining. With eventfd, handler goroutines
	// signal directly and epoll_wait returns event-driven — no polling needed.
	if len(l.h2Conns) > 0 && l.wakeFD.FD() < 0 {
		return 1
	}
	// Cap the timeout when detached conns exist so checkTimeouts can fire
	// the WS-supplied IdleDeadline before its expiry.
	maxMs := 500
	if l.detachedCount > 0 {
		maxMs = 50
	}
	// Slowloris defence: cap epoll_wait when ReadHeaderTimeout is enabled
	// so checkTimeouts fires the HeaderDeadline reliably under low traffic.
	// 25ms × 0x1F gate = 800ms worst-case sweep latency.
	if l.cfg.ReadHeaderTimeout > 0 && maxMs > 25 {
		maxMs = 25
	}
	switch {
	case l.consecutiveEmpty == 0:
		return base
	case l.consecutiveEmpty <= 10:
		return base
	case l.consecutiveEmpty <= 100:
		d := base * 2
		if d > 100 {
			d = 100
		}
		if d > maxMs {
			d = maxMs
		}
		return d
	default:
		d := base * 4
		if d > maxMs {
			d = maxMs
		}
		return d
	}
}

// detachDrainGrace caps how long the idle-deadline reap waits for a
// truly-detached conn's already-queued terminal bytes to reach the wire
// before it sends FIN anyway. Generous enough to cover several WAN
// round-trips for a peer that is merely behind, short enough that a peer
// which has stopped reading for good releases its fd promptly. A peer that is
// actually gone never waits this long: the next flush (handleWritable's, or
// the dirty pass's for a conn on the list) surfaces EPIPE/ECONNRESET and its
// error branch closes the conn immediately.
const detachDrainGrace = time.Second

// closingDrainFloor bounds the drain of a conn whose close is deferred behind
// its response (markClosing) when WriteTimeout is disabled: how long the peer
// may take nothing of it (no acknowledged byte, no flush that sent some).
// io_uring's closingDrainTimeoutNanos is the same 5 s.
const closingDrainFloor = 5 * time.Second

// h1DeadlineSnapshot is what checkTimeouts knows about a conn's H1 state: a
// copy taken under cs.detachMu, so the pointer is never dereferenced after
// switchToH2Local may have released it.
type h1DeadlineSnapshot struct {
	detached     bool  // truly detached (WS/SSE): the middleware owns the I/O lifecycle
	idleDL       int64 // H1State.IdleDeadlineNs; read, and meaningful, only when detached
	hdrDL        int64 // H1State.HeaderDeadlineNs, 0 when none; read only when not detached
	writePending bool  // csWritePending(cs); read only when detached
}

// snapshotH1Deadlines copies out of cs.h1State, under cs.detachMu, the fields
// checkTimeouts decides on (celeris#865, the epoll twin of celeris#548/#722;
// iouring has snapshotH1Deadlines in worker.go, whose shape this follows).
//
// The dispatch goroutine's switchToH2Local sets cs.h1State = nil under
// detachMu. Reading the pointer and what it points to outside the lock is a
// data race, which the race detector reports against that write (probatorium
// run 37969571447), and the memory model lets a reload find nil. The gc
// compiler merges the adjacent loads of the old code, so what was observed is
// the race, not a crash. The lock is taken with TryLock and never with Lock: the
// dispatch goroutine holds detachMu across ProcessH1, so a blocking Lock would
// park the loop thread, and every conn on it, behind one slow handler
// (celeris#669, celeris#593). The lock is released before the caller acts:
// closeConn takes the same non-reentrant mutex.
//
// ok=false means the lock is held by a bounded holder (a detached conn's
// guarded writeFn, for one write) and NOTHING was read: the sweep skips the
// conn this time. The skip is not bounded: a holder that takes the lock again
// and again (a hot broadcaster) can win the TryLock race on several sweeps in
// a row, and the sweep runs often on a busy loop (see checkTimeouts' cadence
// note), so a run of skips is unlikely but not excluded, and a conn whose
// deadline has passed is reaped on a later sweep, by chance, not on the next.
// Each holder's critical section is bounded (one write), so the conn is
// reaped once any sweep finds the lock free.
//
// When the lock is held because the dispatch goroutine is inside a handler
// (dispatchBusy), nothing is read either, and ok=true with the zero snapshot:
// such a conn is not detached (Detach sets asyncDetachUnlocked before it
// publishes Detached, and dispatchBusy excludes it) and has no header deadline
// (ProcessH1 clears it once the request is parsed, before the handler runs).
// So the zero snapshot IS the answer, and checkTimeouts applies the idle,
// read and write timeouts to a conn whose handler overruns them, as it did
// before (nothing refreshes lastActivity while a handler runs; celeris#669).
func snapshotH1Deadlines(cs *connState) (snap h1DeadlineSnapshot, ok bool) {
	mu := cs.detachMu
	if mu != nil && !mu.TryLock() {
		return snap, dispatchBusy(cs, nil)
	}
	// Only plain and atomic loads from here to the Unlock: nothing can panic
	// or block while the lock is held. A detached conn needs its idle deadline
	// and write state, any other its header deadline, which are the loads the
	// unlocked code made.
	if h1 := cs.h1State; h1 != nil {
		if snap.detached = h1.Detached.Load(); snap.detached {
			snap.idleDL = h1.IdleDeadlineNs.Load()
			snap.writePending = csWritePending(cs)
		} else {
			snap.hdrDL = h1.HeaderDeadlineNs.Load()
		}
	}
	if mu != nil {
		mu.Unlock()
	}
	return snap, true
}

// sweepFloor is the least time between two checkTimeouts sweeps that the run
// loop's iteration gate asks for, and the period of the timerfd that
// guarantees one on an idle loop. A busy loop's iterations are well under a
// millisecond apart, and a gate that counts them swept thousands of times a
// second (celeris#885 item 29); this is the bound the timerfd already promises
// a conn's header deadline.
const sweepFloor = 25 * time.Millisecond

// testHookCheckTimeouts, when set, runs at the start of every checkTimeouts
// sweep on the loop's own goroutine, with the loop. The cadence tests count
// sweeps and read the conns through it (celeris#885 item 29). An atomic pointer,
// not a plain variable: a test sets it while loops of an earlier engine of the
// package may still be winding down. A load and a nil test per sweep.
var testHookCheckTimeouts atomic.Pointer[func(*Loop)]

// checkTimeouts scans active connections and closes any that have exceeded
// their configured timeout.
//
// Cadence: the run loop calls it from the gate every 32 epoll_wait iterations
// when ReadHeaderTimeout is set (the default is 10 s) or a conn is detached,
// otherwise every 1024, but only when sweepFloor (25 ms) has passed since the
// last sweep (lastSweep, stamped here, so the timerfd's sweeps count too); the
// 25 ms timerfd (armed only when ReadHeaderTimeout is set) guarantees one on an
// idle loop. A busy loop therefore sweeps about 40 times a second, where the
// gate alone swept as often as its iterations allowed: measured on 2 loops with
// 4096 idle keep-alive conns each, before the floor (celeris PR 969): about 120
// sweeps a second in all with no traffic (the timerfd alone gives 80), and
// 1,800 to 6,200 a second in all with one to sixteen busy clients.
//
// Iterates the dense liveConns slice (#318) rather than the sparse
// 0..maxFD range, so the cost is O(active conns) regardless of FD space.
// Iterating by REVERSE index is load-bearing: closeConn → removeLiveConn
// swaps the closed conn's slot with the last entry, so a forward scan
// would skip the swapped-in element. Walking high→low means any swap only
// touches indices we've already visited.
func (l *Loop) checkTimeouts() {
	now := time.Now().UnixNano()
	l.lastSweep = now
	if h := testHookCheckTimeouts.Load(); h != nil {
		(*h)(l)
	}
	for i := len(l.liveConns) - 1; i >= 0; i-- {
		cs := l.liveConns[i]
		if cs.hijacked.Load() {
			// Handed to the application by an async Hijack, and waiting
			// for its dispatch goroutine to hand it back (celeris#668):
			// not this loop's to time out, and its descriptor number may
			// already be another conn's.
			continue
		}
		fd := cs.fd
		// ReadHeaderTimeout for a conn whose protocol is not detected yet: it
		// has no H1 state, so the header deadline below has nothing to read,
		// and detectDeadline (accept + ReadHeaderTimeout) stands in for it.
		// Such a conn is never detached, so this needs neither detachMu nor
		// the snapshot.
		// detected is tested first: it shares the cache line of fd and dirty,
		// which this loop reads anyway, and the sweep of a loop full of
		// established conns then never loads detectDeadline (RULE 74,
		// BenchmarkCheckTimeouts865).
		if !cs.detected {
			if dl := cs.detectDeadline; dl > 0 && now > dl {
				l.closeConn(fd)
				continue
			}
		}
		// Detached connections (e.g. WebSocket): honor an explicit deadline
		// supplied by the middleware via SetWSIdleDeadline. Skip the
		// engine-config-driven idle/read/write timeouts since the middleware
		// owns the I/O lifecycle. Async-mode conns set detachMu up front
		// without a true detach — fall through to the normal scan for those.
		// h1State is read once, under detachMu (celeris#865); see
		// snapshotH1Deadlines. A conn whose lock is held by a bounded holder
		// is skipped for this sweep and met again on a later one (not
		// necessarily the next: the skip is probabilistic).
		snap, ok := snapshotH1Deadlines(cs)
		if !ok {
			continue
		}
		if snap.detached {
			dl := snap.idleDL
			if dl <= 0 || now <= dl {
				// Unset, or pushed back out by the middleware (a WS frame
				// extends it): forget any grace stamp so a later expiry
				// starts with a full drain window.
				cs.drainDeadline = 0
				// WriteTimeout is the one engine timeout a detached stream
				// keeps: "no progress for WriteTimeout" on what is queued for
				// the peer (celeris#888 items 38 and 39). Here, whatever the
				// middleware's idle deadline says: a WebSocket peer that sends
				// pings pushes it out for ever and reads nothing of what it is
				// sent. The expired case below is the middleware ending the
				// stream and has its own bound.
				if snap.writePending {
					if l.detachedStalled(cs, now) {
						l.closeConn(fd)
					}
				} else if cs.closeSince != 0 {
					cs.endDetachedStall()
				}
				continue
			}
			// The reap doubles as "this stream is over, drop the conn":
			// the SSE middleware stores a 1 ns deadline once its last event
			// is queued (celeris#494) and the WS middleware does the same
			// behind the close echo. closeConn's SHUT_WR commits only what
			// the KERNEL already took, so reaping while writeBuf still holds
			// those bytes truncates the stream — and a truly-detached conn is
			// precisely the one that parks its EAGAIN remainder (on EPOLLOUT,
			// celeris#885 item 42; handleWritable flushes it), where nothing
			// else closes on drain: this sweep does, once the remainder has
			// gone. Let that flush finish first, mirroring iouring's
			// deferred close (cs.closing while sends are pending) and the
			// EPOLLRDHUP path's cs.peerClosed defer. Bounded: a peer that
			// stops reading for good must not strand the fd.
			if snap.writePending {
				if cs.drainDeadline == 0 {
					cs.drainDeadline = now + int64(detachDrainGrace)
				}
				if now <= cs.drainDeadline {
					continue
				}
			}
			l.closeConn(fd)
			continue
		}
		// A conn whose close is deferred behind its response (markClosing)
		// has its own clock: the engine reads it no more, so lastActivity,
		// the last read, says nothing about it, and ReadTimeout or
		// IdleTimeout measured from there cut a response the client was
		// still taking (celeris#876). Nor is there a request any more for a
		// header deadline to bound. closeSince starts at the close request
		// and moves forward with each flush that sends (noteClosingProgress)
		// and with each sweep that finds the peer has acknowledged more bytes
		// (noteClosingAcked): a socket raises EPOLLOUT only after about half
		// of its send queue drained, so a slow client is taking the response
		// long before the flush that would show it.
		// Keyed on the clock, not peerClosed: a conn that carries
		// peerClosed without it (a test fixture) takes the scan below.
		if cs.closeSince != 0 {
			bound := l.closingDrainBound()
			// A sample is due every quarter of the bound, and once more when
			// the bound has passed since the last one was taken before it: the
			// rate limit must not stand in for a fresh look at the moment of
			// the reap when sweeps are irregular (a conn first met late in its
			// bound, the next sweep past it). Once a sample is from past the
			// bound, a conn that stays (it was given another bound) waits for
			// the rate limit again, so this costs one getsockopt per reap
			// decision, not one per sweep.
			if time.Duration(now-cs.closeSampled) >= bound/4 ||
				(time.Duration(now-cs.closeSince) > bound && cs.closeSampled <= cs.closeSince+int64(bound)) {
				l.noteClosingAcked(cs, now)
			}
			if time.Duration(now-cs.closeSince) > bound {
				l.closeConn(fd)
			}
			continue
		}
		// ReadHeaderTimeout: slowloris defence. Mirror net/http behavior:
		// plain unix.Close (handled inside closeConn → fastClose branch),
		// no shutdown, no linger, no response body. net/http on header
		// timeout calls c.close() → plain Close; std walker shows 0
		// hangs in probatorium. SHUT_RDWR+LINGER and 408+SHUT_WR+drain
		// approaches both retained ~5-7% walker-hangs (nightly
		// 26429150655 and predecessors), pointing at our extra syscalls
		// racing with the walker's drip Write rather than at FIN/RST
		// signaling. Simplest mirrors std.
		if dl := snap.hdrDL; dl > 0 && now > dl {
			l.closeConn(fd)
			continue
		}
		// A response the kernel has not taken whole has its own clock
		// (writeSince, celeris#885 items 35 and 45): lastActivity is the last
		// read, so for a keep-alive conn parked on EPOLLOUT it measured neither
		// a peer that takes nothing (WriteTimeout was applied to dirty conns
		// only, and EPOLLOUT takes the conn off the list) nor one that takes the
		// response steadily (ReadTimeout and IdleTimeout cut it, the request
		// being longer ago than they are). The common conn has neither the
		// clock nor a place on the dirty list, and pays one load and test.
		last := cs.lastActivity
		if cs.writeSince != 0 || cs.dirty {
			var done bool
			if last, done = l.sweepWriteClock(cs, fd, now, last); done {
				continue
			}
		}
		elapsed := time.Duration(now - last)
		if l.cfg.IdleTimeout > 0 && elapsed > l.cfg.IdleTimeout {
			l.closeConn(fd)
		} else if l.cfg.ReadTimeout > 0 && elapsed > l.cfg.ReadTimeout {
			l.closeConn(fd)
		}
	}
}

// detachedStalled is checkTimeouts' look at a truly-detached conn (WS/SSE)
// that has bytes queued for its peer: whether the peer has taken none of them
// for WriteTimeout (celeris#888 items 38 and 39, celeris#884 item 69). The
// middleware owns the stream's lifecycle, so no ReadTimeout, IdleTimeout or
// absolute WriteTimeout applies to it; but a client that stays connected and
// reads nothing never fails a write on a native engine (the write only queues),
// and nothing else ended its stream short of the 64 MiB cap on what the engine
// holds for it. The close reaches the middleware as any engine close does
// (OnDetachClose, so an SSE stream's context ends and OnDisconnect runs).
//
// The clock is closeSince, the one a conn whose close is deferred behind its
// response has (a detached conn is never in that state: closeWhenFlushed closes
// it at once, and a half-close is the middleware's to see), started at the
// first sweep that finds bytes queued and restarted only by evidence that the
// peer takes them: the bytes it has acknowledged moved (noteClosingAcked, a
// sample every quarter of the bound, the first being the baseline, so a peer
// that took bytes in the first quarter and then none is closed up to a quarter
// of the bound earlier than WriteTimeout after its last byte). Not lastActivity, which is the last read:
// a WebSocket peer that sends and reads nothing, or an SSE client's request,
// says nothing about what it takes. The acknowledged count sees the bytes that
// the stream's own goroutine writes through the guarded hook (it flushes with
// no lock the loop's clocks are under) as well as the loop's flushes, and costs
// nothing per write. The sweep does the sampling after it released detachMu,
// and the clock is the loop thread's alone, as for a closing conn.
//
// A socket that does not report the acknowledged count has no bound (see the
// check below).
//
// WriteTimeout 0 (no timeout, or -1) is no bound: unlike a closing conn, which
// has no handler to wait for and gets closingDrainFloor, a stream is the
// middleware's to end, and a server that turned the write timeout off asked for
// a client to be held for as long as it stays. A conn with nothing queued ends
// the clock (endDetachedStall). Loop thread.
func (l *Loop) detachedStalled(cs *connState, now int64) bool {
	bound := int64(l.cfg.WriteTimeout)
	if bound <= 0 {
		return false
	}
	if cs.closeSince == 0 {
		// A new stall. No sample yet: a conn whose queue is there at one
		// sweep and gone at the next (a hot broadcaster's) would pay a
		// getsockopt per sweep for an episode that ends before it matters.
		// The first sample, the baseline, is due a quarter of the bound from
		// now.
		cs.closeSince = now
		cs.closeSampled = now
		cs.closeAckedKnown = false
		return false
	}
	if now-cs.closeSampled >= bound/4 || (now-cs.closeSince > bound && cs.closeSampled <= cs.closeSince+bound) {
		l.noteClosingAcked(cs, now)
	}
	if cs.closeSampled > cs.closeSince && !cs.closeAckedKnown {
		// A sample was taken since the stall began and the socket gave no
		// acknowledged count (not a TCP socket):
		// the stream's own goroutine flushes with no lock the loop's clocks are
		// under, and nothing else sees the bytes it writes, so a peer that
		// reads cannot be told from one that does not. No bound, as before this
		// one.
		return false
	}
	return now-cs.closeSince > bound
}

// endDetachedStall ends the stall clock of a detached conn that has nothing
// queued (detachedStalled). Loop thread.
func (cs *connState) endDetachedStall() {
	cs.closeSince = 0
	cs.closeAckedKnown = false
}

// sweepWriteClock is checkTimeouts' look at a live conn that holds a response
// the kernel has not taken whole (celeris#885 items 35 and 45): it has the
// write clock (writeSince), or is on the dirty list. WriteTimeout is "no
// progress for WriteTimeout" there: the clock starts at the first partial flush
// and restarts when a flush takes bytes (noteWriteProgress), where std's is an
// absolute deadline for the whole response. It answers the instant IdleTimeout
// and ReadTimeout are measured from, the later of the last read and the clock,
// and whether the conn is done for this sweep (closed, or exempt from the idle
// scan). Kept out of line: the sweep of a conn without a clock does not run it.
// Loop thread.
func (l *Loop) sweepWriteClock(cs *connState, fd int, now, last int64) (from int64, done bool) {
	ws := cs.writeSince
	if ws == 0 {
		// On the list without a clock: not parked on EPOLLOUT, flushed by the
		// pass that follows. WriteTimeout from the last read, as before, and
		// no idle scan.
		if wt := l.cfg.WriteTimeout; wt > 0 && time.Duration(now-last) > wt {
			l.closeConn(fd)
		}
		return last, true
	}
	// Is anything still queued? A drain the loop did not see (the dispatch
	// goroutine's own flush, under its lock) leaves the clock set with an empty
	// queue. A lock held by a running handler cannot say: the answer is "yes".
	pending := true
	if mu := cs.detachMu; mu != nil {
		if mu.TryLock() {
			pending = csWritePending(cs)
			mu.Unlock()
		}
	} else {
		pending = csWritePending(cs)
	}
	if pending {
		if wt := l.cfg.WriteTimeout; wt > 0 && time.Duration(now-ws) > wt {
			l.closeConn(fd)
			return last, true
		}
	} else {
		// End the clock as endWrite does, from its last stamp, which is no
		// later than the drain.
		cs.writeSince = 0
		if ws > last {
			cs.lastActivity = ws
		}
	}
	if ws > last {
		last = ws
	}
	return last, false
}

func (l *Loop) closeConn(fd int) {
	// Capture the slot under driverMu rather than bare. Every other l.conns
	// reader on this path is the worker itself, but hijackConn nils this slot
	// from the DISPATCH goroutine under the write lock (celeris#654), so an
	// unlocked read here races a real concurrent writer — the same reason the
	// ownership re-check below takes the lock. No caller holds driverMu (the
	// run loop, drainRead, checkTimeouts, drainDetachQueue, the dirty pass and
	// attachAdoptedFD's carry replay all call in unlocked), so this cannot
	// deadlock, and a close already costs several syscalls.
	l.driverMu.RLock()
	var cs *connState
	if fd >= 0 && fd < len(l.conns) {
		cs = l.conns[fd]
	}
	l.driverMu.RUnlock()
	if cs == nil {
		return
	}
	detached := cs.detachMu != nil
	if detached {
		// Signal both dispatch goroutine (async handlers) and detached
		// goroutine (WebSocket/SSE) to stop. asyncClosed is checked at
		// the top of runAsyncHandler's loop between ProcessH1 runs.
		cs.asyncClosed.Store(true)
		// Wake the parked dispatch goroutine so it observes asyncClosed
		// and exits cleanly. Safe when the Cond was never armed (the
		// L field is lazily set in acquireConnState when async=true).
		if cs.asyncCond.L != nil {
			cs.asyncInMu.Lock()
			cs.asyncCond.Broadcast()
			cs.asyncInMu.Unlock()
		}
		// Signal the detached goroutine's writeFn to stop writing.
		// The mutex serializes with any in-progress write — if the
		// goroutine is mid-write, we wait until it finishes.
		//
		// But not for a handler (celeris#669, the epoll twin of the closed
		// #593). The dispatch goroutine holds this mutex across ProcessH1,
		// i.e. for as long as the user handler runs, and a blocking Lock
		// here parked the loop thread — every connection on it, no
		// epoll_wait, no accept, no flush — until the handler returned:
		// through the timeout reap, EPOLLRDHUP, EPOLLHUP and every error
		// path that closes. When the lock is held and that goroutine is
		// running, leave the close to it: asyncClosed is set, so it exits at
		// its next check, and its exit hands cs back through the detach
		// queue, whose asyncClosed branch calls here again with the lock
		// free. Until then the conn stays whole — in the table, the live set
		// and epoll, its descriptor open — because the handler is still
		// writing its response into it. When the goroutine is parked or
		// gone, the holder is a guarded writeFn in one write, and waiting
		// for it is bounded; see dispatchBusy.
		if !cs.detachMu.TryLock() {
			if dispatchBusy(cs, &cs.closeOwed) {
				return
			}
			cs.detachMu.Lock()
		}
		// celeris#654: re-validate ownership now that we hold the lock.
		// Under AsyncHandlers the user handler runs inside ProcessH1 with
		// detachMu held, and Context.Hijack → hijackConn detaches the conn
		// right there on the dispatch goroutine: EPOLL_CTL_DEL,
		// l.conns[fd] = nil, the public counters, and close() of the
		// original descriptor (the caller keeps a dup). A closeConn that
		// captured cs BEFORE that ran and then waited on this lock would,
		// without a re-check, redo every one of those steps on a conn the
		// engine no longer owns: the close counted twice, the live gauge
		// negative, an OnDisconnect for a connection handed to the
		// application, and a SHUT_WR + Close on a descriptor NUMBER the
		// kernel's lowest-free-fd rule has already reissued to something
		// else — another loop's accept, an fd the handler opened, a driver
		// conn registered off-thread into this same epfd.
		//
		// Since celeris#669 closeConn no longer waits on a running handler,
		// so this wait-then-hijack interleaving needs a holder that is not
		// the dispatch goroutine; the check stays as the guard for it.
		//
		// One check suffices. The slot is only cleared off-thread by
		// hijackConn, which in async mode runs under this very mutex, and
		// asyncClosed (stored above) keeps the dispatch goroutine from
		// entering another ProcessH1 once we release it — so no hijack can
		// slip in between here and the CloseH1 re-lock further down.
		l.driverMu.RLock()
		owned := fd < len(l.conns) && l.conns[fd] == cs
		l.driverMu.RUnlock()
		if !owned {
			// What this early return does, and deliberately does not do. It
			// is not "the teardown below minus the fd close": each step is
			// either already performed, or now belongs to someone else.
			//
			//   - removeDirty: DONE HERE. It is keyed by cs, so it can only
			//     touch the conn we captured. hijackConn does not unlink and
			//     releaseConnState clears only cs's own links, so a hijacked
			//     conn left on the list would have the loop flush pending
			//     bytes to a reissued descriptor. No-op unless the conn
			//     really was dirty (a partial write from a prior pipelined
			//     response). The two ordinary hijack releases are unlinked at
			//     their own sites — hijackConn and drainDetachQueue.
			//   - removeH2Conn: NOT done, on purpose. It is keyed by FD, not
			//     by cs, and when the slot has been reissued that fd belongs
			//     to another connection: unlinking it would break the new
			//     owner's H2 write-queue polling. A hijacked H2 conn cannot
			//     arise today anyway (switchToH2Local nils h1State, so
			//     ProcessH1 — and therefore Hijack — cannot run on an H2
			//     conn); if that ever changes, the unlink belongs in
			//     hijackConn, the site that knows it is hijacking.
			//   - the public counters, EPOLL_CTL_DEL, the fd close and the
			//     slot nil: all already done by hijackConn.
			//   - removeLiveConn and connCount: not ours to do either. An
			//     off-thread hijack leaves both to drainDetachQueue's
			//     hijacked branch, on this thread, when it drains the
			//     notice hijackConn enqueued (celeris#668).
			//   - CloseH1 and the pool release: not ours to do. Leaving
			//     detachClosed false is what lets drainDetachQueue reach its
			//     cs.hijacked branch (detachClosed is tested first and
			//     would skip it); an off-thread-hijacked connState is
			//     never pooled.
			//   - OnDisconnect: NOT fired. The conn did not disconnect, it
			//     was handed to the application, and the sync hijack path
			//     fires nothing either. Both directions are pinned by
			//     hijack_closeconn_race_linux_test.go.
			l.removeDirty(cs)
			cs.detachMu.Unlock()
			return
		}
		// A drainRead branch that met a running handler left its I/O error
		// here rather than wait for the lock (closeErr, celeris#669). Do
		// what that branch did, now that the lock is held: flush what is
		// queued, then tell a detached middleware.
		if err := cs.closeErr; err != nil {
			cs.closeErr = nil
			_ = l.flushWrites(cs, true)
			if cs.h1State != nil && cs.h1State.OnError != nil {
				cs.h1State.OnError(err)
			}
		}
		cs.detachClosed = true
		// Acquire barrier: only invoke OnDetachClose once the WS upgrade has
		// fully wired the conn (WSReady). Otherwise the read of OnDetachClose —
		// and the ws.Close() it calls — races the upgrade installing it and the
		// rest of the ws state on the async goroutine after Detach released
		// detachMu (peer RST mid-upgrade). Not-yet-wired conns are still torn
		// down via the fd close + read path below.
		if cs.h1State != nil && cs.h1State.WSReady.Load() && cs.h1State.OnDetachClose != nil {
			cs.h1State.OnDetachClose()
			cs.h1State.OnDetachClose = nil
		}
		cs.detachMu.Unlock()
		// Drop callbacks once the engine relinquishes the conn so any late
		// goroutine references resolve to no-ops without crashing. Same acquire
		// barrier as OnDetachClose above: only drop them once the WS upgrade has
		// finished reading them (via WSReadPauser) and published WSReady — before
		// that, the async upgrade goroutine is still reading PauseRecv/ResumeRecv.
		if cs.h1State != nil && cs.h1State.WSReady.Load() {
			cs.h1State.PauseRecv = nil
			cs.h1State.ResumeRecv = nil
		}
		// Only decrement for a conn that was counted (WS/SSE detach whose
		// increment ran). Async-mode HTTP1 conns pre-allocate detachMu without
		// bumping detachedCount, so decrementing would underflow here, and a
		// conn closed before the drain counted it would take another's place.
		if cs.detachCounted {
			cs.detachCounted = false
			if l.detachedCount > 0 {
				l.detachedCount--
			}
		}
	}
	// Close H1 state unless a real WS/SSE detach handed ownership to a
	// middleware goroutine. Async-mode HTTP1 conns have detachMu set
	// but h1State.Detached is false — we still own H1 state there.
	//
	// For detached / async-dispatched conns, hold detachMu while
	// tearing down h1State. runAsyncHandler runs ProcessH1 under the
	// same lock, and CloseH1 writes state.stream / state.bodyBuf /
	// state.bodyNeeded that ProcessH1 reads; without the lock a peer
	// close mid-request corrupts H1State (tracked as celeris#256 on
	// the iouring side). cs.asyncClosed is set earlier; the goroutine
	// checks it on loop re-entry, so acquiring detachMu here only
	// blocks for the current ProcessH1 call.
	trulyDetached := detached && cs.h1State != nil && cs.h1State.Detached.Load()
	l.removeDirty(cs)
	// Release any in-progress sendfile transfer's dup'd descriptor. Sendfile
	// responses only run on non-detached H1 conns, so releaseConnState would
	// also catch it, but close it here too so the dup fd is reclaimed
	// promptly even on the detached branch (which skips releaseConnState).
	if cs.sendfile != nil {
		cs.sendfile.close()
		cs.sendfile = nil
	}
	if !trulyDetached && cs.h1State != nil {
		if detached {
			cs.detachMu.Lock()
			// Re-read Detached UNDER the lock. An in-flight Context.Detach()
			// (OnDetach on the async dispatch goroutine) publishes Detached and
			// only then releases detachMu (see initProtocol), so acquiring the
			// lock the handler held observes the final ownership. If the conn
			// detached while we waited, the WS/SSE middleware goroutine owns
			// teardown — skip CloseH1; otherwise we would recycle a
			// Context/stream it is still using (the use-after-recycle that
			// nil-derefs WSRawWriteFn under a peer RST mid-upgrade).
			if cs.h1State.Detached.Load() {
				trulyDetached = true
			} else {
				conn.CloseH1(cs.h1State)
			}
			cs.detachMu.Unlock()
		} else {
			conn.CloseH1(cs.h1State)
		}
	}
	if cs.h2State != nil {
		conn.CloseH2(cs.h2State)
		l.removeH2Conn(fd)
	}
	_ = unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_DEL, fd, nil)
	// Clear the slot before the descriptor is closed, and under driverMu
	// (celeris#775). The number is free to be reissued the moment the close
	// returns, and a driver's RegisterConn on it reads this slot, under the
	// same lock, from its own goroutine: written bare, that is a data race,
	// and cleared after the close, a window in which the driver is refused
	// ("already an HTTP connection") for a number that no longer is one. The
	// slot is read on this thread only by checks keyed by cs (ask, sweep,
	// the ownership re-check above), none of which runs between here and the
	// close. One uncontended lock per close, on a path of several syscalls.
	l.driverMu.Lock()
	l.conns[fd] = nil
	l.driverMu.Unlock()
	// H1 close: SHUT_WR + Close. The shutdown call forces FIN regardless
	// of the kernel recv-buffer state — important under slowloris where
	// the peer is still writing drips and a plain Close on a non-empty
	// recv buffer would send RST instead (which is not retransmitted by
	// TCP, so a single packet loss strands the walker).
	//
	// No recv drain between SHUT_WR and Close: an empty drain leaves
	// no race, but a non-empty one introduces a multi-µs window in which
	// a fresh peer drip queues — then Close sees it and emits RST anyway.
	// Just trust SHUT_WR's FIN to be retransmitted until ACK'd.
	//
	// Graceful drain path retained for H2 (GOAWAY / RST_STREAM flushing)
	// and for truly detached WS/SSE conns (middleware-queued close
	// frames).
	plainClose := cs.h1State != nil && !trulyDetached && cs.h2State == nil
	switch {
	case plainClose:
		_ = unix.Shutdown(fd, unix.SHUT_WR)
		_ = unix.Close(fd)
	default:
		_ = unix.Shutdown(fd, unix.SHUT_WR)
		sockopts.CloseDrain(fd, "epoll/closeConn", cs.remoteAddr)
		_ = unix.Close(fd)
	}
	if h := testHookConnClosed; h != nil {
		h(l, fd)
	}
	l.removeLiveConn(cs)
	l.connCount--
	l.activeConns.Add(-1)
	l.closeCount.Add(1)

	if l.cfg.OnDisconnect != nil {
		l.cfg.OnDisconnect(cs.remoteAddr)
	}

	// Never release the connState while a goroutine may still hold a
	// reference (WS/SSE detach OR async dispatch). GC collects cs once
	// the goroutine finishes and all closure references are dropped.
	if !detached {
		l.dropAsk(cs) // celeris#657 P8: never pool a connState an ask still names
		releaseConnState(cs)
	}
}

func (l *Loop) shutdown() {
	// celeris#874: the run loop moves the per-iteration batches into the
	// shared counters at the end of every iteration and has returned by now,
	// but the send drain below writes with onLoopThread=true, into
	// bytesWrittenBatch, and nothing moves it after that. Flushed on the way
	// out, after the last thing that can write, so Metrics() read once the
	// engine has stopped is exact. Once, off the iteration path: the run
	// loop's own flush is untouched.
	defer l.flushBatchedCounters()

	// First: close every adoption still queued, and refuse every AdoptConn
	// from here on. Everything below walks the conn table only, which a
	// queued adoption is not in yet (celeris#658).
	l.closeAdoptQueue()
	l.shutdownDrivers()

	// Phase 1: signal every async dispatch / detached goroutine to stop and
	// tear down protocol state — but do NOT close any fd and do NOT release
	// any connState yet. asyncClosed + the cond Broadcast MUST happen before
	// the asyncWG.Wait() below, or Wait deadlocks on a goroutine parked in
	// asyncCond.Wait(). fds and the pool release are deferred to phase 3 so a
	// still-running dispatch goroutine cannot write into a closed/reused fd
	// or touch pooled-and-reissued connState memory (the shutdown counterpart
	// to the hijackConn use-after-release fix). Iterate liveConns by reverse
	// index for consistency with checkTimeouts (no removal happens here, but
	// keeping the idiom avoids surprises).
	for i := len(l.liveConns) - 1; i >= 0; i-- {
		cs := l.liveConns[i]
		if cs.hijacked.Load() {
			continue // the application's since the Hijack (celeris#668)
		}
		detached := cs.detachMu != nil
		if detached {
			cs.asyncClosed.Store(true)
			if cs.asyncCond.L != nil {
				cs.asyncInMu.Lock()
				cs.asyncCond.Broadcast()
				cs.asyncInMu.Unlock()
			}
			cs.detachMu.Lock()
			// A read-end error parked for a close that this shutdown finishes
			// in its place (closeOnReadEnd met a running handler) is delivered
			// as closeConn delivers it: under the lock, before detachClosed and
			// OnDetachClose, or the middleware is never told (celeris#867).
			// The flush closeConn does first is the send drain's here.
			if err := cs.closeErr; err != nil {
				cs.closeErr = nil
				if cs.h1State != nil && cs.h1State.OnError != nil {
					cs.h1State.OnError(err)
				}
			}
			cs.detachClosed = true
			// Acquire barrier — see closeConn: skip OnDetachClose until the WS
			// upgrade has fully wired the conn (WSReady) to avoid racing the
			// post-Detach wiring on the async goroutine.
			if cs.h1State != nil && cs.h1State.WSReady.Load() && cs.h1State.OnDetachClose != nil {
				cs.h1State.OnDetachClose()
				cs.h1State.OnDetachClose = nil
			}
			cs.detachMu.Unlock()
		}
		trulyDetached := detached && cs.h1State != nil && cs.h1State.Detached.Load()
		if !trulyDetached && cs.h1State != nil {
			// Mirror the closeConn fix: hold detachMu while tearing
			// down h1State so ProcessH1 in runAsyncHandler isn't still
			// reading the fields CloseH1 writes.
			if detached {
				cs.detachMu.Lock()
				// Re-read Detached under the lock (see closeConn): an in-flight
				// OnDetach publishes Detached before releasing detachMu, so
				// skip CloseH1 if the conn detached while we waited — else we
				// recycle a Context/stream the middleware goroutine still uses.
				if !cs.h1State.Detached.Load() {
					conn.CloseH1(cs.h1State)
				}
				cs.detachMu.Unlock()
			} else {
				conn.CloseH1(cs.h1State)
			}
		}
		if cs.h2State != nil {
			conn.CloseH2(cs.h2State)
		}
	}

	// Phase 2: wait for async dispatch goroutines to exit. They were
	// signaled (asyncClosed + Broadcast) in phase 1; this join guarantees
	// no goroutine is still inside ProcessH1 / writeFn against cs before we
	// close fds or recycle connState below.
	l.asyncWG.Wait()

	// Phase 2a (celeris#863): a deferred transplant's conn is in no table
	// phase 3 walks, and its hand-off will never be finished now.
	l.closeDeferredTransplants()

	// Phase 2b (celeris#760): every response the handlers wrote is queued
	// now; send what the sockets have not taken yet before phase 3 closes
	// them, as io_uring does before its shutdown (celeris#595). Closing at
	// once cut off the tail of any response larger than the socket buffers
	// to a client that reads more slowly than the loop shuts down.
	l.drainSends()

	// Phase 3: now that the async dispatch goroutines have exited (phase 2),
	// close the fds and release the connState back to the pool. The pool
	// release stays gated on !detached for the same reason as closeConn: a
	// truly-detached WS/SSE conn's middleware goroutine is NOT tracked by
	// asyncWG and may still hold a reference, so we let GC reclaim cs once
	// that goroutine drops its closure refs rather than recycling it here.
	for i := len(l.liveConns) - 1; i >= 0; i-- {
		cs := l.liveConns[i]
		if cs.hijacked.Load() {
			// hijackConn closed this conn's descriptor already, and the
			// number may be reissued: closing it here would close another
			// conn's (celeris#668).
			continue
		}
		fd := cs.fd
		// The slot goes first, under driverMu and around the slot write
		// alone, never across the close (which can wait on SO_LINGER,
		// celeris#735): a driver's RegisterConn reads it, under the same
		// lock, from its own goroutine, and the number is free to be
		// reissued once the descriptor is closed (celeris#775).
		l.driverMu.Lock()
		l.conns[fd] = nil
		l.driverMu.Unlock()
		_ = unix.Close(fd)
		if cs.detachMu == nil {
			l.dropAsk(cs) // celeris#657 P8: never pool a connState an ask still names
			releaseConnState(cs)
		}
	}
	clear(l.liveConns)
	l.liveConns = l.liveConns[:0]
	// celeris#657 R2: this loop holds nothing now, so it must not leave its
	// last cycle's residue standing in the engine-wide gauges. Nothing else
	// retracts it — the sweep does not run after shutdown.
	l.sweepRetract()

	if l.listenFD >= 0 {
		_ = unix.Close(l.listenFD)
		l.listenFD = -1 // a Goexit in a later step must not close the number twice (reportLoopExit)
	}
	// The loop has left the SO_REUSEPORT group for good. PauseAccept waits
	// for this flag, and a loop that exits in the middle of a pause linger
	// would otherwise leave it false until PauseAccept's own bound ran out
	// (celeris#662).
	l.listenFDClosed.Store(true)
	l.pause.Notify()
	// celeris#655: Close waits for the signals already in flight and turns
	// every later one into a no-op, so the producers asyncWG does not track
	// — detached WS/SSE callbacks, the H2 write queue — cannot write this
	// descriptor number once it is free to be recycled.
	l.wakeFD.Close()
	if l.timerFD >= 0 {
		_ = unix.Close(l.timerFD)
	}
	// celeris#655: the same barrier for the other descriptor a driver
	// goroutine can reach. closeEpollFD refuses every later driver
	// epoll_ctl, so none of them can operate on this number once it is free
	// to be recycled.
	l.closeEpollFD()
}

// flushBatchedCounters moves the loop's per-iteration batches (requests, bytes
// received, bytes sent) into the engine-wide counters. The run loop does the
// same inline at the end of each iteration (kept inline: it is the per-event
// path); this is shutdown's copy, for what the loop's last work after that
// flush added (celeris#874). Loop thread.
func (l *Loop) flushBatchedCounters() {
	if l.reqBatch > 0 {
		l.reqCount.Add(l.reqBatch)
		l.reqBatch = 0
	}
	if l.bytesReadBatch > 0 {
		l.bytesRead.Add(l.bytesReadBatch)
		l.bytesReadBatch = 0
	}
	if l.bytesWrittenBatch > 0 {
		l.bytesWritten.Add(l.bytesWrittenBatch)
		l.bytesWrittenBatch = 0
	}
}

// h2PoolSettled reports whether the loop, its context cancelled, may shut
// down as far as HTTP/2 is concerned (celeris#759). A stream on an async
// route runs its handler on the shared worker pool, off this loop, and its
// response comes back through the conn's write queue, which only this loop
// drains; shutdown cancelled such streams and closed their conns under their
// handlers, so the client got unexpected EOF, and the hooks ran before the
// handlers had finished. So the loop keeps turning, reading and writing as
// usual on the conns it has (it accepts no new one: stopAccepting), until no
// HTTP/2 conn has a pool handler running, a response still in its write
// queue, or response DATA waiting for the client's WINDOW_UPDATE. Every
// HTTP/2 conn is sent GOAWAY first, so its client opens no new stream on it,
// as net/http's graceful shutdown does, and a stream it opens anyway is
// refused. The wait is bounded like the send drain (sendDrainWait): the
// budget of the last Engine.Shutdown, and never less than
// shutdownSendDrainFloor. Loop thread.
func (l *Loop) h2PoolSettled() bool {
	if len(l.h2Conns) == 0 {
		return true
	}
	if l.h2DrainStart.IsZero() {
		l.h2DrainStart = time.Now()
	}
	busy := false
	for _, fd := range l.h2Conns {
		cs := l.conns[fd]
		if cs == nil || cs.h2State == nil {
			continue
		}
		if !cs.h2GoAwaySent {
			mu := cs.detachMu
			if mu != nil {
				mu.Lock()
			}
			cs.h2GoAwaySent = cs.h2State.GoAway(cs.writeFn)
			if mu != nil {
				mu.Unlock()
			}
			if cs.h2GoAwaySent {
				l.markDirty(cs) // the dirty pass flushes the GOAWAY
			}
		}
		if cs.h2State.PoolHandlersRunning() || cs.h2State.WriteQueuePending() || cs.h2State.OutboundPending() {
			busy = true
		}
	}
	if !busy {
		return true
	}
	_, more := l.sendDrainWait(l.h2DrainStart)
	return !more
}

// stopAccepting takes the listener out of the epoll set and closes it, once
// the loop's context is cancelled (celeris#759). The loop may go on turning
// after that, for as long as its HTTP/2 conns keep it (h2PoolSettled), and it
// used to accept and serve new connections meanwhile, which were then cut at
// the budget; net/http's Shutdown closes its listeners first. What is still
// in the kernel's accept queue is reset, as the close in shutdown did.
// Idempotent. Loop thread.
func (l *Loop) stopAccepting() {
	if l.listenFD < 0 {
		return
	}
	_ = unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_DEL, l.listenFD, nil)
	_ = unix.Close(l.listenFD)
	l.listenFD = -1
	l.listenHot = false
	l.lingerUntil = 0
}

// drainSends is shutdown's send drain (celeris#760). It flushes every live
// conn with response bytes still queued, and waits for their sockets to take
// more, until nothing is queued or the drain's time is up (sendDrainWait). A
// conn whose write fails is left to phase 3's close. The run loop is no
// longer turning: the conns are polled directly (poll(2), POLLOUT), not
// through the epoll set.
//
// Loop thread, after phase 2: no dispatch goroutine is left to write, and a
// detached conn's middleware finds detachClosed set by phase 1 and writes
// nothing more. detachMu is still taken around each flush, as every flush
// site does.
func (l *Loop) drainSends() {
	start := time.Now()
	var fds []unix.PollFd
	for {
		fds = fds[:0]
		for i := len(l.liveConns) - 1; i >= 0; i-- {
			cs := l.liveConns[i]
			if cs.hijacked.Load() {
				continue // the application's since the Hijack (celeris#668)
			}
			mu := cs.detachMu
			if mu != nil {
				mu.Lock()
			}
			pending := false
			if csWritePending(cs) && l.flushWrites(cs, true) == nil {
				pending = csWritePending(cs)
			}
			if mu != nil {
				mu.Unlock()
			}
			if pending {
				fds = append(fds, unix.PollFd{Fd: int32(cs.fd), Events: unix.POLLOUT})
			}
		}
		if len(fds) == 0 {
			return
		}
		wait, ok := l.sendDrainWait(start)
		if !ok {
			return
		}
		if h := drainPollHook.Load(); h != nil {
			(*h)() // tests only (SetDrainPollHook)
		}
		if _, err := unix.Poll(fds, int(wait/time.Millisecond)+1); err != nil && err != unix.EINTR {
			return
		}
	}
}

// sendDrainWait reports how long drainSends may wait for a writable socket
// now, at most shutdownSendDrainPoll, and false once the drain's time is up.
//
// The drain, begun at start, runs while the budget the last Engine.Shutdown
// call handed over (drainBudget) is live: until its ctx's deadline, and for
// a ctx with none (context.Background, a WithCancel ctx: net/http's "wait as
// long as it takes") until the ctx is done. Either way no longer than
// WriteTimeout after the drain began, when that is set: the bound a live
// conn's stalled write gets, and net/http's, so a client that never reads
// cannot hold Shutdown(context.Background()) for ever. It never ends before
// shutdownSendDrainFloor, which is also all it gets once the budget is done
// (at its deadline, or cancelled before it) or when no Shutdown handed one
// over.
func (l *Loop) sendDrainWait(start time.Time) (time.Duration, bool) {
	end := start.Add(shutdownSendDrainFloor)
	if l.drainBudget != nil {
		if p := l.drainBudget.Load(); p != nil && (*p).Err() == nil {
			ext, bounded := (*p).Deadline()
			if wt := l.cfg.WriteTimeout; wt > 0 && (!bounded || start.Add(wt).Before(ext)) {
				ext, bounded = start.Add(wt), true
			}
			if !bounded {
				return shutdownSendDrainPoll, true // until the budget is done
			}
			if ext.After(end) {
				end = ext
			}
		}
	}
	left := time.Until(end)
	if left <= 0 {
		return 0, false
	}
	return min(left, shutdownSendDrainPoll), true
}

// createListenSocket binds and listens on addr. deferAccept asks for
// TCP_DEFER_ACCEPT (resource.Config.DisableDeferAccept turns it off). A
// pause clears the option on this socket and lingers before it closes it
// (stepAcceptPause), so the option costs a pause time, not connections
// (celeris#662).
func createListenSocket(addr string, deferAccept bool) (int, error) {
	sa, err := parseAddr(addr)
	if err != nil {
		return -1, err
	}

	family := unix.AF_INET
	if _, ok := sa.(*unix.SockaddrInet6); ok {
		family = unix.AF_INET6
	}

	fd, err := unix.Socket(family, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("socket: %w", err)
	}

	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}

	// TCP_NODELAY on the listen socket: Linux copies it onto every accepted
	// socket at SYN time, so per-accept ApplyFD can skip its own NODELAY
	// setsockopt (one fewer syscall per accept on the hot path).
	_ = unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)
	// TCP_DEFER_ACCEPT: the kernel holds a connection out of the accept queue
	// until its first data arrives, saving a wakeup per idle connection. It
	// also hides that connection from acceptQueuedOnPause, which is why a
	// pause clears it and lingers before the close (celeris#662).
	if deferAccept {
		_ = unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_DEFER_ACCEPT, 1)
	}
	// TCP_FASTOPEN: allow data in SYN packet, saving 1 RTT for TFO-capable clients.
	_ = unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_FASTOPEN, 256)

	if err := bindiag.BindWithRetry(fd, sa); err != nil {
		diag := bindiag.Format(fd, sa)
		_ = unix.Close(fd)
		return -1, fmt.Errorf("bind: %w [%s]", err, diag)
	}
	if err := unix.Listen(fd, 4096); err != nil {
		diag := bindiag.Format(fd, sa)
		_ = unix.Close(fd)
		return -1, fmt.Errorf("listen: %w [%s]", err, diag)
	}
	return fd, nil
}

// listenAddrOf is boundAddr behind a var so a test can make every loop fail
// to report its address (celeris#639).
var listenAddrOf = boundAddr

func boundAddr(fd int) net.Addr {
	return bindiag.BoundAddr(fd)
}

// sockaddrString formats a peer address as std does; see bindiag.SockaddrString
// (celeris#925).
func sockaddrString(fd int, sa unix.Sockaddr) string {
	return bindiag.SockaddrString(fd, sa)
}

func parseAddr(addr string) (unix.Sockaddr, error) {
	host, portStr := "", addr

	// Handle IPv6 bracket notation: [::1]:8080, [::]:8080
	if len(addr) > 0 && addr[0] == '[' {
		closeBracket := -1
		for i := 1; i < len(addr); i++ {
			if addr[i] == ']' {
				closeBracket = i
				break
			}
		}
		if closeBracket < 0 {
			return nil, fmt.Errorf("invalid addr: missing closing bracket: %s", addr)
		}
		host = addr[1:closeBracket]
		if closeBracket+1 < len(addr) && addr[closeBracket+1] == ':' {
			portStr = addr[closeBracket+2:]
		} else {
			return nil, fmt.Errorf("invalid addr: missing port after bracket: %s", addr)
		}
	} else {
		for i := len(addr) - 1; i >= 0; i-- {
			if addr[i] == ':' {
				host = addr[:i]
				portStr = addr[i+1:]
				break
			}
		}
	}

	port := 0
	for _, c := range portStr {
		if c < '0' || c > '9' {
			return nil, fmt.Errorf("invalid port: %s", portStr)
		}
		port = port*10 + int(c-'0')
		if port > 65535 {
			// "1.2.3.4:18446744073709551616" wrapped to port 0, which Listen
			// then took for a request for an ephemeral port.
			return nil, fmt.Errorf("invalid port: %s", portStr)
		}
	}

	if host == "" {
		return &unix.SockaddrInet4{Port: port}, nil
	}

	// The host is what net.ParseIP says it is, as in std: an IPv4 address (an
	// IPv4-mapped IPv6 one, "::ffff:1.2.3.4", included) binds as IPv4, any
	// other IPv6 address as IPv6, and what does not parse is an error. That
	// refuses what the octet loop this replaced refused by hand, with std's
	// own rules: a byte over 255 ("300.1.1.1" bound 44.1.1.1), a leading zero
	// ("0127.0.0.1": octal in some parsers), a fifth byte, and the shorthands
	// inet_aton reads ("10", "127.1": "1.2.3" bound 1.2.3.0).
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, fmt.Errorf("invalid addr: %s", addr)
	}
	if ip4 := ip.To4(); ip4 != nil {
		sa := &unix.SockaddrInet4{Port: port}
		copy(sa.Addr[:], ip4)
		return sa, nil
	}
	sa := &unix.SockaddrInet6{Port: port}
	copy(sa.Addr[:], ip.To16())
	return sa, nil
}
