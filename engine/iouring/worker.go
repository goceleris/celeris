//go:build linux

package iouring

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/engine/internal/bindiag"
	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/ctxkit"
	"github.com/goceleris/celeris/internal/platform"
	"github.com/goceleris/celeris/internal/sockopts"
	"github.com/goceleris/celeris/protocol/detect"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"

	"golang.org/x/sys/unix"
)

// fixedFileTableSize is the number of slots in the fixed file table.
// Must accommodate the maximum number of concurrent connections per worker.
const fixedFileTableSize = 65536

// errIORingRecv wraps a negative io_uring recv result as a syscall.Errno.
// Used to surface concrete errors to detached middleware via H1State.OnError.
func errIORingRecv(res int32) error {
	return unix.Errno(uint32(-res))
}

// errIORingSend wraps a negative io_uring send result as a syscall.Errno.
func errIORingSend(res int32) error {
	return unix.Errno(uint32(-res))
}

// bufRingGroupID is the provided buffer ring group ID.
const bufRingGroupID = 0

// bufRingCountMin is the floor for the scaled provided-buffer-ring size.
// Below ~1024 entries the kernel reports ENOBUFS under 1500+ conn churn
// (the per-conn re-arm path fires constantly and burns the latency
// budget). The formula below keeps this as the floor and scales up from
// there. See resolveBufRingCount for the full rationale.
const bufRingCountMin = 1024

// bufRingCountMax caps the scaled provided-buffer-ring size. The hard
// ceiling is the kernel's IORING_REGISTER_PBUF_RING limit of 32768
// entries (a ring larger than that is rejected by the kernel) — which is
// also the largest count the uint16 tail/mask/bid arithmetic in
// BufferRing can address. At the default 8 KiB BufferSize this is 256 MiB
// of buffer memory per worker at the absolute max; the auto-scaling
// formula stays far below this in practice. Operators with unusual
// workloads can override via the env var, but never past this cap.
const bufRingCountMax = 1 << 15 // 32768 entries × 8 KiB = 256 MiB worst case (kernel PBUF_RING cap)

// CELERIS_IOURING_PBUF_COUNT overrides the auto-scaled provided-buffer-ring
// size. Must be a power of 2 and at least bufRingCountMin. Use this when
// the default scaling formula under-provisions your workload — typically
// the case for very-high-concurrency benchmarks (16k+ connections) where
// each worker may have more in-flight multishot recvs than the formula
// anticipates. Setting 0 (or leaving the env var unset) reverts to
// auto-scaling from Workers × TargetConnsPerWorker.
const envPbufCount = "CELERIS_IOURING_PBUF_COUNT"

// resolveBufRingCount picks the provided-buffer-ring size for a worker.
// The default formula is `nextPowerOf2(max(bufRingCountMin, 2 *
// TargetConnsPerWorker))`, i.e. two buffers per conn in the scaler's
// steady-state target — enough headroom that the kernel rarely stalls
// waiting for buffer returns. Above 1024 conns the previous hard-coded
// 1024 was too small: buffers were reused aggressively, the kernel
// stalled, and the very behaviour the ring is designed to optimise
// (multishot recv CQE batching) collapsed into CQE storms (celeris#322).
//
// The ring is PER-WORKER: NewBufferRing is created once per Worker on its
// own ring, so the scaling MUST be driven by the per-worker conn target,
// NOT by the engine-wide Workers count. Multiplying by Workers over-sized
// every worker's ring by the worker count, wasting count×BufferSize of
// mmap'd RSS per worker and risking the kernel cap on large boxes
// (celeris#322 follow-up).
// Operators can override via CELERIS_IOURING_PBUF_COUNT.
func resolveBufRingCount(_ resource.ResolvedResources, scalerTargetConnsPerWorker int) int {
	if v := os.Getenv(envPbufCount); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n&(n-1) != 0 {
				n = resource.NextPowerOf2(n)
			}
			return clampBufRingCount(n)
		}
	}
	target := scalerTargetConnsPerWorker
	if target <= 0 {
		target = 20 // mirrors scaler.Resolve's default
	}
	scaled := 2 * target
	if scaled < bufRingCountMin {
		scaled = bufRingCountMin
	}
	return clampBufRingCount(resource.NextPowerOf2(scaled))
}

func clampBufRingCount(n int) int {
	if n < bufRingCountMin {
		return bufRingCountMin
	}
	if n > bufRingCountMax {
		return bufRingCountMax
	}
	return n
}

// pendingReleaseEntry queues a connState for deferred release — its
// fd has been closed but the kernel may still have SQEs referencing
// its buffers; we hold it until cs.kernelInflight reports every
// kernel-held op has produced its terminal CQE (the close path submits
// ASYNC_CANCELs so this is normally within a loop pass or two), with
// releaseAtNanos as the wall-clock BACKSTOP for kernel anomalies. See
// Worker.pendingRelease docstring.
//
// detached entries (async-dispatch / WS / SSE conns) skip the pool
// recycle: their cs has live state (h1State, asyncCond, asyncInBuf
// slices) that goroutine closures may still reference even after the
// worker observes asyncClosed and runs finishCloseDetached. Holding
// the strong ref alive past the kernel's recv-SQE drain window is
// what we need; the recycle path resetting fields would race with the
// goroutine's defer that reads cs.fd / cs.asyncInBuf. (The goroutine's
// own closure references remain visible to GC after we drop ours, so
// dropping the ref once the kernel ops drained is safe.)
type pendingReleaseEntry struct {
	cs             *connState
	releaseAtNanos int64
	detached       bool
}

// closedOpsEntry is the Worker.closedOps value: the conn(s) closed under
// one (fd, generation) user_data identity and the total number of
// terminal recv/send CQEs the kernel still owes them. conns has one
// element except under an fd+generation collision, where the CQEs are
// indistinguishable and every colliding conn is held until the combined
// count drains (release-late is safe; release-early is the UAF).
type closedOpsEntry struct {
	inflight int32
	conns    []*connState
}

// pendingReleaseHoldNanos is the WALL-CLOCK BACKSTOP for releasing a
// closed connState whose kernel ops never produced a terminal CQE.
//
// It is NOT the primary release gate. Release is gated on
// cs.kernelInflight == 0: the close path ASYNC_CANCELs the armed
// recv/send by generation-tagged user_data and drainPendingRelease
// frees the connState only once every cancelled/completed op has
// delivered its terminal CQE. That is exact — no window to
// tune. The wall clock only matters if the kernel never delivers a
// terminal CQE at all (or the cancel SQE was dropped on a full SQ
// ring); when it fires, drainPendingRelease logs a WARN because a
// kernel-held op may still reference cs.buf and releasing is a
// last-resort trade of a potential use-after-free against an
// unbounded memory leak.
//
// 5 s is comfortably above any plausible straggler window: TCP
// retransmits begin at RTO_MIN = 200 ms and a 4 KiB POST tail
// straggling through several backoffs stays well under a second —
// the 100 ms hold this replaces sat BELOW RTO_MIN, which is exactly
// how the v1.4.15/7beebb9 heap corruption slipped past it.
//
// The backstop MUST stay time-based, not iteration-based: under
// churn-close the loop spins sub-millisecond per iteration while idle
// iterations stretch to 100 ms+ (see the v1.5.0 review 2.9 history on
// queuePendingRelease).
const pendingReleaseHoldNanos int64 = int64(5 * time.Second)

// Worker is an io_uring event-loop worker pinned to a single OS thread.
type Worker struct {
	id         int
	cpuID      int
	ring       *Ring
	listenFD   int
	tier       TierStrategy
	fixedFiles bool // runtime flag: true if ACCEPT_DIRECT is working
	sqpoll     bool // true when SQPOLL is active (kernel submits SQEs)
	sendZC     bool // true when SEND_ZC is available (kernel 6.0+)
	async      bool // true when Config.AsyncHandlers dispatches handlers to spawned Gs
	// h1Only is true when engine config locks every conn to HTTP/1.1
	// (Protocol == HTTP1 AND EnableH2Upgrade == false). cs.protocol is set
	// once at registerConn and never written, so the recv hot path can
	// skip the atomic Load.
	h1Only bool

	// asyncWG tracks runAsyncHandler goroutines so graceful shutdown
	// can Wait on them before returning. See engine/epoll/loop.go
	// for rationale — keeps dispatch Gs from touching connState
	// memory after the engine claims to have stopped.
	asyncWG   sync.WaitGroup
	conns     []*connState
	connCount int // number of active connections (local, for draining check)
	maxFD     int // upper bound fd for iteration in checkTimeouts/shutdown
	// liveConns is a dense slice of currently-active FDs, maintained
	// alongside the sparse conns map. checkTimeouts and shutdown iterate
	// liveConns to avoid the O(maxFD) scan that dominated the worker
	// hot path above ~8 Ki conns (celeris#318). Append-on-register,
	// swap-with-last-on-deregister; the slice is owned by the worker
	// thread so no locking is required.
	liveConns    []int
	handler      stream.Handler
	resolved     resource.ResolvedResources
	sockOpts     sockopts.Options
	bufRing      *BufferRing // ring-mapped provided buffers for multishot recv
	logger       *slog.Logger
	cfg          resource.Config
	ready        chan error
	acceptPaused *atomic.Bool
	wake         chan struct{}
	wakeMu       sync.Mutex
	suspended    atomic.Bool
	// inactive is the per-worker pause flag used by the dynamic worker
	// scaler. ORed with acceptPaused (which is engine-wide) when computing
	// effective paused state. The scaler flips this to deactivate idle
	// workers under low load and reactivate them under burst load.
	inactive atomic.Bool
	// listenFDClosed signals that the worker has cancelled in-flight
	// accept SQEs and closed its listen FD in response to acceptPaused
	// being set. PauseAccept polls this so it only returns once the
	// SO_REUSEPORT group has actually shed this listener — important
	// for the adaptive engine where the standby's listen sockets must
	// be out of the kernel routing pool before the bound address is
	// exposed (otherwise a fresh dial can land on the standby and get
	// RST as it pauses).
	listenFDClosed atomic.Bool

	reqCount      *atomic.Uint64
	activeConns   *atomic.Int64
	errCount      *atomic.Uint64
	asyncPromoted *atomic.Uint64 // cumulative inline → dispatch promotions (#300)
	reqBatch      uint64         // batched request count, flushed to reqCount per iteration

	tickCounter uint32
	cachedNow   int64  // cached time.Now().UnixNano(), refreshed every 64 iterations
	iterCount   uint64 // monotonic event-loop iteration counter (for pendingRelease)

	// pendingRelease defers returning connState structs to the pool
	// until the kernel has drained every in-flight I/O SQE that may
	// still hold pointers into cs.buf / cs.sendBuf. unix.Close(fd)
	// does NOT complete a pending io_uring recv (the op holds its own
	// file reference — only inbound data, FIN, or an ASYNC_CANCEL
	// completes it), so the close path cancels the armed ops and this
	// queue holds cs until cs.kernelInflight confirms their terminal
	// CQEs arrived. If we released cs earlier, Go's GC could reclaim
	// cs.buf's backing array; the kernel then writes straggler bytes
	// (e.g. a retransmitted POST segment at RTO ≥ 200 ms) into memory
	// the runtime has repurposed — historically a SIGSEGV in
	// runtime.stackalloc dereferencing HTTP bytes as a pointer (#256),
	// and on Go 1.26 (Green Tea GC, in-span alloc/mark bits) a fatal
	// "s.allocCount != s.nelems" span-corruption (v1.4.15/7beebb9, both bench runs). Neither is
	// race-detectable because the writer is the kernel, not Go code.
	// releaseAtNanos is only the anomaly backstop — see
	// pendingReleaseHoldNanos.
	pendingRelease []pendingReleaseEntry

	// closedOps routes terminal recv/send CQEs that arrive AFTER their
	// conn was closed (w.conns[fd] already nil or reused) back to the
	// closed connState for kernelInflight accounting. Keyed by
	// connOpKey (generation<<40 | fd) — the op tag is stripped so one
	// entry covers a conn's recv and send. Populated by
	// noteClosedInflight on the close paths, consumed by
	// noteStaleTerminalOp at CQE dispatch; nil/empty whenever no
	// closed conn has kernel ops outstanding, so the hot path pays a
	// single len check. Worker-thread-only.
	//
	// A (fd, generation) collision — two closed conns whose ops share
	// a user_data identity, possible under fd reuse because generations
	// are per-connState-object — makes their terminal CQEs mutually
	// indistinguishable, so the entry carries a COMBINED count and
	// releases all colliding conns only after every expected terminal
	// CQE arrived (errs toward holding longer; see closedOpsEntry).
	closedOps map[uint64]*closedOpsEntry

	dirtyHead      *connState // head of intrusive doubly-linked dirty list
	hasBufReturns  bool       // set when provided buffers need publishing
	sendsPending   bool       // true when SEND SQEs are in the SQ ring (guarantees CQE production)
	h2Conns        []int      // FDs of H2 connections (for write queue polling)
	h2EventFD      int        // eventfd for H2 write queue wakeup (-1 if unavailable)
	h2PollArmed    bool       // true when POLL_ADD is active on h2EventFD
	h2cfg          conn.H2Config
	emptyIters     uint32 // consecutive iterations with zero CQEs (for adaptive timeout)
	detachQueue    []*connState
	detachQMu      sync.Mutex
	detachQSpare   []*connState
	detachQPending atomic.Int32 // 1 when detachQueue has entries; gates the hot-path drain
	detachedCount  int          // number of currently-detached conns; gates idle-deadline sweep

	// EventLoopProvider state. driverConns is keyed by real FD and is
	// completely disjoint from the HTTP conns array. hasDriverConns is the
	// zero-cost gate: when false, the HTTP fast path pays no overhead.
	driverConns         map[int]*driverConn
	driverMu            sync.RWMutex
	hasDriverConns      atomic.Bool
	driverActionQueue   []driverAction
	driverActionSpare   []driverAction
	driverActionMu      sync.Mutex
	driverActionPending atomic.Int32
}

func newWorker(id, cpuID int, tier TierStrategy, handler stream.Handler,
	resolved resource.ResolvedResources,
	cfg resource.Config, reqCount *atomic.Uint64, activeConns *atomic.Int64, errCount *atomic.Uint64,
	asyncPromoted *atomic.Uint64, acceptPaused *atomic.Bool) (*Worker, error) { //nolint:unparam // error return used by callers for future fallible init

	// Listen socket creation is deferred to run() (after CPU pinning and NUMA
	// binding) so that the kernel allocates socket internal buffers on the
	// worker's NUMA node. This eliminates cross-socket access for accept
	// queue operations on multi-socket systems.

	return &Worker{
		id:            id,
		cpuID:         cpuID,
		listenFD:      -1,
		h2EventFD:     -1,
		tier:          tier,
		sqpoll:        tier.SQPollIdle() > 0,
		sendZC:        tier.SupportsSendZC(),
		async:         cfg.AsyncHandlers,
		h1Only:        cfg.Protocol == engine.HTTP1 && !cfg.EnableH2Upgrade,
		conns:         make([]*connState, fixedFileTableSize),
		liveConns:     make([]int, 0, 1024),
		handler:       handler,
		resolved:      resolved,
		cfg:           cfg,
		logger:        cfg.Logger,
		reqCount:      reqCount,
		activeConns:   activeConns,
		errCount:      errCount,
		asyncPromoted: asyncPromoted,
		acceptPaused:  acceptPaused,
		wake:          make(chan struct{}),
		ready:         make(chan error, 1),
		h2cfg: conn.H2Config{
			MaxConcurrentStreams: cfg.MaxConcurrentStreams,
			InitialWindowSize:    cfg.InitialWindowSize,
			MaxFrameSize:         cfg.MaxFrameSize,
			MaxRequestBodySize:   cfg.MaxRequestBodySize,
		},
		sockOpts: sockopts.Options{
			TCPNoDelay:  true,
			TCPQuickAck: true,
			SOBusyPoll:  50 * time.Microsecond,
			RecvBuf:     resolved.SocketRecv,
			SendBuf:     resolved.SocketSend,
		},
	}, nil
}

func (w *Worker) run(ctx context.Context) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	_ = platform.PinToCPU(w.cpuID)

	// Bind memory allocations to this CPU's NUMA node before creating
	// the listen socket, ring, and buffers. This ensures the socket's
	// accept queue, mmap'd SQ/CQ rings, SQE arrays, and provided buffer
	// regions are all NUMA-local to the worker thread, eliminating
	// cross-socket QPI/UPI traffic on multi-socket systems.
	numaNode := platform.CPUForNode(w.cpuID)
	if err := platform.BindNumaNode(numaNode); err == nil {
		defer func() { _ = platform.ResetNumaPolicy() }()
	}

	// Create the listen socket on the worker's NUMA node. Each worker has its
	// own listen socket via SO_REUSEPORT; kernel allocates socket internals
	// (accept queue, buffers) on the current thread's NUMA node.
	listenFD, err := createListenSocket(w.cfg.Addr)
	if err != nil {
		w.ready <- fmt.Errorf("worker %d: listen socket: %w", w.id, err)
		return
	}
	w.listenFD = listenFD

	// Create ring after LockOSThread — SINGLE_ISSUER requires all ring
	// operations from the same OS thread. NewRingCPU pins the kernel's SQPOLL
	// thread to the same CPU as this worker, ensuring NUMA-local SQ ring polling.
	ring, err := NewRingCPU(uint32(w.resolved.SQERingSize), w.tier.SetupFlags(), w.tier.SQPollIdle(), w.cpuID)
	if err != nil {
		w.ready <- fmt.Errorf("worker %d ring setup: %w", w.id, err)
		return
	}
	w.ring = ring

	// Register fixed file table if the tier supports it.
	if w.tier.SupportsFixedFiles() {
		if err := w.ring.RegisterFiles(fixedFileTableSize); err != nil {
			w.logger.Warn("fixed file table registration failed, falling back",
				"worker", w.id, "err", err)
		} else {
			w.fixedFiles = true
		}
	}

	// Multishot recv + ring-mapped provided buffers is OFF by default.
	//
	// Under sustained HTTP/1 Connection:close churn against a client
	// that pools connections (Go's http.Client with keep-alive of
	// course does NOT pool closed conns, but load balancers, service
	// mesh sidecars, and raw-socket benchmarking tools like goceleris/
	// loadgen all hold a pool of open conns and expect the server to
	// close them), multishot recv on aarch64 kernel 6.6.10 throttles
	// the whole worker to ~30 accepts/s / ~90 req/s — a 100× collapse
	// vs the epoll engine's ~25 k req/s on the identical workload.
	// The profile showed workers drowning in spurious recv CQEs (25 k
	// per worker per second against ~50 useful completions), and
	// disabling multishot recv in favour of single-shot per-conn
	// recv (the same model epoll uses) recovered churn to ~35 k rps
	// while costing ≈2 % on keep-alive simple and being a wash on
	// json-64k / body / headers.
	//
	// Opt back in with CELERIS_IOURING_MULTISHOT_RECV=1 for workloads
	// that are known to be dominated by long-lived keep-alive conns
	// and that benefit from the CQE-batching multishot provides.
	//
	// The ring size scales with the worker's per-conn target (celeris#322):
	// the previous hard-coded 1024 entries was undersized above ~1024 conns
	// and produced CQE storms as the kernel stalled waiting for buffer
	// returns. The formula gives 2 buffers per conn at the scaler's
	// steady-state target — comfortable headroom without runaway RSS.
	// CELERIS_IOURING_PBUF_COUNT overrides the auto-scaled value.
	if w.tier.SupportsMultishotRecv() && os.Getenv("CELERIS_IOURING_MULTISHOT_RECV") == "1" {
		var targetConns int
		if w.cfg.WorkerScaling != nil {
			targetConns = w.cfg.WorkerScaling.TargetConnsPerWorker
		}
		bufRingCount := resolveBufRingCount(w.resolved, targetConns)
		br, err := NewBufferRing(w.ring, bufRingGroupID, bufRingCount, w.resolved.BufferSize)
		if err != nil {
			w.logger.Warn("ring-mapped buffer registration failed, using per-connection buffers",
				"worker", w.id, "err", err, "buf_ring_count", bufRingCount)
		} else {
			w.bufRing = br
		}
	}

	// Create eventfd for H2 write queue wakeup. Handler goroutines signal
	// the eventfd after enqueuing response frames; io_uring POLL_ADD on the
	// eventfd wakes the ring event-driven, replacing the 100μs polling timeout.
	efd, efdErr := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
	if efdErr != nil {
		efd = -1
	}
	w.h2EventFD = efd

	w.prepareAccept()
	if _, err := w.ring.Submit(); err != nil {
		w.ready <- fmt.Errorf("worker %d initial submit: %w", w.id, err)
		return
	}

	w.ready <- nil
	w.cachedNow = time.Now().UnixNano()

	for {
		if ctx.Err() != nil {
			w.shutdown()
			return
		}

		// ACTIVE → DRAINING: cancel pending io_uring operations on the listen
		// socket, then close it. The cancel releases the kernel's io_uring
		// reference to the underlying file, allowing the socket to leave the
		// SO_REUSEPORT group immediately. Without this, unix.Close alone
		// leaves a phantom socket that intercepts connections.
		// Cache the atomic load: same value used by the two branches
		// below and (further down) the SUSPENDED check. Saves 2 atomic
		// loads per event-loop iteration on the steady-state hot path.
		// OR with the per-worker inactive flag (dynamic scaler).
		paused := w.acceptPaused.Load() || w.inactive.Load()
		if w.listenFD >= 0 && paused {
			if sqe := w.ring.GetSQE(); sqe != nil {
				prepCancelFDSkipSuccess(sqe, w.listenFD)
				setSQEUserData(sqe, 0)
				// Submit and wait for the cancel to complete before closing.
				_ = w.ring.SubmitAndWaitTimeout(50 * time.Millisecond)
				// Process CQEs: skip cancel completions (userData=0),
				// handle everything else normally to avoid breaking
				// active connections.
				cancelNow := time.Now().UnixNano()
				cqH, cqT := w.ring.BeginCQ()
				for cqH != cqT {
					entry := w.ring.cqeAt(cqH)
					if entry.UserData != 0 {
						w.processCQE(ctx, entry, cancelNow)
					}
					cqH++
				}
				w.ring.EndCQ(cqH)
			}
			_ = unix.Close(w.listenFD)
			w.listenFD = -1
		}
		// Maintain the listenFDClosed signal that PauseAccept polls on.
		// Set it whenever paused==true regardless of whether we just
		// closed the FD or it was already -1 from a prior Pause-Resume
		// cycle that hadn't re-created the socket yet (the worker may
		// be mid-iteration when paused flips back to true, so listenFD
		// can transiently be -1 while paused is true). Clear when
		// paused goes false so a subsequent Pause observes a fresh
		// signal once the new listenFD is created.
		if paused {
			w.listenFDClosed.Store(true)
		} else {
			w.listenFDClosed.Store(false)
		}

		// SUSPENDED → ACTIVE: re-create listen socket after ResumeAccept.
		if w.listenFD < 0 && !paused {
			fd, err := createListenSocket(w.cfg.Addr)
			if err != nil {
				w.logger.Error("re-create listen socket", "worker", w.id, "err", err)
				w.shutdown()
				return
			}
			w.listenFD = fd
			w.prepareAccept()
			if _, err := w.ring.Submit(); err != nil {
				w.logger.Error("submit after listen re-create", "worker", w.id, "err", err)
			}
		}

		var cqHead, cqTail uint32
		if w.sqpoll {
			// SQPOLL path: kernel thread submits SQEs from the shared ring.
			// We never call Submit() — just clear the pending counter.
			w.ring.ClearPending()

			// Wake SQPOLL thread if it went idle after sqThreadIdle ms.
			if w.ring.SQNeedWakeup() {
				_ = w.ring.WakeupSQPoll()
			}

			// Check CQ ring — if CQEs ready, process without syscall.
			cqHead, cqTail = w.ring.BeginCQ()
			if cqHead == cqTail {
				// No CQEs — wait with adaptive timeout.
				if err := w.ring.WaitCQETimeout(w.adaptiveTimeout()); err != nil {
					w.shutdown()
					return
				}
				cqHead, cqTail = w.ring.BeginCQ()
			}
		} else {
			// Non-SQPOLL path: 4-mode adaptive submit.
			// 1. CQEs already ready + pending SQEs → Submit() (lightweight, no wait)
			// 2. CQEs already ready, nothing pending → skip syscall entirely
			// 3a. No CQEs + SENDs pending → SubmitAndWait() (no ext_arg)
			// 3b. No CQEs + no SENDs → SubmitAndWaitTimeout() (ext_arg + timeout)
			//
			// Modes 1 and 3a avoid ext_arg overhead: the kernel skips hrtimer
			// setup/teardown and sigset parsing, saving ~200-500ns per call.
			// Mode 3a is safe because SEND SQEs always produce CQEs (no
			// CQE_SKIP_SUCCESS), guaranteeing SubmitAndWait returns promptly.
			// Mode 3b uses a timeout because the only pending SQEs may have
			// CQE_SKIP_SUCCESS (e.g., CLOSE), and external events (recv, accept)
			// need a timeout for graceful shutdown via ctx.Err() checks.
			hasPending := w.ring.Pending() > 0
			cqHead, cqTail = w.ring.BeginCQ()
			if cqHead != cqTail && hasPending {
				// Mode 1: CQEs ready + SQEs pending. SubmitAndWait combines
				// submission + CQE retrieval into one syscall (saves ~300ns vs
				// separate Submit + next-iteration wait).
				if err := w.ring.SubmitAndWait(); err != nil {
					w.shutdown()
					return
				}
				cqHead, cqTail = w.ring.BeginCQ()
			} else if cqHead != cqTail { //nolint:revive // intentional no-op: CQEs ready, no pending SQEs, no syscall needed
			} else if hasPending && w.sendsPending {
				// Mode 3a: SEND SQEs pending — guaranteed CQE on completion.
				// SubmitAndWait avoids ext_arg overhead (no hrtimer, no sigset).
				if err := w.ring.SubmitAndWait(); err != nil {
					w.shutdown()
					return
				}
				cqHead, cqTail = w.ring.BeginCQ()
			} else if hasPending || cqHead == cqTail {
				// Mode 3b: no guaranteed CQEs — wait with adaptive timeout for
				// shutdown checks and CQE_SKIP_SUCCESS operations.
				if err := w.ring.SubmitAndWaitTimeout(w.adaptiveTimeout()); err != nil {
					w.shutdown()
					return
				}
				cqHead, cqTail = w.ring.BeginCQ()
			}
		}
		w.sendsPending = false

		if cqHead != cqTail {
			w.emptyIters = 0 // Reset adaptive timeout on activity.
			// Refresh cached timestamp every 64 iterations to amortize
			// time.Now() vDSO cost (~50ns on ARM64). Timeout detection
			// uses multi-second windows so ~1ms resolution is sufficient.
			if w.tickCounter&0x3F == 0 {
				w.cachedNow = time.Now().UnixNano()
			}
			now := w.cachedNow
			for cqHead != cqTail {
				entry := w.ring.cqeAt(cqHead)
				// Inlined CQE dispatch — eliminates processCQE method call
				// and avoids passing context.Context on every CQE (only
				// udAccept needs it). Decode op+fd once per CQE; the
				// previous form re-decoded fd from entry.UserData inside
				// each case. Hot ops (recv/send) are checked first.
				ud := entry.UserData
				fd := int(ud & fdMask)
				switch ud & udMask {
				case udRecv:
					// Generation gate (review 2.6): drop a stale CQE from a
					// prior fd occupant (recycling its provided buffer)
					// before routing to the handler.
					if !w.staleConnCQE(entry, fd, ud) {
						w.handleRecv(entry, fd, now)
					}
				case udSend:
					if !w.staleConnCQE(entry, fd, ud) {
						w.handleSend(entry, fd, now)
					}
				case udAccept:
					w.handleAccept(ctx, entry, fd, now)
				case udClose:
					if !w.staleConnCQE(entry, fd, ud) {
						w.handleClose(fd)
					}
				case udH2Wakeup:
					w.handleH2Wakeup()
				case udHeaderTimer:
					// MUST be in the inlined hot path — processCQE
					// (which has the same case) is only called from
					// the cancel path. Without this case, slowloris-
					// defence timer CQEs are silently dropped.
					if !w.staleConnCQE(entry, fd, ud) {
						w.handleHeaderTimer(fd)
					}
				case udDriverRecv:
					w.handleDriverRecv(entry, fd)
				case udDriverSend:
					w.handleDriverSend(entry, fd)
				case udDriverClose:
					w.handleDriverClose(fd)
				}
				cqHead++
			}
		}
		w.ring.EndCQ(cqHead)

		// SENDs queued during CQE processing remain in the SQ ring and are
		// submitted at the top of the next iteration. For non-SQPOLL, the
		// adaptive submit combines them into a single submit+wait syscall.
		// For SQPOLL, the kernel thread picks them up from the shared ring
		// and ClearPending is called at the top of the SQPOLL path.

		// Flush batched request count to the shared atomic counter. This
		// replaces per-request atomic.Add with one atomic per CQE batch,
		// eliminating cache-line bouncing under multi-worker contention.
		if w.reqBatch > 0 {
			w.reqCount.Add(w.reqBatch)
			w.reqBatch = 0
		}

		// Single atomic publish for all batched buffer returns (P0).
		if w.hasBufReturns {
			w.bufRing.PublishBuffers()
			w.hasBufReturns = false
		}

		// Drain H2 async write queues FIRST. Handler goroutines enqueue
		// response frame bytes; draining them before the dirty list ensures
		// SEND SQEs are queued as early as possible after CQE processing,
		// reducing pipeline stalls for H2 multiplexed streams.
		for _, fd := range w.h2Conns {
			cs := w.conns[fd]
			if cs != nil && cs.h2State != nil && cs.h2State.WriteQueuePending() {
				cs.h2State.DrainWriteQueue(cs.writeFn)
				if w.flushSend(cs) {
					w.markDirty(cs)
				}
			}
		}

		// Advance the monotonic iteration counter and release any
		// connStates whose deferred-release window has elapsed (see
		// queuePendingRelease docstring).
		w.iterCount++
		if len(w.pendingRelease) > 0 {
			w.drainPendingRelease()
		}

		// Drain detached goroutine writes. Goroutines append to the queue
		// instead of calling markDirty directly (dirtyHead is worker-local).
		w.drainDetachQueue()

		// Apply driver-side actions (RegisterConn / UnregisterConn / Write)
		// on the worker thread so SQE submission honors single-issuer.
		if w.hasDriverConns.Load() || w.driverActionPending.Load() != 0 {
			w.drainDriverActions()
		}

		// Retry pending sends and dropped recv arms on dirty connections
		// (SQ ring was full earlier). Typically empty under normal load.
		for cs := w.dirtyHead; cs != nil; {
			next := cs.dirtyNext
			if !cs.sending {
				if mu := cs.detachMu; mu != nil {
					mu.Lock()
				}
				sqFull := w.flushSend(cs)
				if cs.needsRecv && !cs.recvPaused {
					// Arm via pickRecvTarget so a deferred BODY recv re-arms
					// into the H1 bodyBuf with cs.recvIntoBody set, instead of
					// blindly re-arming into cs.buf. Re-arming into cs.buf
					// while recvIntoBody stayed true made handleRecv take the
					// body branch on a cs.buf-sized CQE and corrupt the body
					// (v1.5.0 review 2.4). pickRecvTarget MUTATES recvIntoBody,
					// so call it exactly once per arm; it is idempotent across
					// SQ-full retries because NextRecvBuf returns the same tail
					// while bodyBuf state is unchanged.
					if w.prepareRecv(cs, w.pickRecvTarget(cs)) {
						cs.needsRecv = false
					}
				}
				canRemove := !sqFull && len(cs.sendBuf) == 0 && len(cs.writeBuf) == 0 && (!cs.needsRecv || cs.recvPaused)
				if mu := cs.detachMu; mu != nil {
					mu.Unlock()
				}
				if canRemove {
					w.removeDirty(cs)
				}
			}
			cs = next
		}

		// SENDs queued during CQE processing are submitted at the top of
		// the next iteration: Mode 1 SubmitAndWait combines submit + CQE
		// retrieval in one syscall. No separate submit needed here.

		// Increment empty iterations counter when no CQEs were found.
		// (Reset to 0 above when CQEs are present.)
		if w.emptyIters < 200 {
			w.emptyIters++
		}

		// Check connection timeouts. Default cadence is every 1024
		// iterations (~100ms under load); the gate tightens to every 32
		// iterations (~50ms idle wall time) in two cases:
		//   - detached conns exist with idle deadlines → WS idle-close
		//     must fire within its configured budget.
		//   - ReadHeaderTimeout > 0 → slowloris defence. The per-conn
		//     IORING_OP_TIMEOUT is the primary mechanism but it can
		//     silently drop arms under SQ-ring pressure (armHeaderTimer
		//     returns without arming if GetSQE+Submit retry still fails).
		//     The sweep is the belt-and-braces fallback; under heavy
		//     legit traffic (observability+static_swagger_proxy) the
		//     0x3FF gate is too coarse and the walker times out before
		//     the sweep notices. Tightening to 0x1F caps the worst-case
		//     close latency on a dropped arm to ~50ms.
		w.tickCounter++
		gate := uint32(0x3FF)
		if w.detachedCount > 0 || w.cfg.ReadHeaderTimeout > 0 {
			gate = 0x1F
		}
		if w.tickCounter&gate == 0 {
			w.checkTimeouts()
		}

		// DRAINING → SUSPENDED: no listen socket, no connections, CQEs processed.
		// Checked after CQE processing so accept CQEs for connections that
		// completed before the listen socket close are served, not leaked.
		// Combined paused: engine-wide OR per-worker (dynamic scaler).
		//
		// hasDriverConns gate: connCount only counts HTTP conns. An
		// EventLoopProvider driver may still have live conns in
		// w.driverConns even when connCount==0; suspending the worker would
		// park its event loop and starve those driver conns of CQE
		// servicing. Stay active while any driver conn is registered (v1.5.0
		// review 2.10).
		if w.listenFD < 0 && w.connCount == 0 && !w.hasDriverConns.Load() && (w.acceptPaused.Load() || w.inactive.Load()) {
			w.wakeMu.Lock()
			if !w.acceptPaused.Load() && !w.inactive.Load() {
				w.wakeMu.Unlock()
				continue
			}
			w.suspended.Store(true)
			wake := w.wake
			w.wakeMu.Unlock()

			select {
			case <-wake:
			case <-ctx.Done():
				w.shutdown()
				return
			}
			continue
		}
	}
}

// staleConnCQE reports whether c is a late/in-flight CQE from a PRIOR
// occupant of fd (review 2.6). A CQE is stale when the slot is empty
// (cs == nil) or the conn currently at fd has a different generation than
// the one stamped into the CQE's user_data at SQE-submission time. Only
// conn-bound ops (udRecv/udSend/udClose/udHeaderTimer) carry a generation;
// callers must not invoke this for non-conn-bound ops.
//
// As the single chokepoint every conn-bound CQE passes through, this is
// also where kernelInflight accounting happens (v1.4.15/7beebb9 corruption fix): a TERMINAL
// recv/send CQE — one without CQE_F_MORE; multishot recv and SEND_ZC
// post intermediate F_MORE CQEs, and a cancel op's own CQE carries the
// udProvide tag so it never reaches here — decrements the owning conn's
// in-flight op count. For a live conn that is cs directly; for a stale
// CQE the closed conn is resolved through Worker.closedOps, keeping the
// bookkeeping on the OLD connState captured at arm time rather than
// whatever currently occupies w.conns[fd]. The decrement is what lets
// drainPendingRelease return the closed connState to the pool.
//
// CRITICAL: a stale recv CQE may still own a provided ring buffer. Dropping
// it without returning the buffer leaks a ring entry → ENOBUFS → the
// CQE-storm regression (celeris#322). When stale, we therefore recycle the
// buffer exactly as handleRecv's unknown/closing branch does (PushBuffer +
// hasBufReturns, published once per loop pass). The recycle is keyed on the
// CQE flags, not the conn, so it is correct even though the conn is gone.
func (w *Worker) staleConnCQE(c *completionEntry, fd int, ud uint64) bool {
	var cs *connState
	if fd >= 0 && fd < len(w.conns) {
		cs = w.conns[fd]
	}
	op := ud & udMask
	terminalOp := (op == udRecv || op == udSend) && !cqeHasMore(c.Flags)
	if cs != nil && cs.generation == decodeGen(ud) {
		if terminalOp {
			if op == udRecv {
				cs.recvArmed = false
			}
			// KNOWN RESIDUAL — gen-collision misroute. A closed
			// predecessor's terminal CQE that arrives after this fd was
			// re-occupied by a conn with a COLLIDING generation (gens are
			// per-connState-object; 1/65536 per reuse with the 16-bit
			// tag) is indistinguishable from the live conn's own CQE and
			// lands here. The clamp below only stops an already-drained
			// counter going negative; a misdecrement from >=1 DOES
			// under-count the live conn (and may clear recvArmed above
			// with its recv still kernel-armed), so its own close can
			// skip the cancel and release early — the UAF class this
			// accounting exists to prevent. Reachability is narrow:
			// non-SQPOLL task-work ordering posts the close-path
			// -ECANCELED during the submit syscall, before the fd can be
			// re-accepted; the window needs a dropped cancel SQE (full SQ
			// ring) or SQPOLL's decoupled completion ordering ON TOP of
			// the gen collision. When it fires, the closed conn's
			// closedOps entry is left orphaned and the 5 s backstop WARN
			// in drainPendingRelease is the production signal.
			if cs.kernelInflight > 0 {
				cs.kernelInflight--
			}
		}
		return false
	}
	if terminalOp {
		w.noteStaleTerminalOp(ud)
	}
	if cqeHasBuffer(c.Flags) && w.bufRing != nil {
		w.bufRing.PushBuffer(cqeBufferID(c.Flags))
		w.hasBufReturns = true
	}
	return true
}

// noteStaleTerminalOp attributes a terminal recv/send CQE that arrived
// after its conn closed to the closed connState(s) registered under the
// CQE's (fd, generation) identity, releasing them for drainPendingRelease
// once the kernel owes them nothing. No-op when the identity is unknown
// (conn closed with zero in-flight ops, or already backstop-released).
func (w *Worker) noteStaleTerminalOp(ud uint64) {
	if len(w.closedOps) == 0 {
		return
	}
	key := connOpKey(ud)
	e := w.closedOps[key]
	if e == nil {
		return
	}
	e.inflight--
	if e.inflight > 0 {
		return
	}
	for _, cs := range e.conns {
		cs.kernelInflight = 0
	}
	delete(w.closedOps, key)
}

func (w *Worker) processCQE(ctx context.Context, c *completionEntry, now int64) {
	ud := c.UserData
	op := decodeOp(ud)
	fd := decodeFD(ud)

	switch op {
	case udRecv:
		if w.staleConnCQE(c, fd, ud) {
			return
		}
		w.handleRecv(c, fd, now)
	case udSend:
		if w.staleConnCQE(c, fd, ud) {
			return
		}
		w.handleSend(c, fd, now)
	case udClose:
		if w.staleConnCQE(c, fd, ud) {
			return
		}
		w.handleClose(fd)
	case udAccept:
		w.handleAccept(ctx, c, fd, now)
	case udH2Wakeup:
		w.handleH2Wakeup()
	case udHeaderTimer:
		if w.staleConnCQE(c, fd, ud) {
			return
		}
		w.handleHeaderTimer(fd)
	case udDriverRecv:
		w.handleDriverRecv(c, fd)
	case udDriverSend:
		w.handleDriverSend(c, fd)
	case udDriverClose:
		w.handleDriverClose(fd)
	}
}

// handleHeaderTimer processes an IORING_OP_TIMEOUT CQE submitted by
// armHeaderTimer. The kernel fires it at the absolute deadline regardless
// of CQE traffic on the worker, giving slowloris defence parity with
// std's SetReadDeadline-based enforcement. The CQE is always processed —
// no res check — because:
//   - res == -ETIME on timer expiry (normal)
//   - res == -ECANCELED if cancelled (we don't cancel, only let it fire)
//   - res != 0 on any other error (we still need to clear headerTimerArmed)
//
// Race-free against ProcessH1's ClearHeaderDeadline: HeaderDeadlineNs is
// atomic; if cleared between SQE submission and CQE arrival, the close
// gate below skips the close. If re-armed in the same window (next
// keep-alive request), the ArmHeaderDeadline callback submits a fresh
// timer SQE — so we don't need to re-arm here.
// handleHeaderTimer ignores the caller's `now` because cachedNow is
// refreshed only every 64 iterations (60+ms stale under bursty load)
// and a stale now < dl comparison would spuriously re-arm a fresh 10s
// timer, effectively doubling the slowloris window. One vDSO call per
// timer CQE — rare in the hot path — is the right trade-off.
func (w *Worker) handleHeaderTimer(fd int) {
	if fd < 0 || fd > w.maxFD {
		return
	}
	cs := w.conns[fd]
	if cs == nil {
		return
	}
	cs.headerTimerArmed = false
	if cs.closing || cs.h1State == nil {
		return
	}
	dl := cs.h1State.HeaderDeadlineNs.Load()
	if dl == 0 {
		// Headers completed before the timer fired; no-op. The next
		// ArmHeaderDeadline (keep-alive next request) will submit a
		// fresh timer SQE.
		return
	}
	now := time.Now().UnixNano()
	if now < dl {
		// True early fire (kernel clock drift, very rare). Re-arm
		// a fresh timer for the actual remaining time.
		w.armHeaderTimer(cs)
		return
	}
	// Deadline exceeded — slowloris defence fires. Mirror std/net.http's
	// approach: plain unix.Close, no shutdown, no linger, no response.
	// net/http on hdrDeadline → isCommonNetReadError → "don't reply",
	// then c.close() → c.rwc.Close() (plain TCP close). Std walker shows
	// 0 hangs on the exact same probatorium test. We previously tried:
	//   - SHUT_RDWR + close + LINGER{1,0} (RST) → ~17-21 hangs per cell
	//   - 408 response + SHUT_WR + drain + close → ~17-25 hangs per cell
	// Both elaborate paths had the same ~5-7% walker-hang rate.
	// Suspect: SHUT_*/drain syscalls + LINGER each create their own
	// timing race vs. the walker's drip Write; plain close is simpler
	// and matches the observed-working std behavior exactly.
	w.closeConn(fd)
}

// armHeaderTimer submits an IORING_OP_TIMEOUT SQE that fires at
// cs.h1State.HeaderDeadlineNs. Called from initProtocol on conn-accept
// and from the OnHeaderDeadlineArmed callback when ProcessH1 re-arms
// for a keep-alive next request.
//
// Idempotent: if a timer is already in flight (cs.headerTimerArmed),
// the new arm is a no-op — the in-flight timer's CQE will check the
// current HeaderDeadlineNs and either close or re-submit as needed.
// Without this guard a fast-cycling keep-alive client could queue many
// in-flight timer SQEs for the same conn, exhausting the SQ ring.
func (w *Worker) armHeaderTimer(cs *connState) {
	if cs.h1State == nil || cs.headerTimerArmed {
		return
	}
	dl := cs.h1State.HeaderDeadlineNs.Load()
	if dl == 0 {
		return
	}
	now := time.Now().UnixNano()
	remaining := dl - now
	if remaining <= 0 {
		// Deadline already past — close immediately rather than queue
		// a zero-duration timer (kernel would fire it instantly anyway).
		w.closeConn(cs.fd)
		return
	}
	cs.headerTimerSpec.Sec = remaining / int64(time.Second)
	cs.headerTimerSpec.Nsec = remaining % int64(time.Second)
	sqe := w.ring.GetSQE()
	if sqe == nil {
		// Ring full — submit pending SQEs to drain, then retry.
		// Without this, high-CQE-traffic cells (observability,
		// static_swagger_proxy with concurrent Markov + adversarial
		// walkers) silently dropped a fraction of timer arms; those
		// conns then relied on the sweep, hitting the cadence floor.
		// Surfaced by nightly 26397557463 where slow refapps still
		// showed 62% slowloris hang rate post-forceRSTClose fix.
		if _, err := w.ring.Submit(); err != nil {
			return
		}
		sqe = w.ring.GetSQE()
		if sqe == nil {
			return
		}
	}
	prepTimeout(sqe, unsafe.Pointer(&cs.headerTimerSpec), 0, 0)
	setSQEUserData(sqe, encodeUserDataGen(udHeaderTimer, cs.fd, cs.generation))
	cs.headerTimerArmed = true
}

// adaptiveTimeout returns a wait timeout that scales with idle duration.
// Under load (CQEs arriving), returns 1ms for minimal latency. During idle
// periods, backs off to reduce syscall overhead (up to 100ms). When the
// listen socket is closed (draining), uses 1s. When H2 connections exist
// without eventfd, uses 100us for write queue polling. When detached
// conns exist with idle deadlines or ReadHeaderTimeout is configured, the
// cap drops to 25ms so checkTimeouts can fire detached-idle deadlines
// before expiry AND back up the per-conn IORING_OP_TIMEOUT for slowloris
// defence when the kernel timer SQE failed to arm under SQ-ring pressure.
func (w *Worker) adaptiveTimeout() time.Duration {
	if w.listenFD < 0 {
		return 1 * time.Second
	}
	if len(w.h2Conns) > 0 && w.h2EventFD < 0 {
		return 100 * time.Microsecond
	}
	if w.dirtyHead != nil {
		return 0
	}
	maxWait := 100 * time.Millisecond
	if w.detachedCount > 0 {
		maxWait = 50 * time.Millisecond
	}
	if w.cfg.ReadHeaderTimeout > 0 && maxWait > 25*time.Millisecond {
		maxWait = 25 * time.Millisecond
	}
	switch {
	case w.emptyIters <= 10:
		return 1 * time.Millisecond
	case w.emptyIters <= 100:
		d := 5 * time.Millisecond
		if d > maxWait {
			d = maxWait
		}
		return d
	default:
		return maxWait
	}
}

func (w *Worker) handleAccept(ctx context.Context, c *completionEntry, _ int, now int64) {
	if c.Res < 0 {
		// EINVAL with fixed files: ACCEPT_DIRECT not supported on this kernel.
		// Disable fixed files and retry with regular multishot accept.
		if c.Res == -22 && w.fixedFiles {
			w.logger.Warn("ACCEPT_DIRECT failed (EINVAL), disabling fixed files",
				"worker", w.id)
			w.fixedFiles = false
			if w.listenFD >= 0 {
				sqe := w.ring.GetSQE()
				if sqe != nil {
					prepMultishotAccept(sqe, w.listenFD)
					setSQEUserData(sqe, encodeUserData(udAccept, w.listenFD))
				}
			}
			return
		}
		w.errCount.Add(1)
		// Re-arm accept whenever the kernel is not going to deliver more
		// CQEs from the current SQE. In single-shot mode !cqeHasMore is
		// always true (each accept produces exactly one CQE), so this
		// preserves the old single-shot re-arm. In multishot mode an error
		// CQE clears F_MORE to signal the multishot was terminated — and
		// without this re-arm the worker would permanently stop accepting
		// (the old `!SupportsMultishotAccept()` guard never re-armed in
		// multishot mode, silently killing accept on a transient ENOMEM /
		// EMFILE). See celeris v1.5.0 review 2.1.
		if w.listenFD >= 0 && !cqeHasMore(c.Flags) {
			w.prepareAccept()
		}
		return
	}

	newFD := int(c.Res)
	w.onAcceptedFD(ctx, newFD, now, w.fixedFiles)

	// Re-arm accept when CQE_F_MORE is clear. In single-shot mode this
	// fires on every CQE. In multishot mode kernel sets F_MORE=1 to say
	// "more CQEs coming from this SQE"; if F_MORE is clear the multishot
	// was terminated (kernel backpressure or error) and without re-arming
	// the worker permanently stops accepting on its listen socket.
	// Observed on aarch64 kernel 6.6.10: multishot accept silently
	// terminated under HTTP/1 Connection:close churn pressure, killing
	// accept throughput on that worker until the engine restarted.
	if !cqeHasMore(c.Flags) && w.listenFD >= 0 {
		w.prepareAccept()
	}
}

// onAcceptedFD sets up state for a newly accepted fd — builds connState,
// registers it with the worker, and arms the first recv.
func (w *Worker) onAcceptedFD(ctx context.Context, newFD int, now int64, isFixedFile bool) {
	// Bounds check: reject FDs outside the flat conn array.
	if newFD < 0 || newFD >= len(w.conns) {
		if !isFixedFile {
			_ = unix.Close(newFD)
		}
		w.errCount.Add(1)
		return
	}

	if !isFixedFile {
		_ = sockopts.ApplyFD(newFD, w.sockOpts)
	}
	// For fixed files, socket options were applied by the kernel at accept time
	// via inherited options. TCP_NODELAY etc. must be set post-accept for
	// non-inherited options — but with fixed files (ACCEPT_DIRECT), the fd field
	// is actually a fixed file index and we can't call setsockopt on it directly.

	bufSize := w.resolved.BufferSize
	if w.bufRing != nil {
		bufSize = 0
	}
	connCtx := ctxkit.WithWorkerID(ctx, w.id)
	cs := acquireConnState(connCtx, newFD, bufSize, w.async)
	cs.fixedFile = isFixedFile

	if !isFixedFile {
		if sa, err := unix.Getpeername(newFD); err == nil {
			cs.remoteAddr = sockaddrString(sa)
		}
	}

	w.conns[newFD] = cs
	w.connCount++
	w.addLiveConn(cs)
	if newFD > w.maxFD {
		w.maxFD = newFD
	}
	cs.writeFn = w.makeWriteFn(cs)
	w.activeConns.Add(1)

	if w.cfg.OnConnect != nil {
		w.cfg.OnConnect(cs.remoteAddr)
	}

	cs.lastActivity = now

	// H2C + EnableH2Upgrade is semantically "H2-first but accept H1→H2
	// upgrades too", so route it through the recv-time detection path (like
	// Auto) rather than locking cs.protocol=H2C on accept. Without
	// this, the first HTTP/1.1 upgrade request was fed to ProcessH2 and
	// the PRI-preface check silently failed, leaving the client with 27
	// bytes of server SETTINGS frame and no 101 Switching Protocols
	// response.
	if w.cfg.Protocol != engine.Auto &&
		(w.cfg.Protocol != engine.H2C || !w.cfg.EnableH2Upgrade) {
		cs.protocol.Store(int32(w.cfg.Protocol))
		cs.detected = true
		w.initProtocol(cs)
	}
	if !w.prepareRecv(cs, cs.buf) {
		cs.needsRecv = true
		w.markDirty(cs)
	}
}

func (w *Worker) hijackConn(fd int) (net.Conn, error) {
	cs := w.conns[fd]
	if cs == nil {
		return nil, errors.New("celeris: connection not found")
	}
	if cs.fixedFile {
		return nil, errors.New("celeris: cannot hijack fixed file connection")
	}
	if cs.sending || len(cs.sendBuf) > 0 || len(cs.writeBuf) > 0 {
		return nil, errors.New("celeris: cannot hijack with pending sends")
	}
	w.removeLiveConn(cs)
	w.conns[fd] = nil
	w.connCount--
	w.activeConns.Add(-1)
	// Cancel-then-release discipline, hijack variant: a single-shot
	// recv SQE is virtually always still armed on cs.buf here. The fd
	// lives on under the caller's net.Conn, so an uncancelled recv would
	// not only pin cs.buf past release (the #256-class UAF) but also
	// STEAL the first bytes the hijacker tries to read. Cancel it by its
	// generation-tagged user_data and defer the pool release until the
	// terminal CQE arrives, exactly like finishClose.
	w.cancelConnOps(fd, cs)
	w.noteClosedInflight(cs)
	w.queuePendingRelease(cs)
	f := os.NewFile(uintptr(fd), "tcp")
	c, err := net.FileConn(f)
	_ = f.Close()
	return c, err
}

func (w *Worker) initProtocol(cs *connState) {
	switch engine.Protocol(cs.protocol.Load()) {
	case engine.HTTP1:
		cs.h1State = conn.NewH1State()
		cs.h1State.RemoteAddr = cs.remoteAddr
		cs.h1State.MaxRequestBodySize = w.cfg.MaxRequestBodySize
		cs.h1State.OnExpectContinue = w.cfg.OnExpectContinue
		cs.h1State.EnableH2Upgrade = w.cfg.EnableH2Upgrade
		cs.h1State.WorkerID = int32(w.id)
		cs.h1State.WorkerIDSet = true
		// Slowloris defence: kernel-enforced per-conn header deadline.
		//
		// Sync mode (!w.async): ProcessH1 runs inline on the worker thread,
		// so OnHeaderDeadlineArmed → armHeaderTimer is always a worker-
		// thread call. Safe.
		//
		// Async mode (w.async): ProcessH1 runs on the per-conn dispatch
		// goroutine. The keep-alive re-arm path (ProcessH1 sees
		// HeaderDeadlineNs == 0 and calls ArmHeaderDeadline at line 360
		// of internal/conn/h1.go) would fire OnHeaderDeadlineArmed from
		// the goroutine, which would call w.ring.GetSQE — a violation of
		// IORING_SETUP_SINGLE_ISSUER that can silently drop SQEs or
		// corrupt the SQ ring. Leave OnHeaderDeadlineArmed nil in async
		// mode; the initial accept-time arm fires via the direct
		// armHeaderTimer call below (worker thread, safe), the recv-
		// path retry (handleRecv) covers SQ-pressure drops, and keep-
		// alive re-arms fall back to checkTimeouts which now runs every
		// 32 iters × 25ms = 800ms worst case.
		cs.h1State.ReadHeaderTimeoutNs = int64(w.cfg.ReadHeaderTimeout)
		if !w.async {
			cs.h1State.OnHeaderDeadlineArmed = func() { w.armHeaderTimer(cs) }
		}
		cs.h1State.ArmHeaderDeadline()
		if w.async {
			// Direct worker-thread arm for the initial deadline; the
			// nil OnHeaderDeadlineArmed above means ArmHeaderDeadline
			// only stamped HeaderDeadlineNs, not the SQE.
			w.armHeaderTimer(cs)
			// Per-handler async (celeris #300): wire the route resolver so
			// ProcessH1, when run inline on the worker (InlineMode), can
			// detect an async route and bail to the dispatch goroutine.
			// Only meaningful in async mode; gated on HasAsyncRoutes so
			// pure-sync servers running with Config.AsyncHandlers=true
			// don't pay the per-recv resolver call.
			if r, ok := w.handler.(stream.AsyncRouteResolver); ok && r.HasAsyncRoutes() {
				cs.h1State.RouteAsync = r.RouteAsync
			}
		}
		if !w.cfg.EnableH2Upgrade {
			cs.h1State.DisableH2CDetect()
		}
		// Wire the scatter-gather body writer so the H1 response adapter
		// can hand large bodies straight to the WRITEV path without the
		// intermediate respBuf → cs.writeBuf memcpy. writeBodyFn stores
		// the body slice on the connState; flushSend emits an iovec SQE.
		// Only enabled on the synchronous inline-handler path: async
		// mode handlers run on goroutines and need detachMu-guarded
		// access to cs.bodyBuf, which the current writer does not
		// provide. Async mode falls back to the copy path.
		if !w.async {
			cs.h1State.SetWriteBodyFn(w.makeWriteBodyFn(cs))
		}
		cs.h1State.OnDetach = func() {
			// Async mode may have already allocated detachMu in
			// acquireConnState; reuse it so the async goroutine and the
			// middleware goroutine share one mutex. Otherwise create
			// a fresh mutex for the WS/SSE detach flow.
			mu := cs.detachMu
			if mu == nil {
				mu = &sync.Mutex{}
				cs.detachMu = mu
			}
			// Sync mode: OnDetach runs on the worker thread, so the
			// worker-owned bookkeeping is safe to mutate here.
			// Async mode: OnDetach runs on the per-conn dispatch
			// goroutine — w.detachedCount races with the worker
			// thread's adaptiveTimeout read and any ring SQE
			// submission violates SINGLE_ISSUER. Defer both to
			// drainDetachQueue via asyncDetachPending so the worker
			// owns the mutation. The first guarded() write below
			// enqueues+signals, so drainDetachQueue fires promptly.
			if !w.async {
				w.detachedCount++
			} else {
				cs.asyncDetachPending = true
			}
			orig := cs.writeFn
			wakeupFD := w.h2EventFD
			guarded := func(data []byte) {
				mu.Lock()
				if cs.detachClosed {
					mu.Unlock()
					return
				}
				orig(data)
				mu.Unlock()
				// Signal the event loop to flush. Do NOT call markDirty
				// from this goroutine — dirtyHead is worker-local.
				w.detachQMu.Lock()
				w.detachQueue = append(w.detachQueue, cs)
				w.detachQPending.Store(1)
				w.detachQMu.Unlock()
				if wakeupFD >= 0 {
					var val [8]byte
					val[0] = 1
					_, _ = unix.Write(wakeupFD, val[:])
				}
			}
			cs.writeFn = guarded
			// Also update the response adapter so StreamWriter writes
			// go through the guarded path (not the stale pre-Detach writeFn).
			cs.h1State.UpdateWriteFn(guarded)
			// Expose raw write for WebSocket (bypasses chunked encoding).
			cs.h1State.RawWriteFn = guarded
			// Install pause/resume callbacks for WebSocket backpressure.
			// They set a desired state and wake the worker; the actual
			// recv cancel / re-arm is performed in drainDetachQueue
			// (worker thread).
			cs.h1State.PauseRecv = func() {
				if cs.recvPauseDesired.Swap(true) {
					return
				}
				w.detachQMu.Lock()
				w.detachQueue = append(w.detachQueue, cs)
				w.detachQPending.Store(1)
				w.detachQMu.Unlock()
				if wakeupFD >= 0 {
					var val [8]byte
					val[0] = 1
					_, _ = unix.Write(wakeupFD, val[:])
				}
			}
			cs.h1State.ResumeRecv = func() {
				if !cs.recvPauseDesired.Swap(false) {
					return
				}
				w.detachQMu.Lock()
				w.detachQueue = append(w.detachQueue, cs)
				w.detachQPending.Store(1)
				w.detachQMu.Unlock()
				if wakeupFD >= 0 {
					var val [8]byte
					val[0] = 1
					_, _ = unix.Write(wakeupFD, val[:])
				}
			}
			// Ensure eventfd poll is armed so the worker wakes up.
			// Sync mode runs OnDetach on the worker thread, so this
			// SQE submission is safe. Async mode defers to
			// drainDetachQueue (worker-thread) via asyncDetachPending
			// — see the SINGLE_ISSUER note on the asyncDetachPending
			// field.
			if !w.async && !w.h2PollArmed && w.h2EventFD >= 0 {
				w.prepareH2Poll()
				w.h2PollArmed = true
			}
			// Async mode (HTTP1): the dispatch goroutine took detachMu
			// around ProcessH1 so writeBuf access serialises with the
			// worker's flushSend. Now that we've installed `guarded`
			// (which re-acquires detachMu on every call), keeping the
			// lock held would deadlock the very next write — including
			// the middleware-emitted 101 / SSE headers that immediately
			// follow Detach. Release the lock here and have the
			// dispatch goroutine observe asyncDetachUnlocked to skip
			// its symmetric Unlock when ProcessH1 returns. This is
			// celeris#273 — pre-fix, /ws and /events would TIMEOUT on
			// iouring + AsyncHandlers because the dispatch goroutine
			// was deadlocked on its own re-entrant Lock attempt.
			//
			// Gate on cs.asyncPromoted: detachMu is Locked around
			// ProcessH1 ONLY by runAsyncHandler (the dispatch goroutine),
			// which runs exclusively for PROMOTED conns. With per-handler
			// async (#300/#302) a not-yet-promoted async-mode conn runs
			// its FIRST request INLINE on the worker thread (tryInline);
			// a SYNC route — the WebSocket /ws upgrade or SSE /events
			// stream, which are not themselves .Async() — does not bail
			// with ErrAsyncDispatch, so its handler runs inline and calls
			// Context.Detach() → OnDetach while detachMu was NEVER Locked.
			// Unlocking here under the old w.async-only guard then faulted
			// with "sync: unlock of unlocked mutex", fatally crashing the
			// process on the first /ws or /events request. The
			// asyncPromoted gate restricts the Unlock to the dispatch-
			// goroutine path that actually holds the lock. See celeris#309.
			if w.async && cs.asyncPromoted.Load() && cs.detachMu != nil && !cs.asyncDetachUnlocked {
				cs.asyncDetachUnlocked = true
				cs.detachMu.Unlock()
			}
			// Async mode: enqueue cs so drainDetachQueue picks up the
			// deferred bookkeeping (asyncDetachPending). The first
			// guarded() write will also enqueue+signal, but the
			// explicit signal here covers the rare case where the
			// middleware returns without writing (e.g. a 4xx Detach
			// rejection path). Idempotent — drainDetachQueue
			// clears asyncDetachPending after running it.
			if w.async {
				w.detachQMu.Lock()
				w.detachQueue = append(w.detachQueue, cs)
				w.detachQPending.Store(1)
				w.detachQMu.Unlock()
				if wakeupFD >= 0 {
					var val [8]byte
					val[0] = 1
					_, _ = unix.Write(wakeupFD, val[:])
				}
			}
			// Publish barrier: Store(true) LAST so a worker that observes
			// Detached.Load()==true is guaranteed (atomic happens-before) to
			// also see every detach side effect set above — detachMu install,
			// guarded writeFn/RawWriteFn, pause/resume callbacks, async
			// bookkeeping. In async mode OnDetach runs on the dispatch
			// goroutine while the worker reads Detached on the hot path
			// (writeCap, drainRecv, WS delivery); publishing it first would
			// let the worker act on a half-installed detach. See the data-race
			// fix making Detached atomic.
			cs.h1State.Detached.Store(true)
		}
		if !cs.fixedFile {
			cs.h1State.HijackFn = func() (net.Conn, error) {
				return w.hijackConn(cs.fd)
			}
		}
	case engine.H2C:
		cs.h2State = conn.NewH2State(w.handler, w.h2cfg, cs.writeFn, w.h2EventFD)
		cs.h2State.SetRemoteAddr(cs.remoteAddr)
		// Arm eventfd POLL_ADD on first H2 connection so the ring wakes
		// event-driven when handler goroutines enqueue responses.
		if !w.h2PollArmed && w.h2EventFD >= 0 {
			w.prepareH2Poll()
			w.h2PollArmed = true
		}
		w.h2Conns = append(w.h2Conns, cs.fd)
	}
}

// switchToH2 promotes an H1 connection to H2 mid-stream (RFC 7540 §3.2).
// Called after ProcessH1 returns ErrUpgradeH2C. Drops H1 state, builds H2
// state with the upgrade info pre-applied, and drains any residual bytes
// (which may contain the H2 client preface + initial SETTINGS) through
// ProcessH2 synchronously.
func (w *Worker) switchToH2(cs *connState) error {
	if err := w.switchToH2Local(cs); err != nil {
		return err
	}
	if !w.h2PollArmed && w.h2EventFD >= 0 {
		w.prepareH2Poll()
		w.h2PollArmed = true
	}
	w.h2Conns = append(w.h2Conns, cs.fd)
	return nil
}

// switchToH2Local does every part of switchToH2 except the worker-owned
// steps (w.h2Conns append, prepareH2Poll) which must run on the worker
// goroutine — that slice and SQE submission are SINGLE_ISSUER. The
// async dispatch goroutine uses this under cs.detachMu and then asks
// the worker to finish via asyncH2Promoted + detachQueue.
func (w *Worker) switchToH2Local(cs *connState) error {
	info := cs.h1State.UpgradeInfo
	h2State, err := conn.NewH2StateFromUpgrade(w.handler, w.h2cfg, cs.writeFn, w.h2EventFD, info)
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
	cs.protocol.Store(int32(engine.H2C))

	var processErr error
	if len(info.Remaining) > 0 {
		processErr = conn.ProcessH2(cs.ctx, info.Remaining, cs.h2State, w.handler, cs.writeFn, w.h2cfg)
	}
	conn.ReleaseUpgradeInfo(info)
	return processErr
}

func (w *Worker) handleRecv(c *completionEntry, fd int, now int64) {
	cs := w.conns[fd]
	if cs == nil || cs.closing {
		// If multishot recv with provided buffers, batch-return the buffer
		// even for unknown/closing connections to prevent buffer leak (P0).
		if cqeHasBuffer(c.Flags) && w.bufRing != nil {
			w.bufRing.PushBuffer(cqeBufferID(c.Flags))
			w.hasBufReturns = true
		}
		return
	}

	if c.Res <= 0 {
		if cqeHasBuffer(c.Flags) && w.bufRing != nil {
			w.bufRing.PushBuffer(cqeBufferID(c.Flags))
			w.hasBufReturns = true
		}
		// Recv was cancelled by drainDetachQueue (WS backpressure pause).
		// Don't close — the connection stays open until ResumeRecv re-arms.
		if c.Res == -int32(unix.ECANCELED) && cs.recvPaused {
			return
		}
		// ENOBUFS (-105): provided buffer ring exhausted. The multishot recv
		// is terminated by the kernel but the connection is healthy. Re-arm
		// multishot recv — buffers will be available after current batch is
		// returned via PublishBuffers().
		if c.Res == -105 && w.bufRing != nil {
			w.bufRing.PublishBuffers()
			if !cs.recvPaused {
				if !w.prepareRecv(cs, cs.buf) {
					cs.needsRecv = true
					w.markDirty(cs)
				}
			}
			return
		}
		// Surface read failure to detached middleware before closing.
		// Check cs.h1State under detachMu — the async dispatch
		// goroutine's switchToH2Local nulls cs.h1State under the same
		// lock, so reading it before acquiring detachMu is a TOCTOU
		// that the race detector flags (#256 regression class).
		if cs.detachMu != nil {
			cs.detachMu.Lock()
			if cs.h1State != nil && cs.h1State.OnError != nil {
				cs.h1State.OnError(errIORingRecv(c.Res))
			}
			cs.detachMu.Unlock()
		}
		w.closeConn(fd)
		return
	}

	cs.lastActivity = now

	// Direct-into-bodyBuf path: the previous recv SQE targeted
	// H1State.bodyBuf (NextRecvBuf). The CQE's Res applies to bodyBuf,
	// NOT cs.buf, so check this FIRST — indexing cs.buf[:c.Res] when
	// c.Res > cap(cs.buf) would panic. Skip ProcessH1, extend the body,
	// and dispatch the handler when the body is full.
	if cs.recvIntoBody && cs.h1State != nil {
		cs.recvIntoBody = false
		complete := cs.h1State.ConsumeBodyRecv(int(c.Res))
		if !complete {
			if !cqeHasMore(c.Flags) && !cs.recvLinked && !cs.recvPaused {
				if !w.prepareRecv(cs, w.pickRecvTarget(cs)) {
					cs.needsRecv = true
					w.markDirty(cs)
				}
			}
			return
		}
		rest, derr := cs.h1State.DispatchBufferedBody(cs.ctx, w.handler, cs.writeFn)
		if errors.Is(derr, conn.ErrUpgradeH2C) {
			if err := w.switchToH2(cs); err != nil {
				w.closeConn(fd)
				return
			}
			if mu := cs.detachMu; mu != nil {
				mu.Lock()
			}
			cs.recvLinked = false
			if w.bufRing == nil {
				if w.flushSendLink(cs) {
					w.markDirty(cs)
				}
			} else {
				if w.flushSend(cs) {
					w.markDirty(cs)
				}
			}
			if mu := cs.detachMu; mu != nil {
				mu.Unlock()
			}
			return
		}
		if derr != nil {
			w.closeConn(fd)
			return
		}
		if len(rest) > 0 {
			if perr := conn.ProcessH1(cs.ctx, rest, cs.h1State, w.handler, cs.writeFn); perr != nil {
				if !errors.Is(perr, conn.ErrHijacked) {
					w.closeConn(fd)
				}
				return
			}
		}
		if mu := cs.detachMu; mu != nil {
			mu.Lock()
		}
		if w.flushSend(cs) {
			w.markDirty(cs)
		}
		if mu := cs.detachMu; mu != nil {
			mu.Unlock()
		}
		if !cqeHasMore(c.Flags) && !cs.recvLinked && !cs.recvPaused {
			if !w.prepareRecv(cs, w.pickRecvTarget(cs)) {
				cs.needsRecv = true
				w.markDirty(cs)
			}
		}
		return
	}

	var data []byte
	var providedBufID uint16
	hasProvidedBuf := false

	if cqeHasBuffer(c.Flags) && w.bufRing != nil {
		// Multishot recv with ring-mapped provided buffers.
		providedBufID = cqeBufferID(c.Flags)
		data = w.bufRing.GetBuffer(providedBufID, int(c.Res))
		hasProvidedBuf = true
	} else {
		// Per-connection buffer (single-shot recv).
		data = cs.buf[:c.Res]
	}

	// Auto protocol detection on first recv (no MSG_PEEK needed).
	if !cs.detected {
		// Accumulate across recvs so a protocol prefix that spans multiple
		// packets (notably the 24-byte H2 client preface) is not lost. The
		// single-shot path re-arms into cs.buf at offset 0 and the bufRing
		// path returns each provided buffer, so the only durable place to
		// hold partial bytes is cs.detectAccum (a Go-heap buffer the kernel
		// never targets). The fast common case — detection resolves on the
		// first recv — pays no copy (v1.5.0 review 2.8).
		detectData := data
		if len(cs.detectAccum) > 0 {
			cs.detectAccum = append(cs.detectAccum, data...)
			detectData = cs.detectAccum
		}
		proto, derr := detect.Detect(detectData)
		if derr != nil {
			// ErrInsufficientData (more bytes needed) AND ErrUnknownProtocol
			// both re-arm recv and wait — matching the pre-2.8 behavior
			// (the slowloris header timer / idle timeout reaps a conn that
			// never produces recognizable bytes). The ONLY change here is
			// that partial bytes are preserved in cs.detectAccum across
			// recvs instead of being overwritten at cs.buf offset 0.
			if derr == detect.ErrInsufficientData && len(cs.detectAccum) == 0 {
				// First partial recv — begin accumulating.
				cs.detectAccum = append(cs.detectAccum, data...)
			}
			if hasProvidedBuf {
				// Provided-buffer bytes are now copied into detectAccum (for
				// the ErrInsufficientData path); return the buffer to the
				// kernel (early — we skip the normal batch publish, P0).
				w.bufRing.ReturnBuffer(providedBufID)
			}
			if !cqeHasMore(c.Flags) {
				if !w.prepareRecv(cs, cs.buf) {
					cs.needsRecv = true
					w.markDirty(cs)
				}
			}
			return
		}
		cs.protocol.Store(int32(proto))
		cs.detected = true
		w.initProtocol(cs)
		// If we accumulated across recvs, hand the FULL accumulated prefix
		// (not just this last recv) to the protocol handler below. Reset the
		// accumulator header; `data`'s own slice header keeps the backing
		// array alive for the rest of this function.
		if len(cs.detectAccum) > 0 {
			data = cs.detectAccum
			cs.detectAccum = cs.detectAccum[:0]
		}
	}

	// Async handler dispatch (Config.AsyncHandlers on HTTP1): hand the
	// received bytes to a per-conn goroutine and return to the CQE drain
	// immediately. The goroutine runs ProcessH1 under cs.detachMu and
	// enqueues on detachQueue so this worker submits SEND SQEs on its
	// own goroutine (SINGLE_ISSUER). Mirrors the epoll W3 shape.
	//
	// Per-handler async (celeris #300): only PROMOTED conns go straight
	// to the dispatch goroutine here. A fresh async-mode HTTP1 conn first
	// tries the inline fast path below (InlineMode); ProcessH1 bails with
	// ErrAsyncDispatch the moment it parses an async-marked route, at
	// which point the conn is promoted and its stashed request handed to
	// the goroutine. This lets sync routes run inline on the worker
	// (no handoff) on a server that mixes sync + async handlers.
	asyncFeed := false
	if w.async && cs.asyncPromoted.Load() && (w.h1Only || engine.Protocol(cs.protocol.Load()) == engine.HTTP1) {
		cs.asyncInMu.Lock()
		// celeris#364: re-check under asyncInMu — the dispatch goroutine clears
		// asyncPromoted (reverting the conn to inline) under this same lock. If
		// it won the race, do NOT feed asyncInBuf (the goroutine is exiting);
		// fall through to inline with the unconsumed `data` instead.
		if cs.asyncPromoted.Load() {
			asyncFeed = true
		} else {
			cs.asyncInMu.Unlock()
		}
	}
	if asyncFeed {
		// Backpressure: drop the conn if the dispatch goroutine is
		// falling behind. Prevents a pipelining client from ballooning
		// asyncInBuf without bound.
		if len(cs.asyncInBuf)+len(data) > maxPendingInputBytes {
			cs.asyncInMu.Unlock()
			if hasProvidedBuf {
				w.bufRing.PushBuffer(providedBufID)
				w.hasBufReturns = true
			}
			w.closeConn(fd)
			return
		}
		// Append directly into asyncInBuf — dispatch goroutine swaps
		// with asyncOutBuf under the same mutex before running
		// ProcessH1, so the provided-buffer slice cannot be overwritten
		// in-flight. Zero allocation on steady state.
		cs.asyncInBuf = append(cs.asyncInBuf, data...)
		starting := !cs.asyncRun
		if starting {
			cs.asyncRun = true
		}
		cs.asyncInMu.Unlock()
		if hasProvidedBuf {
			w.bufRing.PushBuffer(providedBufID)
			w.hasBufReturns = true
		}
		if starting {
			w.asyncWG.Add(1)
			go w.runAsyncHandler(cs)
		} else {
			cs.asyncCond.Signal()
		}
		w.reqBatch++
		// Re-attempt the slowloris-defence timer arm if the initial
		// attempt (from initProtocol on accept) silently dropped under
		// SQ-ring pressure. ArmHeaderDeadline is idempotent on
		// HeaderDeadlineNs, so it won't reset the deadline; but
		// armHeaderTimer's headerTimerArmed gate means we only retry
		// the SQE submission if the prior arm is not in flight. This
		// is the explicit "retry on next recv" path that was missing —
		// without it, a conn whose initial arm failed relies entirely
		// on the sweep, doubling worst-case close latency on slow
		// refapps (observability/static_swagger_proxy).
		if cs.h1State != nil && cs.h1State.HeaderDeadlineNs.Load() > 0 && !cs.headerTimerArmed {
			w.armHeaderTimer(cs)
		}
		if !cqeHasMore(c.Flags) && !cs.recvPaused {
			if !w.prepareRecv(cs, cs.buf) {
				cs.needsRecv = true
				w.markDirty(cs)
			}
		}
		return
	}

	var processErr error
	// Stash the worker's cached "now" on H1State so populateCachedStream
	// can copy it to the stream — HandleStream skips a per-request
	// time.Now() vDSO call.
	if cs.h1State != nil {
		cs.h1State.NowNs = now
	}
	// Per-handler async (celeris #300): on an async-mode HTTP1 conn that
	// hasn't been promoted yet, run ProcessH1 inline in InlineMode so it
	// bails (ErrAsyncDispatch) when it hits an async route. The flag is
	// set only around the ProcessH1 call(s) below.
	tryInline := w.async && !cs.asyncPromoted.Load() && cs.h1State != nil &&
		(w.h1Only || engine.Protocol(cs.protocol.Load()) == engine.HTTP1)
	// h1Only mode (Protocol=HTTP1 + EnableH2Upgrade=false): no atomic
	// Load, no switch dispatch, no upgrade-handling block — ProcessH1
	// cannot return ErrUpgradeH2C because tryUpgradeH2C is gated off
	// in the parser. The compiler can specialize the call site without
	// hitting any of the H2 / upgrade machinery in the binary's hot
	// section.
	if w.h1Only {
		if tryInline {
			cs.h1State.InlineMode = true
		}
		processErr = conn.ProcessH1(cs.ctx, data, cs.h1State, w.handler, cs.writeFn)
		if tryInline {
			cs.h1State.InlineMode = false
		}
	} else {
		switch engine.Protocol(cs.protocol.Load()) {
		case engine.HTTP1:
			if tryInline {
				cs.h1State.InlineMode = true
			}
			processErr = conn.ProcessH1(cs.ctx, data, cs.h1State, w.handler, cs.writeFn)
			if tryInline {
				cs.h1State.InlineMode = false
			}
			if errors.Is(processErr, conn.ErrUpgradeH2C) {
				// H1→H2 upgrade. switchToH2 consumes the upgrade info and
				// re-arms recv so subsequent data is parsed as H2.
				if hasProvidedBuf {
					w.bufRing.PushBuffer(providedBufID)
					w.hasBufReturns = true
				}
				if err := w.switchToH2(cs); err != nil {
					w.closeConn(fd)
					return
				}
				// Flush the buffered 101 Switching Protocols + H2 server preface
				// + stream 1 response bytes. Without this explicit flush the
				// client blocks forever waiting for the 101 (the normal post-
				// process flush path below is bypassed by this early return).
				if mu := cs.detachMu; mu != nil {
					mu.Lock()
				}
				cs.recvLinked = false
				if w.bufRing == nil {
					if w.flushSendLink(cs) {
						w.markDirty(cs)
					}
				} else {
					if w.flushSend(cs) {
						w.markDirty(cs)
					}
				}
				if mu := cs.detachMu; mu != nil {
					mu.Unlock()
				}
				// Re-arm recv to keep reading H2 frames. flushSendLink may have
				// already chained a recv via IOSQE_IO_LINK — skip our standalone
				// re-arm in that case to avoid submitting two recv SQEs on the
				// same fd, which would split incoming H2 frames across CQEs and
				// occasionally lose END_STREAM delivery for later streams
				// (observed as flaky TestH2CUpgradeSubsequentStreams/iouring).
				if !cqeHasMore(c.Flags) && !cs.recvLinked {
					if !w.prepareRecv(cs, cs.buf) {
						cs.needsRecv = true
						w.markDirty(cs)
					}
				}
				return
			}
		case engine.H2C:
			// Async-mode conns: serialize inline ProcessH2 against the
			// runAsyncHandler goroutine that owns cs.writeBuf until its
			// H1→H2 upgrade-flush completes. Without the lock, a new
			// recv arriving while the goroutine is mid-flush runs
			// ProcessH2 → writeFn → cs.writeBuf manipulation concurrent
			// with the goroutine's `cs.writeBuf = cs.writeBuf[:0]`
			// clear — a data race matrixBenchStrict caught on the third
			// run (see issue #256 investigation thread).
			if cs.detachMu != nil {
				cs.detachMu.Lock()
				processErr = conn.ProcessH2(cs.ctx, data, cs.h2State, w.handler, cs.writeFn, w.h2cfg)
				cs.detachMu.Unlock()
			} else {
				processErr = conn.ProcessH2(cs.ctx, data, cs.h2State, w.handler, cs.writeFn, w.h2cfg)
			}
		}
	}

	// Per-handler async (celeris #300): handle the inline → dispatch
	// handoff. ProcessH1 (InlineMode) returned ErrAsyncDispatch the moment
	// it parsed an async-marked route; the request (+ any pipelined bytes)
	// is stashed in the H1 buffer. Promote the conn and hand the stashed
	// bytes to its dispatch goroutine, then return — every subsequent recv
	// goes straight to the dispatch path (asyncPromoted guard above).
	if tryInline {
		if errors.Is(processErr, conn.ErrAsyncDispatch) {
			// celeris#364: record the route that forced promotion (single-shot
			// recv only — the revert path assumes the worker-owned cs.buf recv
			// model). Set BEFORE asyncPromoted/goroutine start so the dispatch
			// goroutine observes it (happens-before). Empty path => the
			// goroutine treats the conn as not revert-eligible.
			if w.bufRing == nil {
				cs.promotedMethod, cs.promotedPath = cs.h1State.CurrentRoute()
			}
			cs.asyncPromoted.Store(true)
			w.asyncPromoted.Add(1)
			stashed := cs.h1State.TakeBufferedBytes()
			// Flush any inline-handled response (pipelined sync request
			// before the async one) before promoting, so the sync
			// response ships immediately instead of waiting on the async
			// handler runtime (#300 L1). flushSend self-gates on
			// cs.sending / cs.zcNotifPending — when a SEND is already
			// in flight it's a no-op and the dispatch goroutine's later
			// flush picks up writeBuf intact, preserving order. SQ
			// pressure (returns true) is non-fatal: the dispatch
			// goroutine's direct unix.Write will still ship the bytes.
			if len(cs.writeBuf) > 0 && !cs.sending && !cs.zcNotifPending {
				_ = w.flushSend(cs)
			}
			if hasProvidedBuf {
				w.bufRing.PushBuffer(providedBufID)
				w.hasBufReturns = true
			}
			w.promoteConnToAsync(cs, fd, stashed, c)
			return
		}
		// Inline handled the request(s). If ProcessH1 left partial state
		// (buffered headers / accumulating body), promote so the
		// continuation runs on the dispatch goroutine — the partial-state
		// parse paths must not run inline (only the fresh-parse site
		// honors the async check).
		if processErr == nil && cs.h1State.HasPendingDispatchState() {
			// No complete request parsed yet (buffered headers / chunked), so
			// no route to record — leave promotedPath empty: not revert-eligible.
			cs.asyncPromoted.Store(true)
			w.asyncPromoted.Add(1)
		}
	}

	// Batch-return the provided buffer after processing. The data has been
	// consumed by the protocol handler. Actual publish happens after the CQE
	// drain loop completes (P0).
	if hasProvidedBuf {
		w.bufRing.PushBuffer(providedBufID)
		w.hasBufReturns = true
	}

	w.reqBatch++

	// Slowloris-defence: retry timer arm if the initial attempt dropped
	// silently under SQ-ring pressure. Mirrors the async-path retry above.
	// Sync mode: ProcessH1 is idempotent on HeaderDeadlineNs so it won't
	// re-trigger OnHeaderDeadlineArmed if the deadline is already set,
	// leaving conns whose initial arm failed dependent on the sweep alone.
	if cs.h1State != nil && cs.h1State.HeaderDeadlineNs.Load() > 0 && !cs.headerTimerArmed {
		w.armHeaderTimer(cs)
	}

	// lastActivity already set above; timeout checked in checkTimeouts.

	if processErr != nil {
		if errors.Is(processErr, conn.ErrHijacked) {
			return // FD already detached
		}
		// Flush pending writes (e.g. error responses) before closing.
		_ = w.flushSend(cs)
		// Check cs.h1State under detachMu (same TOCTOU class fixed at
		// line 944 — see handleRecv recv-failure path).
		if cs.detachMu != nil {
			cs.detachMu.Lock()
			if cs.h1State != nil && cs.h1State.OnError != nil {
				cs.h1State.OnError(processErr)
			}
			cs.detachMu.Unlock()
		}
		w.closeConn(fd)
		return
	}

	// Flush response with linked RECV when using single-shot per-connection
	// buffers. The linked SEND→RECV lets the kernel start RECV immediately
	// after SEND completes, eliminating one loop iteration per request.
	cs.recvLinked = false
	// Hoist the detachMu load: the same mu is checked Lock and Unlock on
	// the success path, plus once on the early-return overflow path. One
	// pointer load instead of three.
	mu := cs.detachMu
	if mu != nil {
		mu.Lock()
	}
	// Back-pressure: capture pending size inside the lock so concurrent
	// goroutine writes via the guarded writeFn don't race the read.
	pending := len(cs.writeBuf) + len(cs.sendBuf)
	if pending > cs.sendCap() {
		if mu != nil {
			mu.Unlock()
		}
		w.closeConn(fd)
		return
	}
	if w.bufRing == nil {
		if w.flushSendLink(cs) {
			w.markDirty(cs)
		}
	} else {
		if w.flushSend(cs) {
			w.markDirty(cs)
		}
	}
	if mu != nil {
		mu.Unlock()
	}

	// For multishot recv, CQE_F_MORE means the kernel will produce more CQEs
	// without needing a new SQE. Only re-arm if multishot ended.
	// For linked SEND→RECV, the RECV is already queued — skip standalone re-arm.
	// Don't re-arm if recv is paused (WebSocket backpressure).
	if !cqeHasMore(c.Flags) && !cs.recvLinked && !cs.recvPaused {
		if !w.prepareRecv(cs, w.pickRecvTarget(cs)) {
			cs.needsRecv = true
			w.markDirty(cs)
		}
	}
}

func (w *Worker) handleSend(c *completionEntry, fd int, now int64) {
	cs := w.conns[fd]
	if cs == nil {
		return
	}

	// SEND_ZC notification CQE: the NIC has finished DMA-reading the buffer.
	// Now safe to modify/reuse sendBuf. Process the deferred result.
	if cqeIsNotif(c.Flags) {
		cs.zcNotifPending = false
		w.completeSend(cs, fd, int(cs.zcSentBytes), now)
		return
	}

	// SEND_ZC first CQE: result is ready but buffer is still in DMA.
	// Store the result and wait for the notification before touching sendBuf.
	if w.sendZC && cqeHasMore(c.Flags) {
		if c.Res < 0 {
			cs.sending = false
			cs.zcNotifPending = true
			cs.zcSentBytes = c.Res // store negative for error path on NOTIF
			return
		}
		cs.zcNotifPending = true
		cs.zcSentBytes = c.Res
		// sending stays true until NOTIF completes the cycle.
		return
	}

	// Regular SEND completion (non-ZC path). cs.sending is reset on
	// every exit branch — by completeSend on success, inline on error
	// paths — so we skip the entry-reset to save one write per request
	// on the hot success path.

	// SEND_ZC EINVAL fallback: kernel does not support the opcode.
	// Disable ZC for this worker and retry the send with regular SEND.
	if c.Res == -22 && w.sendZC {
		cs.sending = false
		w.sendZC = false
		w.logger.Warn("SEND_ZC not supported (EINVAL), falling back to regular SEND",
			"worker", w.id)
		if w.flushSend(cs) {
			w.markDirty(cs)
		}
		return
	}

	if c.Res < 0 {
		cs.sending = false
		w.errCount.Add(1)
		cs.sendBuf = cs.sendBuf[:0]
		mu := cs.detachMu
		if mu != nil {
			mu.Lock()
		}
		cs.writeBuf = cs.writeBuf[:0]
		if cs.h1State != nil && cs.h1State.OnError != nil {
			cs.h1State.OnError(errIORingSend(c.Res))
		}
		if mu != nil {
			mu.Unlock()
		}
		if cs.closing {
			w.finishCloseAny(fd, cs)
		} else {
			w.closeConn(fd)
		}
		return
	}

	w.completeSend(cs, fd, int(c.Res), now)
}

// completeSend processes a send result after the buffer is safe to modify.
// Called directly for regular SEND, or from the NOTIF handler for SEND_ZC.
//
// For detached connections, cs.sendBuf is read by the makeWriteFn closure
// running on the user's goroutine (back-pressure check at line 1222).
// All cs.sendBuf mutations below MUST be guarded by detachMu when one
// exists, otherwise the goroutine read races the event-loop write —
// observed via -race in TestNativeEngineLargePayload/io_uring.
func (w *Worker) completeSend(cs *connState, fd int, sent int, now int64) {
	cs.sending = false

	// Take the lock up-front for detached connections so the entire
	// state mutation (sendBuf truncate / writeBuf reset / OnError fire)
	// is serialized against the goroutine writeFn path.
	if mu := cs.detachMu; mu != nil {
		mu.Lock()
		defer mu.Unlock()
	}

	if sent < 0 {
		w.errCount.Add(1)
		cs.sendBuf = cs.sendBuf[:0]
		cs.writeBuf = cs.writeBuf[:0]
		cs.sendBody = nil
		cs.bodyBuf = nil
		if cs.h1State != nil && cs.h1State.OnError != nil {
			cs.h1State.OnError(errIORingSend(int32(sent)))
		}
		if cs.closing {
			w.finishCloseAny(fd, cs)
		} else {
			w.closeConn(fd)
		}
		return
	}

	// Partial-send handling, split by whether we issued a plain SEND
	// (sendBuf only) or a WRITEV (sendBuf + sendBody). Partial WRITEV
	// responses collapse the remainder into sendBuf so the retry path
	// uses the plain SEND fast-path; this pays a one-time body-sized
	// memcpy only when the kernel returned a short send (rare on
	// localhost TCP, occasional on congested networks).
	if len(cs.sendBody) > 0 {
		headerLen := len(cs.sendBuf)
		total := headerLen + len(cs.sendBody)
		switch {
		case sent >= total:
			cs.sendBuf = cs.sendBuf[:0]
			cs.sendBody = nil
		case sent >= headerLen:
			cs.sendBuf = cs.sendBuf[:0]
			cs.sendBuf = append(cs.sendBuf, cs.sendBody[sent-headerLen:]...)
			cs.sendBody = nil
		default:
			remaining := headerLen - sent
			copy(cs.sendBuf, cs.sendBuf[sent:])
			cs.sendBuf = cs.sendBuf[:remaining]
			cs.sendBuf = append(cs.sendBuf, cs.sendBody...)
			cs.sendBody = nil
		}
	} else if sent < len(cs.sendBuf) {
		remaining := len(cs.sendBuf) - sent
		copy(cs.sendBuf, cs.sendBuf[sent:])
		cs.sendBuf = cs.sendBuf[:remaining]
	} else {
		cs.sendBuf = cs.sendBuf[:0]
	}
	// detachMu (if any) is held by the deferred unlock at the top of
	// the function — no per-branch Unlock needed below.
	if cs.closing && len(cs.sendBuf) == 0 && len(cs.writeBuf) == 0 {
		w.finishCloseAny(fd, cs)
		return
	}

	// All data sent — re-arm recv if needed, remove from dirty list.
	if len(cs.sendBuf) == 0 && len(cs.writeBuf) == 0 {
		if cs.needsRecv && !cs.recvPaused {
			if w.prepareRecv(cs, cs.buf) {
				cs.needsRecv = false
			} else {
				w.markDirty(cs)
				return
			}
		}
		w.removeDirty(cs)
		cs.lastActivity = now
		return
	}

	// Re-send remainder or flush new data. Only markDirty on SQ ring full.
	// detachMu (if any) is held by the deferred unlock at the top.
	if w.flushSend(cs) {
		w.markDirty(cs)
	}
}

func (w *Worker) handleClose(fd int) {
	// finishClose already removed from conns and decremented activeConns.
	// With CQE_SKIP_SUCCESS, this handler may not fire for successful close.
	// Clear the slot as a safety guard for error CQEs.
	if fd >= 0 && fd < len(w.conns) {
		w.conns[fd] = nil
	}
	// Note: liveConns removal is the caller's responsibility — every
	// path that calls finishCloseAny reaches here through closeConn or
	// a similar path that already removed the FD from liveConns.
}

func (w *Worker) closeConn(fd int) {
	cs := w.conns[fd]
	if cs == nil {
		return
	}
	detached := cs.detachMu != nil
	if detached {
		cs.asyncClosed.Store(true)
		if cs.asyncCond.L != nil {
			cs.asyncInMu.Lock()
			cs.asyncCond.Broadcast()
			cs.asyncInMu.Unlock()
		}
		// Signal the detached goroutine's writeFn to stop writing.
		cs.detachMu.Lock()
		cs.detachClosed = true
		if cs.h1State != nil && cs.h1State.OnDetachClose != nil {
			cs.h1State.OnDetachClose()
			cs.h1State.OnDetachClose = nil
		}
		cs.detachMu.Unlock()
		// Drop callbacks once the engine relinquishes the conn so any
		// late goroutine references resolve to no-ops without crashing.
		if cs.h1State != nil {
			cs.h1State.PauseRecv = nil
			cs.h1State.ResumeRecv = nil
		}
		// Only decrement when OnDetach actually fired (WS/SSE detach).
		// Async mode pre-allocates detachMu in acquireConnState but does
		// NOT increment detachedCount, so decrementing here would cause
		// underflow for plain async-HTTP1 conns.
		if cs.h1State != nil && cs.h1State.Detached.Load() && w.detachedCount > 0 {
			w.detachedCount--
		}
	}
	w.removeDirty(cs)
	// Close H1 state unless a real WS/SSE detach handed ownership to a
	// middleware goroutine. Async-mode HTTP1 conns have detachMu set
	// but h1State.Detached is false — we still own H1 state there.
	//
	// For detached / async-dispatched conns, we MUST hold detachMu
	// while tearing down h1State. runAsyncHandler runs ProcessH1 under
	// the same lock, and CloseH1 writes state fields (state.stream,
	// state.bodyBuf, state.bodyNeeded) that ProcessH1 reads. Without
	// the lock, a peer close arriving while the goroutine is mid-
	// ProcessH1 races the h1State teardown; under churn-close+
	// async+auto+upg the resulting memory corruption manifested as a
	// SIGSEGV in runtime.stackpoolalloc after ~16 h of load
	// (#256). cs.asyncClosed was already set earlier in this function
	// and the goroutine checks it on loop re-entry, so acquiring
	// detachMu here only blocks for the duration of the current
	// ProcessH1 call.
	trulyDetached := detached && cs.h1State != nil && cs.h1State.Detached.Load()
	if !trulyDetached && cs.h1State != nil {
		if detached {
			cs.detachMu.Lock()
			conn.CloseH1(cs.h1State)
			cs.detachMu.Unlock()
		} else {
			conn.CloseH1(cs.h1State)
		}
	}
	if cs.h2State != nil {
		conn.CloseH2(cs.h2State)
		w.removeH2Conn(fd)
	}

	// Defer actual close until all in-flight and pending SENDs complete,
	// so the last bytes (GOAWAY / RST_STREAM / WS close-echo) reach the
	// client before SHUT_WR. For detached connections this specifically
	// guards the WS close handshake: the WS middleware queues a close-
	// echo frame and then asks the engine to drop the FD via
	// SetWSIdleDeadline(1); without this guard the subsequent
	// checkTimeouts → closeConn pair would fire before the SEND SQE
	// submitted by the dirty-list flush has completed, and the echo
	// would never leave the kernel.
	if cs.sending || cs.zcNotifPending || len(cs.sendBuf) > 0 || len(cs.writeBuf) > 0 {
		cs.closing = true
		if w.flushSend(cs) {
			w.markDirty(cs)
		}
		return
	}

	if detached {
		// Skip deferred-close and pool return while any goroutine holds
		// closure references to cs (WS/SSE detach or async dispatch).
		// GC collects cs once the goroutine finishes and all closure
		// references are dropped.
		w.finishCloseDetached(fd, cs)
		return
	}

	w.finishClose(fd)
}

// cancelConnOps submits ASYNC_CANCEL SQEs for every conn-buffer-targeting
// op the kernel still holds for cs — the armed recv and (belt-and-braces;
// the close paths drain sends first) an in-flight send. Called by the
// close paths BEFORE the fd is closed: unix.Close does not complete a
// pending io_uring recv, so without the cancel the op would sit armed on
// cs.buf until straggler data (e.g. a retransmitted POST segment at
// RTO ≥ 200 ms) completes it — the v1.4.15/7beebb9 heap-corruption trigger. With the
// cancel, the terminal CQE (-ECANCELED, or the op's natural completion if
// it raced the cancel) arrives within a loop pass or two and
// drainPendingRelease can release cs promptly.
//
// Targeting mirrors prepCancelUserDataSkipSuccess's WS-pause usage: match
// by the op's exact generation-tagged user_data, which is unambiguous for
// both real fds and fixed-file indexes, and — because the cancel SQE
// enters the ring BEFORE the fd/slot can be reused — can never hit a
// successor conn's op. The cancel's own CQE is suppressed on success and
// tagged udProvide on failure (-ENOENT/-EALREADY when the op completed
// first), so the dispatcher drops it; only the cancelled op's own
// terminal CQE feeds the kernelInflight accounting.
//
// On a full SQ ring, mirror armHeaderTimer: Submit to drain, retry once,
// and otherwise proceed without the cancel — the op then terminates on
// peer data/FIN or the pendingRelease backstop reaps cs with a WARN.
func (w *Worker) cancelConnOps(fd int, cs *connState) {
	if cs.recvArmed {
		if sqe := w.getCancelSQE(); sqe != nil {
			prepCancelUserDataSkipSuccess(sqe, encodeUserDataGen(udRecv, fd, cs.generation))
			setSQEUserData(sqe, encodeUserData(udProvide, fd))
		}
	}
	if cs.sending || cs.zcNotifPending {
		if sqe := w.getCancelSQE(); sqe != nil {
			prepCancelUserDataSkipSuccess(sqe, encodeUserDataGen(udSend, fd, cs.generation))
			setSQEUserData(sqe, encodeUserData(udProvide, fd))
		}
	}
}

// getCancelSQE returns an SQE for a close-path cancel, submitting the
// pending SQ ring once to make room if it is full (the armHeaderTimer
// pattern). Returns nil only if the ring is full even after the submit.
func (w *Worker) getCancelSQE() unsafe.Pointer {
	sqe := w.ring.GetSQE()
	if sqe != nil {
		return sqe
	}
	if _, err := w.ring.Submit(); err != nil {
		return nil
	}
	return w.ring.GetSQE()
}

// noteClosedInflight registers cs in Worker.closedOps when it still has
// kernel-held ops at close time, so their terminal CQEs — which arrive
// after w.conns[fd] is niled and therefore dispatch as stale — can be
// attributed back to cs (noteStaleTerminalOp) and unblock its release.
// Must be called by every close path that queues cs for deferred release,
// after the last arm/cancel decision for cs has been made.
func (w *Worker) noteClosedInflight(cs *connState) {
	if cs.kernelInflight <= 0 {
		return
	}
	if w.closedOps == nil {
		w.closedOps = make(map[uint64]*closedOpsEntry)
	}
	key := encodeConnOpKey(cs.fd, cs.generation)
	e := w.closedOps[key]
	if e == nil {
		e = &closedOpsEntry{}
		w.closedOps[key] = e
	}
	e.inflight += cs.kernelInflight
	e.conns = append(e.conns, cs)
}

// dropClosedOps removes cs's identity from Worker.closedOps when the
// wall-clock backstop releases it with ops still unaccounted for. Without
// this, a terminal CQE arriving after the backstop would write through
// the map into a connState the pool may have already handed to a new
// conn. Any conns colliding on the same identity lose their accounting
// too and will be reaped by their own backstop — acceptable for a path
// that only fires on kernel anomalies.
func (w *Worker) dropClosedOps(cs *connState) {
	if len(w.closedOps) == 0 {
		return
	}
	delete(w.closedOps, encodeConnOpKey(cs.fd, cs.generation))
}

// queuePendingRelease enqueues cs for deferred release: drainPendingRelease
// recycles it once cs.kernelInflight hits zero (or the wall-clock backstop
// fires). See Worker.pendingRelease docstring for the kernel-buffer-lifetime
// invariant this enforces.
func (w *Worker) queuePendingRelease(cs *connState) {
	w.pendingRelease = append(w.pendingRelease, pendingReleaseEntry{
		cs:             cs,
		releaseAtNanos: time.Now().UnixNano() + pendingReleaseHoldNanos,
	})
}

// queuePendingReleaseDetached holds cs alive past the kernel's recv-SQE
// drain window without recycling it through sync.Pool. Used by the
// detached close path (async-dispatch HTTP/1.1, WebSocket, SSE), where
// goroutine closures still reference cs.h1State / cs.asyncCond /
// cs.asyncInBuf — the worker observed asyncClosed and tore the conn
// down, but the dispatch goroutine can still be inside its deferred
// recover() block, reading cs.fd and re-clearing cs.asyncInBuf, when
// finishCloseDetached returns. Recycling cs through releaseConnState
// at this point races those reads. We just keep the strong ref alive
// until the kernel ops drain (cs.kernelInflight == 0) and let GC
// collect — the goroutine's own closure references keep cs alive for
// however long it still needs it, but only THIS queue's strong ref is
// visible on behalf of the kernel's invisible recv pointer, so it must
// outlive every pending op targeting cs.buf // span-corruption class).
func (w *Worker) queuePendingReleaseDetached(cs *connState) {
	w.pendingRelease = append(w.pendingRelease, pendingReleaseEntry{
		cs:             cs,
		releaseAtNanos: time.Now().UnixNano() + pendingReleaseHoldNanos,
		detached:       true,
	})
}

// drainPendingRelease releases queued connStates whose kernel-held ops
// have all delivered their terminal CQE (cs.kernelInflight == 0 — the
// close path's ASYNC_CANCELs make that prompt), compacting the queue in
// place. Entries still holding kernel ops stay queued until their
// wall-clock backstop (releaseAtNanos vs cachedNow; the deadline is a
// fresh time.Now()+hold from enqueue so the window is real time even
// when cachedNow lags — v1.5.0 review 2.9). The backstop firing means
// the kernel never delivered a terminal CQE for an op we believe it
// holds — log a WARN, since releasing now trades a potential
// use-after-free against an unbounded leak, and scrub the conn from
// closedOps so a later CQE cannot touch the released memory.
//
// Detached entries skip the pool recycle (releaseConnState would
// reset fields that goroutine closures may still observe via the
// runAsyncHandler defer block) and just drop the strong ref so GC
// can reclaim the cs.
//
// In steady state entries drain within a loop pass or two, so the queue
// stays a handful of entries deep and the full scan is cheap; it is the
// straggler entries themselves that would otherwise block a FIFO-prefix
// scan, so compaction is required for prompt release behind them.
func (w *Worker) drainPendingRelease() {
	kept := w.pendingRelease[:0]
	for i := range w.pendingRelease {
		entry := &w.pendingRelease[i]
		cs := entry.cs
		if cs.kernelInflight > 0 {
			if entry.releaseAtNanos > w.cachedNow {
				kept = append(kept, *entry)
				continue
			}
			// Backstop: kernel anomaly, not normal flow.
			if w.logger != nil {
				w.logger.Warn("releasing connState with kernel ops unaccounted for after backstop hold",
					"worker", w.id, "fd", cs.fd, "generation", cs.generation,
					"inflight", cs.kernelInflight, "detached", entry.detached)
			}
			w.dropClosedOps(cs)
		}
		if !entry.detached {
			releaseConnState(cs)
		}
	}
	// Nil out the tail slots so the backing array drops its strong refs
	// to released connStates.
	for i := len(kept); i < len(w.pendingRelease); i++ {
		w.pendingRelease[i].cs = nil
	}
	w.pendingRelease = kept
}

func (w *Worker) finishClose(fd int) {
	cs := w.conns[fd]
	// Remove from liveConns BEFORE niling w.conns[fd]: removeLiveConn swaps
	// the last live entry into cs.liveIdx and updates that swapped-in
	// connState's liveIdx via w.conns[swappedFD], so the conns slice must
	// still be intact (v1.5.0 review 1.8 hazard).
	w.removeLiveConn(cs)
	w.conns[fd] = nil
	w.connCount--
	w.activeConns.Add(-1)

	if w.cfg.OnDisconnect != nil && cs != nil {
		w.cfg.OnDisconnect(cs.remoteAddr)
	}

	// Capture close-path decisions before queueing cs for deferred release.
	fixedFile := cs != nil && cs.fixedFile
	fastClose := cs != nil && engine.Protocol(cs.protocol.Load()) == engine.HTTP1 && cs.h1State != nil && !cs.h1State.Detached.Load()
	// Cancel-then-release discipline (v1.4.15/7beebb9 corruption fix): ASYNC_CANCEL any kernel-held
	// op still targeting cs's buffers (the single-shot recv is virtually
	// ALWAYS armed here — closing the fd below does NOT complete it), then
	// register cs for stale-CQE accounting and defer the pool release
	// until every op has delivered its terminal CQE. Returning cs to
	// sync.Pool before that would let Go's GC reclaim cs.buf's backing
	// array; the kernel's still-pending recv SQE would then write
	// straggler bytes into memory Go has repurposed — the #256 stackalloc
	// SIGSEGV / Green-Tea-GC span-corruption class.
	if cs != nil {
		w.cancelConnOps(fd, cs)
		w.noteClosedInflight(cs)
		w.queuePendingRelease(cs)
	}

	if fixedFile {
		// Fixed file: close via io_uring direct close (no real FD to shutdown).
		// Stamp the closing conn's generation so its close CQE is matched to
		// THIS occupant — a stale close-error CQE for a freed+reused slot is
		// dropped at dispatch before reaching handleClose, so it can no longer
		// nil the new occupant (review 2.2 error path). cs is still valid here:
		// queuePendingRelease only DEFERS releaseConnState.
		sqe := w.ring.GetSQE()
		if sqe != nil {
			prepCloseDirect(sqe, fd)
			gen := uint16(0)
			if cs != nil {
				gen = cs.generation
			}
			setSQEUserData(sqe, encodeUserDataGen(udClose, fd, gen))
		}
		// Explicitly reset the fixed file slot to -1 so the kernel's
		// IORING_FILE_INDEX_ALLOC allocator can reuse it. Without this,
		// some kernels (e.g., AWS 6.17) fail to recycle CLOSE_DIRECT'd
		// slots, exhausting the 65536-entry table under sustained churn.
		_ = w.ring.UpdateFixedFile(fd, -1)
		return
	}

	// Fast-close path for H1 non-detached connections: close() alone.
	// The response bytes we wrote are already in the kernel send buffer
	// and will go out before the socket tears down; localhost ACKs
	// within microseconds. Skipping shutdown(SHUT_WR) + drainRecvBuffer
	// saves two syscalls per close and is the difference between hertz
	// territory (~30 k rps) and ~24 k rps under bench-harness churn.
	//
	// The graceful path (shutdown + drain + close) is retained for H2
	// because GOAWAY / RST_STREAM frames can be staged in the send
	// buffer at close time; shutdown is what pushes FIN after those
	// frames so the peer sees them. For H1 without a body in-flight,
	// close() without shutdown is equivalent on localhost and within
	// noise on real networks.
	//
	// forceRSTClose (slowloris-defence): SHUT_RDWR + close. See
	// finishCloseDetached for the rationale.
	if cs != nil && cs.forceRSTClose {
		_ = unix.Shutdown(fd, unix.SHUT_RDWR)
		_ = unix.Close(fd)
		return
	}
	// fastClose: plain H1 no-body conn → close() alone suffices.
	if fastClose {
		_ = unix.Close(fd)
		return
	}
	_ = unix.Shutdown(fd, unix.SHUT_WR)
	drainRecvBuffer(fd)
	_ = unix.Close(fd)
}

// finishCloseAny dispatches to finishCloseDetached for detached connections
// and finishClose otherwise. Used by the deferred-close paths in
// handleSend / completeSend, where the connection may be either a plain
// H1/H2 conn or a detached WS/SSE conn (distinguished by detachMu).
func (w *Worker) finishCloseAny(fd int, cs *connState) {
	if cs.detachMu != nil {
		w.finishCloseDetached(fd, cs)
		return
	}
	w.finishClose(fd)
}

// finishCloseDetached closes the FD and removes the connection from bookkeeping
// WITHOUT returning the connState to the pool. Used when a detached goroutine
// still holds closure references to the connState.
func (w *Worker) finishCloseDetached(fd int, cs *connState) {
	// Remove from liveConns BEFORE niling w.conns[fd] (same hazard as
	// finishClose — removeLiveConn touches w.conns[swappedFD]).
	w.removeLiveConn(cs)
	w.conns[fd] = nil
	w.connCount--
	w.activeConns.Add(-1)

	if w.cfg.OnDisconnect != nil {
		w.cfg.OnDisconnect(cs.remoteAddr)
	}

	fixedFile := cs.fixedFile
	// Do NOT call releaseConnState — goroutine closures still reference cs.
	//
	// Cancel-then-release discipline (v1.4.15/7beebb9 corruption fix): ASYNC_CANCEL the armed recv
	// (closing the fd below does NOT complete it — the op holds its own
	// file reference), then hold cs alive in pendingRelease until its
	// terminal CQE arrives. After the goroutine exits (which happens
	// promptly once closeConn sets asyncClosed and broadcasts asyncCond),
	// only this queue's strong ref stands in for the kernel's invisible
	// recv pointer. Without it, GC would reclaim cs.buf while the kernel
	// still has a pending recv SQE targeting &cs.buf[0]; a straggler
	// segment then writes HTTP bytes into memory Go has repurposed —
	// historically a stackalloc SIGSEGV at addr 0x48202f20544547 (#256,
	// strict-matrix churn-close × iouring-async), and on Go 1.26 a fatal
	// "s.allocCount != s.nelems" span corruption (v1.4.15/7beebb9: the old 100 ms
	// wall-clock hold sat below TCP's 200 ms RTO_MIN, so retransmitted
	// POST segments landed after release).
	w.cancelConnOps(fd, cs)
	w.noteClosedInflight(cs)
	w.queuePendingReleaseDetached(cs)

	if fixedFile {
		sqe := w.ring.GetSQE()
		if sqe != nil {
			prepCloseDirect(sqe, fd)
			// Stamp the closing conn's generation (review 2.2 error path) —
			// cs is the non-nil param and stays valid (deferred release).
			setSQEUserData(sqe, encodeUserDataGen(udClose, fd, cs.generation))
		}
		_ = w.ring.UpdateFixedFile(fd, -1)
		return
	}

	// forceRSTClose: slowloris-defence path requested RST. Empirical
	// data (nightly 26418830572 vs 26414322152): SHUT_RDWR + close
	// gives ~50% more reliable observable-RST than plain close +
	// LINGER on iouring. The SHUT_RDWR + LINGER combo wins because
	// SHUT_RDWR drops the receive queue too (close() with LINGER
	// only drops TX). Walker observes the abortive close immediately.
	if cs.forceRSTClose {
		_ = unix.Shutdown(fd, unix.SHUT_RDWR)
		_ = unix.Close(fd)
		return
	}
	// Async-mode HTTP1 conns are NOT truly detached (no WS/SSE middleware
	// owns them) — they just pre-allocate detachMu in acquireConnState so
	// the dispatch goroutine and worker can serialize writeBuf access.
	//
	// Plain unix.Close (matching net/http) sends FIN if the kernel recv
	// buffer is empty, RST if non-empty. For slowloris, walker is still
	// writing drips so the buffer often has unread bytes — close → RST.
	// RST is NOT retransmitted by TCP, so if it's lost on the cluster
	// fabric the walker never observes the close (walker hangs).
	//
	// SHUT_WR forces FIN explicitly (writer half-close) BEFORE close,
	// regardless of recv-buffer state. FIN IS retransmitted from
	// FIN_WAIT_1 until ACK'd, so packet loss can't strand the walker.
	// No drainRecvBuffer (which was the prior approach's race source —
	// the drain syscall and the close syscall left a multi-µs window in
	// which a fresh walker drip would queue, making the close → RST).
	if cs.h1State != nil && !cs.h1State.Detached.Load() {
		_ = unix.Shutdown(fd, unix.SHUT_WR)
		_ = unix.Close(fd)
		return
	}
	// Truly detached (WS/SSE): graceful half-close so middleware-queued
	// close-frame echoes flush before the FIN. Sync unix.Close (not via
	// io_uring) to avoid the async-SQE pile-up that plagued the pre-patch
	// version.
	_ = unix.Shutdown(fd, unix.SHUT_WR)
	drainRecvBuffer(fd)
	_ = unix.Close(fd)
}

// runAsyncHandler is the dispatch goroutine for an HTTP1 conn when
// Config.AsyncHandlers is enabled. Drains cs.asyncInBuf in a loop: take
// currently-buffered bytes, run ProcessH1 under cs.detachMu (serializes
// with worker-initiated writeBuf/sendBuf mutations), enqueue on
// detachQueue so the worker submits SEND SQEs on its own goroutine
// (SINGLE_ISSUER — handler Gs cannot call ring.GetSQE directly).
// Preserves HTTP/1.1 pipelining: ProcessH1's offset loop drains every
// request in the slice in order, and responses land on cs.writeBuf in
// the same order before the flush.
// promoteConnToAsync hands a conn that bailed from the inline fast path
// (ErrAsyncDispatch) to its per-conn dispatch goroutine, seeding it with the
// stashed request bytes. Mirrors the tail of the async-dispatch block. The
// caller has already set cs.asyncPromoted and returned the provided buffer.
func (w *Worker) promoteConnToAsync(cs *connState, _ int, stashed []byte, c *completionEntry) {
	cs.asyncInMu.Lock()
	cs.asyncInBuf = append(cs.asyncInBuf, stashed...)
	starting := !cs.asyncRun
	if starting {
		cs.asyncRun = true
	}
	cs.asyncInMu.Unlock()
	if starting {
		w.asyncWG.Add(1)
		go w.runAsyncHandler(cs)
	} else {
		cs.asyncCond.Signal()
	}
	w.reqBatch++
	if cs.h1State != nil && cs.h1State.HeaderDeadlineNs.Load() > 0 && !cs.headerTimerArmed {
		w.armHeaderTimer(cs)
	}
	if !cqeHasMore(c.Flags) && !cs.recvPaused {
		if !w.prepareRecv(cs, cs.buf) {
			cs.needsRecv = true
			w.markDirty(cs)
		}
	}
}

// canRevertToInline reports whether a promoted conn should be reverted to the
// inline fast path (celeris#364). True when single-shot recv is in use, the
// conn recorded the route that promoted it, and that route's promotion has
// since expired (RouteAsync now false — the route-level TTL de-promotion).
// Called by runAsyncHandler ONLY while holding asyncInMu with asyncInBuf empty.
func (w *Worker) canRevertToInline(cs *connState) bool {
	return w.bufRing == nil && cs.promotedPath != "" && cs.h1State != nil &&
		cs.h1State.RouteAsync != nil &&
		// Clean request boundary only: never revert mid-request (a partial body
		// or buffered headers still accumulating), so h1State ownership flips
		// back to the worker between requests, exactly like a fresh inline conn.
		!cs.h1State.HasPendingData() &&
		!cs.h1State.RouteAsync(cs.promotedMethod, cs.promotedPath)
}

func (w *Worker) runAsyncHandler(cs *connState) {
	defer w.asyncWG.Done()
	defer func() {
		if r := recover(); r != nil {
			if w.logger != nil {
				w.logger.Error("async handler panicked",
					"panic", r,
					"stack", string(debug.Stack()),
					"fd", cs.fd,
				)
			}
			cs.asyncClosed.Store(true)
			cs.asyncInMu.Lock()
			cs.asyncInBuf = cs.asyncInBuf[:0]
			cs.asyncRun = false
			cs.asyncInMu.Unlock()
			// Wake the worker so it observes asyncClosed and tears
			// down the conn via the detachQueue → drain path.
			w.detachQMu.Lock()
			w.detachQueue = append(w.detachQueue, cs)
			w.detachQPending.Store(1)
			w.detachQMu.Unlock()
			if w.h2EventFD >= 0 {
				var val [8]byte
				val[0] = 1
				_, _ = unix.Write(w.h2EventFD, val[:])
			}
		}
	}()
	for {
		cs.asyncInMu.Lock()
		for len(cs.asyncInBuf) == 0 && !cs.asyncClosed.Load() {
			// celeris#364: revert this conn to inline when the route that
			// promoted it has de-promoted (its TTL expired). Safe ONLY here:
			// asyncInBuf is empty (no in-flight input, last response already
			// written) and we hold asyncInMu, which the worker's feed path
			// re-acquires and re-checks asyncPromoted against — so clearing it
			// here cannot race a concurrent feed. The worker owns recv and
			// resumes the inline fast path on the next CQE.
			if w.canRevertToInline(cs) {
				cs.asyncPromoted.Store(false)
				cs.asyncRun = false
				cs.asyncInMu.Unlock()
				return
			}
			cs.asyncCond.Wait()
		}
		if cs.asyncClosed.Load() {
			cs.asyncRun = false
			cs.asyncInMu.Unlock()
			return
		}
		cs.asyncInBuf, cs.asyncOutBuf = cs.asyncOutBuf[:0], cs.asyncInBuf
		data := cs.asyncOutBuf
		cs.asyncInMu.Unlock()

		// Post-detach iterations (WS / SSE): ProcessH1's only job is to
		// deliver `data` to state.WSDataDelivery (writes flow through the
		// guarded writeFn, which acquires cs.detachMu on its own). Taking
		// detachMu here would re-Lock it on every torture-frame delivery
		// — and the post-ProcessH1 branch below (asyncDetachUnlocked)
		// would then skip the symmetric Unlock, leaking the mutex.
		// Once leaked, the WS handler's writeCloseProtocol →
		// writeCloseFrame → guarded → cs.detachMu.Lock() deadlocks
		// forever, which is exactly the per-worker WS-handler hang that
		// celeris#284 surfaced (handler never returns, deferred ws.Close
		// never runs, idleDeadlineFn(1) never fires, conn stays
		// "detached forever," subsequent /ws upgrades on the same worker
		// accumulate stuck state). The pre-detach iteration (the one
		// where the WS middleware calls c.Detach inside ProcessH1) still
		// needs the lock because the handler chain may write a response
		// to cs.writeBuf before Detach fires; OnDetach itself drops the
		// lock on celeris#273's behalf (see line 974).
		acquiredDetachMu := false
		if !cs.asyncDetachUnlocked {
			cs.detachMu.Lock()
			acquiredDetachMu = true
		}
		// Re-check asyncClosed under detachMu (when we acquired it).
		// closeConn sets asyncClosed BEFORE taking detachMu to run
		// CloseH1; if we raced past the top-of-loop check but closeConn
		// acquired detachMu first, by the time we hold detachMu our
		// cs.h1State may already be torn down. Bail out here so we
		// don't call ProcessH1 on a closed state.
		if cs.asyncClosed.Load() {
			if acquiredDetachMu {
				cs.detachMu.Unlock()
			}
			cs.asyncInMu.Lock()
			cs.asyncRun = false
			cs.asyncInMu.Unlock()
			return
		}
		processErr := conn.ProcessH1(cs.ctx, data, cs.h1State, w.handler, cs.writeFn)
		// H1→H2 upgrade on the async dispatch path. ProcessH1 has
		// written the 101 Switching Protocols response to cs.writeBuf
		// and stashed the upgrade info. Promote cs-local state now
		// (safe under detachMu), flush writeBuf synchronously, then
		// hand off to the worker to register the fd on the H2 write-
		// queue poll list. The goroutine exits — all subsequent recvs
		// dispatch via the inline H2 path (cs.protocol is now H2C).
		if errors.Is(processErr, conn.ErrUpgradeH2C) {
			promoteErr := w.switchToH2Local(cs)
			if promoteErr == nil && len(cs.writeBuf) > 0 {
				n, werr := unix.Write(cs.fd, cs.writeBuf)
				switch {
				case werr == nil && n == len(cs.writeBuf):
					cs.writeBuf = cs.writeBuf[:0]
				case werr == nil:
					// Partial write — shift remainder; worker retries
					// via markDirty after we enqueue on detachQueue.
					remaining := len(cs.writeBuf) - n
					copy(cs.writeBuf, cs.writeBuf[n:])
					cs.writeBuf = cs.writeBuf[:remaining]
				case werr == unix.EAGAIN || werr == unix.EWOULDBLOCK:
					// Socket send buffer full — same disposition as a
					// partial write; bytes stay in cs.writeBuf.
				default:
					promoteErr = werr
				}
			}
			cs.detachMu.Unlock()
			if promoteErr != nil {
				// Fatal — route through the asyncClosed teardown so the
				// worker runs closeConn from its own goroutine.
				cs.asyncClosed.Store(true)
				cs.asyncInMu.Lock()
				cs.asyncInBuf = cs.asyncInBuf[:0]
				cs.asyncRun = false
				cs.asyncInMu.Unlock()
			} else {
				cs.asyncInMu.Lock()
				cs.asyncInBuf = cs.asyncInBuf[:0]
				cs.asyncRun = false
				cs.asyncInMu.Unlock()
				cs.asyncH2Promoted.Store(true)
			}
			w.detachQMu.Lock()
			w.detachQueue = append(w.detachQueue, cs)
			w.detachQPending.Store(1)
			w.detachQMu.Unlock()
			if w.h2EventFD >= 0 {
				var val [8]byte
				val[0] = 1
				_, _ = unix.Write(w.h2EventFD, val[:])
			}
			return
		}
		// celeris#273: a user handler may have called c.Detach() inside
		// ProcessH1 (websocket or sse middleware). OnDetach released
		// detachMu so subsequent guarded writeFn calls don't deadlock.
		// The dispatch goroutine no longer owns the lock — skip the
		// direct-write path (the bytes were already enqueued via
		// guarded → detachQueue/eventfd, the worker will flushSend
		// them) and skip the symmetric Unlock below.
		if cs.asyncDetachUnlocked {
			// ErrHijacked is a valid post-Detach return: the H1 parser
			// considers a hijacked conn "done with the request". Treat
			// it like nil here — the middleware now owns the conn and
			// is responsible for its lifetime via the done() callback.
			if processErr != nil && !errors.Is(processErr, conn.ErrHijacked) {
				cs.asyncClosed.Store(true)
				cs.asyncInMu.Lock()
				cs.asyncInBuf = cs.asyncInBuf[:0]
				cs.asyncRun = false
				cs.asyncInMu.Unlock()
				w.detachQMu.Lock()
				w.detachQueue = append(w.detachQueue, cs)
				w.detachQPending.Store(1)
				w.detachQMu.Unlock()
				if w.h2EventFD >= 0 {
					var val [8]byte
					val[0] = 1
					_, _ = unix.Write(w.h2EventFD, val[:])
				}
				return
			}
			// Post-Detach: loop back to wait for more recv bytes (WS
			// frames delivered via WSDataDelivery, or SSE conn-close
			// detection). The handler runs in its own goroutine
			// spawned by the middleware; this dispatch goroutine just
			// shuttles RX bytes into ProcessH1 → WSDataDelivery.
			continue
		}
		// Direct-write fast path: on the async-handler goroutine, call
		// unix.Write(fd, writeBuf) inline instead of bouncing through
		// the detachQueue → eventfd → worker → SEND-SQE round-trip.
		// The iouring multishot recv on this fd is unaffected — TCP is
		// bidirectional and the kernel happily admits a concurrent write
		// from any goroutine even while a recv SQE is pending. Mirrors
		// the engine/epoll runAsyncHandler shape (engine/epoll/loop.go:990)
		// and closes the 3× integrated-Redis regression observed on
		// iouring (95 µs/op → target ~30 µs/op, matching epoll and
		// go-redis + stdlib).
		var partial bool
		if processErr == nil && len(cs.writeBuf) > 0 {
			n, werr := unix.Write(cs.fd, cs.writeBuf)
			if werr != nil {
				if werr == unix.EAGAIN || werr == unix.EWOULDBLOCK {
					// Socket send buffer full — defer to the worker so
					// flushSend can retry under dirty-list management.
					partial = true
				} else {
					processErr = werr
				}
			} else if n < len(cs.writeBuf) {
				// Partial write — shift remainder and defer to worker.
				remaining := len(cs.writeBuf) - n
				copy(cs.writeBuf, cs.writeBuf[n:])
				cs.writeBuf = cs.writeBuf[:remaining]
				partial = true
			} else {
				cs.writeBuf = cs.writeBuf[:0]
			}
		}
		cs.detachMu.Unlock()

		if processErr != nil {
			cs.asyncClosed.Store(true)
			cs.asyncInMu.Lock()
			cs.asyncInBuf = cs.asyncInBuf[:0]
			cs.asyncRun = false
			cs.asyncInMu.Unlock()
			// Wake the worker so it notices asyncClosed and runs closeConn
			// from its own goroutine via the detachQueue → drain path.
			w.detachQMu.Lock()
			w.detachQueue = append(w.detachQueue, cs)
			w.detachQPending.Store(1)
			w.detachQMu.Unlock()
			if w.h2EventFD >= 0 {
				var val [8]byte
				val[0] = 1
				_, _ = unix.Write(w.h2EventFD, val[:])
			}
			return
		}

		if partial {
			w.detachQMu.Lock()
			w.detachQueue = append(w.detachQueue, cs)
			w.detachQPending.Store(1)
			w.detachQMu.Unlock()
			if w.h2EventFD >= 0 {
				var val [8]byte
				val[0] = 1
				_, _ = unix.Write(w.h2EventFD, val[:])
			}
		}
	}
}

func (w *Worker) makeWriteFn(cs *connState) func([]byte) {
	return func(data []byte) {
		if cs.closing {
			return
		}
		// Back-pressure: drop writes when total pending data exceeds limit.
		// The connection will be closed after processing completes.
		if len(cs.writeBuf)+len(cs.sendBuf)+len(cs.bodyBuf) > cs.sendCap() {
			return
		}
		// Append to writeBuf — no per-write allocation. The kernel holds
		// sendBuf (not writeBuf), so appending here is safe.
		// Don't markDirty here — handleRecv calls flushSend after the
		// handler returns. Only markDirty if flushSend fails (SQ ring
		// full), avoiding linked-list overhead on the happy path.
		cs.writeBuf = append(cs.writeBuf, data...)
	}
}

// makeWriteBodyFn returns a closure that stores a zero-copy body reference
// for scatter-gather send via IORING_OP_WRITEV. The body slice is NOT
// copied — it must remain valid and unmutated until completeSend clears
// cs.sendBody. For HTTP/1 response writers calling
// writeBody(pre-computed-response) this is always safe; handlers that
// generate bodies per-request should only call writeBody once with a
// slice they do not mutate further.
//
// Saves one full body-sized memcpy per request: the traditional path
// appends body into a.respBuf, then writeFn appends respBuf into
// cs.writeBuf (two userspace copies of body bytes). With writeBody, the
// body stays in the handler's memory; the engine issues a single WRITEV
// SQE with iovec = [sendBuf (headers), body (alias)] and the kernel
// does one copy directly to the socket buffer.
func (w *Worker) makeWriteBodyFn(cs *connState) func([]byte) {
	return func(body []byte) {
		if cs.closing {
			return
		}
		if len(cs.writeBuf)+len(cs.sendBuf)+len(cs.bodyBuf)+len(body) > cs.sendCap() {
			return
		}
		if cs.bodyBuf != nil {
			// A second large-body write in the same request: fall back
			// to copying (we only carry one iovec entry for the body).
			cs.writeBuf = append(cs.writeBuf, body...)
			return
		}
		cs.bodyBuf = body
	}
}

// prepareH2Poll submits a single-shot POLL_ADD SQE on the H2 eventfd.
// When handler goroutines write the eventfd, the CQE wakes the ring
// event-driven, replacing the 100μs polling timeout.
func (w *Worker) prepareH2Poll() {
	sqe := w.ring.GetSQE()
	if sqe == nil {
		return
	}
	prepPollAdd(sqe, w.h2EventFD, unix.POLLIN)
	setSQEUserData(sqe, encodeUserData(udH2Wakeup, w.h2EventFD))
}

// handleH2Wakeup drains the eventfd counter and re-arms the poll.
// The actual H2 write queue drain happens in the existing bottom-of-loop pass.
func (w *Worker) handleH2Wakeup() {
	var buf [8]byte
	_, _ = unix.Read(w.h2EventFD, buf[:])
	w.prepareH2Poll()
}

// prepareAccept submits an accept SQE using the best available mode.
func (w *Worker) prepareAccept() {
	sqe := w.ring.GetSQE()
	if sqe == nil {
		return
	}
	if w.fixedFiles {
		prepMultishotAcceptDirect(sqe, w.listenFD)
	} else if w.tier.SupportsMultishotAccept() {
		prepMultishotAccept(sqe, w.listenFD)
	} else {
		prepAccept(sqe, w.listenFD, 0)
	}
	setSQEUserData(sqe, encodeUserData(udAccept, w.listenFD))
}

// prepareRecv submits a recv SQE for cs. Uses multishot recv with
// ring-mapped provided buffers when available; falls back to single-shot
// per-connection buffer recv. Returns true if the SQE was submitted,
// false if the SQ ring was full.
//
// Takes the connState (not fd+gen) so the arm and its kernelInflight /
// recvArmed bookkeeping cannot diverge: every armed recv MUST be counted,
// or the close path would release cs.buf while the kernel still holds a
// write pointer into it (v1.4.15/7beebb9 corruption).
func (w *Worker) prepareRecv(cs *connState, buf []byte) bool {
	sqe := w.ring.GetSQE()
	if sqe == nil {
		return false
	}
	if w.bufRing != nil {
		prepMultishotRecv(sqe, cs.fd, bufRingGroupID, w.fixedFiles)
	} else {
		prepRecv(sqe, cs.fd, buf)
	}
	setSQEUserData(sqe, encodeUserDataGen(udRecv, cs.fd, cs.generation))
	cs.recvArmed = true
	cs.kernelInflight++
	return true
}

// pickRecvTarget selects the recv target for the next SQE on cs. When the
// H1 parser is in a partial-body state and there's bodyBuf tail capacity,
// it returns that slice and flags the conn so handleRecv routes the next
// CQE through the direct-body path (bypassing ProcessH1 + cs.buf memcpy).
// Disabled when a provided-buffer ring is in use (multishot recv path owns
// its own buffer lifecycle). Always clears cs.recvIntoBody when the normal
// cs.buf path is picked.
func (w *Worker) pickRecvTarget(cs *connState) []byte {
	cs.recvIntoBody = false
	// Drop any prior body-recv pin: pickRecvTarget is only invoked to arm a
	// NEW recv, which means the previous (single-shot) recv already
	// completed, so no kernel SQE still targets the old bodyBuf array. The
	// body path below re-sets the pin when it arms into bodyBuf again.
	cs.bodyRecvPin = nil
	// Async mode: only a PROMOTED conn hands h1State to the dispatch
	// goroutine, which the worker cannot safely observe NextRecvBuf against.
	// A non-promoted conn (celeris#356 inline-first) runs ProcessH1 on the
	// worker itself (tryInline), so the worker owns h1State exactly as the
	// sync path does and the zero-copy direct-into-bodyBuf recv is safe —
	// gate the bail on cs.asyncPromoted, not blanket w.async.
	if (w.async && cs.asyncPromoted.Load()) || w.bufRing != nil || cs.h1State == nil {
		return cs.buf
	}
	if !w.h1Only && engine.Protocol(cs.protocol.Load()) != engine.HTTP1 {
		return cs.buf
	}
	if b := cs.h1State.NextRecvBuf(); b != nil {
		cs.recvIntoBody = true
		// Pin the bodyBuf backing array (b shares it) so a later
		// conn.CloseH1 → state.bodyBuf=nil cannot let GC reclaim it while
		// this recv SQE is still in flight (#256 body-buffer UAF; see
		// connState.bodyRecvPin). Released by releaseConnState after the
		// pendingRelease window drains.
		cs.bodyRecvPin = b
		return b
	}
	return cs.buf
}

func (w *Worker) drainDetachQueue() {
	if w.detachQPending.Load() == 0 {
		return
	}
	w.detachQMu.Lock()
	w.detachQSpare, w.detachQueue = w.detachQueue, w.detachQSpare[:0]
	w.detachQPending.Store(0)
	w.detachQMu.Unlock()
	for _, cs := range w.detachQSpare {
		if cs.detachClosed {
			continue
		}
		// If the dispatch goroutine enqueued this conn because it
		// observed an error or recovered from a panic, it also set
		// asyncClosed. We're on the worker goroutine here — this is
		// where the conn-table teardown has to happen (dispatch
		// goroutine can't touch w.conns or the dirty list safely).
		if cs.asyncClosed.Load() {
			w.closeConn(cs.fd)
			continue
		}
		// Dispatch goroutine promoted the conn to H2 via switchToH2Local
		// on the h2c-upgrade path. Finish the worker-owned bits of the
		// swap: arm the H2 eventfd poll (once per worker) and register
		// the fd on the write-queue poll list. The conn stays alive;
		// subsequent recvs dispatch via the inline H2 path.
		if cs.asyncH2Promoted.Load() {
			cs.asyncH2Promoted.Store(false)
			if !w.h2PollArmed && w.h2EventFD >= 0 {
				w.prepareH2Poll()
				w.h2PollArmed = true
			}
			w.h2Conns = append(w.h2Conns, cs.fd)
			w.markDirty(cs)
			continue
		}
		// Async-mode Detach finalisation: OnDetach ran on the
		// dispatch goroutine and cannot touch worker-owned state
		// (w.detachedCount or the ring). It set asyncDetachPending
		// and enqueued cs so we land here on the worker thread and
		// run those mutations safely. Idempotent — the flag is
		// cleared before the bookkeeping so a second drainDetachQueue
		// pass (from the same enqueue burst) is a no-op.
		if cs.asyncDetachPending {
			cs.asyncDetachPending = false
			w.detachedCount++
			if !w.h2PollArmed && w.h2EventFD >= 0 {
				w.prepareH2Poll()
				w.h2PollArmed = true
			}
		}
		// Apply pending pause/resume request from the WS middleware.
		// Pause: cancel any in-flight RECV (multishot or single-shot)
		// targeting this fd. Once cancelled, no recv is re-armed until
		// resume is called. Resume: re-arm recv via prepareRecv.
		//
		// The cancel CQE (if any — sqeCQESkipSuccess suppresses success
		// CQEs) is tagged with udProvide so the main dispatcher silently
		// ignores it instead of routing through handleRecv.
		if desired := cs.recvPauseDesired.Load(); desired != cs.recvPaused {
			if desired {
				if sqe := w.ring.GetSQE(); sqe != nil {
					// Cancel the in-flight recv. cs.fd is a fixed-file
					// INDEX when fixed files are on, so cancelling by raw
					// fd would match nothing; match by the recv's
					// user_data instead, which is unambiguous in both
					// modes (v1.5.0 review 2.5). The match MUST include the
					// conn's generation — the recv SQE carries it (review
					// 2.6), so a gen-less target would match nothing. The
					// cancel SQE itself carries the udProvide tag so the
					// dispatcher drops its CQE.
					if cs.fixedFile {
						prepCancelUserDataSkipSuccess(sqe, encodeUserDataGen(udRecv, cs.fd, cs.generation))
					} else {
						prepCancelFDSkipSuccess(sqe, cs.fd)
					}
					setSQEUserData(sqe, encodeUserData(udProvide, cs.fd))
				}
			} else {
				if w.prepareRecv(cs, cs.buf) {
					cs.needsRecv = false
				} else {
					cs.needsRecv = true
				}
			}
			cs.recvPaused = desired
		}
		w.markDirty(cs)
	}
	w.detachQSpare = w.detachQSpare[:0]
}

func (w *Worker) markDirty(cs *connState) {
	if cs.dirty {
		return
	}
	cs.dirty = true
	cs.dirtyNext = w.dirtyHead
	cs.dirtyPrev = nil
	if w.dirtyHead != nil {
		w.dirtyHead.dirtyPrev = cs
	}
	w.dirtyHead = cs
}

func (w *Worker) removeH2Conn(fd int) {
	for i, f := range w.h2Conns {
		if f == fd {
			w.h2Conns[i] = w.h2Conns[len(w.h2Conns)-1]
			w.h2Conns = w.h2Conns[:len(w.h2Conns)-1]
			return
		}
	}
}

func (w *Worker) removeDirty(cs *connState) {
	if !cs.dirty {
		return
	}
	cs.dirty = false
	if cs.dirtyPrev != nil {
		cs.dirtyPrev.dirtyNext = cs.dirtyNext
	} else {
		w.dirtyHead = cs.dirtyNext
	}
	if cs.dirtyNext != nil {
		cs.dirtyNext.dirtyPrev = cs.dirtyPrev
	}
	cs.dirtyNext = nil
	cs.dirtyPrev = nil
}

// flushSend submits one SEND SQE for pending data on this connection.
// Only one SEND is in-flight per connection at a time; if a send is already
// in progress, this is a no-op and the next send will be triggered when the
// current one completes.
//
// Double-buffer strategy: writeBuf accumulates handler writes; sendBuf holds
// data the kernel is currently processing. On flush, writeBuf is swapped into
// sendBuf, and the old sendBuf's capacity is reused for the next writeBuf.
//
// Returns true if data is still pending and needs retry (SQ ring was full).
// The caller should markDirty only when this returns true.
func (w *Worker) flushSend(cs *connState) bool {
	if cs.sending || cs.zcNotifPending {
		return false // send in-flight; handleSend/NOTIF will pick up writeBuf
	}

	// If sendBuf still has data (partial send remainder), re-send it.
	if len(cs.sendBuf) > 0 {
		sqe := w.ring.GetSQE()
		if sqe == nil {
			return true // SQ ring full — caller should markDirty
		}
		w.prepSendSQE(sqe, cs, false)
		setSQEUserData(sqe, encodeUserDataGen(udSend, cs.fd, cs.generation))
		cs.sending = true
		cs.kernelInflight++
		w.sendsPending = true
		return false
	}

	// No in-flight data; swap writeBuf → sendBuf if there's new data.
	if len(cs.writeBuf) == 0 && len(cs.bodyBuf) == 0 {
		return false
	}

	cs.sendBuf, cs.writeBuf = cs.writeBuf, cs.sendBuf[:0]

	// Scatter-gather path: a large body was staged via writeBody without
	// being copied into writeBuf. Emit one WRITEV SQE that reads [headers,
	// body] straight to the socket, saving one body-sized memcpy.
	if len(cs.bodyBuf) > 0 {
		sqe := w.ring.GetSQE()
		if sqe == nil {
			cs.writeBuf, cs.sendBuf = cs.sendBuf, cs.writeBuf
			return true
		}
		cs.sendBody = cs.bodyBuf
		cs.bodyBuf = nil
		n := 0
		if len(cs.sendBuf) > 0 {
			cs.iov[n].Base = uintptr(unsafe.Pointer(&cs.sendBuf[0]))
			cs.iov[n].Len = uint64(len(cs.sendBuf))
			n++
		}
		cs.iov[n].Base = uintptr(unsafe.Pointer(&cs.sendBody[0]))
		cs.iov[n].Len = uint64(len(cs.sendBody))
		n++
		prepWritev(sqe, cs.fd, unsafe.Pointer(&cs.iov[0]), n, false)
		if cs.fixedFile {
			setSQEFixedFile(sqe)
		}
		setSQEUserData(sqe, encodeUserDataGen(udSend, cs.fd, cs.generation))
		cs.sending = true
		cs.kernelInflight++
		w.sendsPending = true
		return false
	}

	sqe := w.ring.GetSQE()
	if sqe == nil {
		// SQ ring full — swap back; caller should markDirty.
		cs.writeBuf, cs.sendBuf = cs.sendBuf, cs.writeBuf
		return true
	}
	w.prepSendSQE(sqe, cs, false)
	setSQEUserData(sqe, encodeUserDataGen(udSend, cs.fd, cs.generation))
	cs.sending = true
	cs.kernelInflight++
	w.sendsPending = true
	return false
}

// prepSendSQE prepares a SEND or SEND_ZC SQE based on worker capabilities.
// SEND_ZC is only used for unlinked sends at or above sendZCMinBytes (the
// notification CQE would break the link chain, and on small payloads its extra
// completion costs more than the avoided memcpy). Smaller and linked sends use
// regular SEND.
func (w *Worker) prepSendSQE(sqe unsafe.Pointer, cs *connState, linked bool) {
	if useSendZC(w.sendZC, linked, len(cs.sendBuf)) {
		if cs.fixedFile {
			prepSendZCFixed(sqe, cs.fd, cs.sendBuf, false)
		} else {
			prepSendZC(sqe, cs.fd, cs.sendBuf, false)
		}
	} else if cs.fixedFile {
		prepSendFixed(sqe, cs.fd, cs.sendBuf, linked)
	} else {
		prepSendPlain(sqe, cs.fd, cs.sendBuf, linked)
	}
}

// useSendZC decides whether a send should use zero-copy. ZC is only viable for
// unlinked sends whose payload is large enough that the saved memcpy outweighs
// the extra NOTIF CQE (see sendZCMinBytes).
func useSendZC(sendZC, linked bool, n int) bool {
	return sendZC && !linked && n >= sendZCMinBytes
}

// flushSendLink is like flushSend but links a RECV SQE after the SEND using
// IOSQE_IO_LINK. The kernel chains the operations: when SEND completes, RECV
// starts automatically without another io_uring_enter. This eliminates one
// loop iteration between request/response cycles.
//
// Only used for single-shot recv (bufRing == nil) on the normal request path.
// Falls back to plain (unlinked) SEND if only one SQE slot is available.
func (w *Worker) flushSendLink(cs *connState) bool {
	if cs.sending || cs.zcNotifPending {
		return false
	}

	// Partial send remainder — no linking (RECV may already be in flight).
	if len(cs.sendBuf) > 0 {
		return w.flushSend(cs)
	}

	// Scatter-gather body is staged — bypass the SEND→RECV link chain
	// because IORING_OP_WRITEV carries two iovec entries and the normal
	// link machinery in this function only wires up a single SEND SQE.
	// flushSend handles WRITEV correctly; the re-arm of multishot recv
	// is handled by the handleRecv tail on the next CQE.
	if len(cs.bodyBuf) > 0 {
		return w.flushSend(cs)
	}

	if len(cs.writeBuf) == 0 {
		return false
	}

	cs.sendBuf, cs.writeBuf = cs.writeBuf, cs.sendBuf[:0]

	sqe := w.ring.GetSQE()
	if sqe == nil {
		cs.writeBuf, cs.sendBuf = cs.sendBuf, cs.writeBuf
		return true
	}

	// SEND_ZC cannot be linked (notification CQE breaks the link chain).
	// For linked SEND→RECV, always use regular SEND.
	// Try to get a second SQE for the linked RECV.
	recvSQE := w.ring.GetSQE()
	if recvSQE != nil {
		// Link SEND → RECV (always regular SEND, never ZC).
		if cs.fixedFile {
			prepSendFixed(sqe, cs.fd, cs.sendBuf, true)
		} else {
			prepSendPlain(sqe, cs.fd, cs.sendBuf, true)
		}
		setSQEUserData(sqe, encodeUserDataGen(udSend, cs.fd, cs.generation))
		prepRecv(recvSQE, cs.fd, cs.buf)
		setSQEUserData(recvSQE, encodeUserDataGen(udRecv, cs.fd, cs.generation))
		cs.recvLinked = true
		// The linked recv is a kernel-held op like any prepareRecv arm:
		// count it and mark it armed so the close path cancels it and
		// release waits for its terminal CQE (a failed linked SEND makes
		// the kernel post -ECANCELED for it — still a terminal CQE).
		cs.recvArmed = true
		cs.kernelInflight++
	} else {
		// Only one SQE slot — unlinked send, can use ZC if available.
		w.prepSendSQE(sqe, cs, false)
		setSQEUserData(sqe, encodeUserDataGen(udSend, cs.fd, cs.generation))
	}
	cs.sending = true
	cs.kernelInflight++
	w.sendsPending = true
	return false
}

// addLiveConn records a new active FD in the dense liveConns slice and
// stamps cs.liveIdx with its position so removeLiveConn can find it in
// O(1). Worker-thread-only (the slice is unsynchronised); called from
// onAcceptedFD. (celeris#318 / v1.5.0 review 1.8)
func (w *Worker) addLiveConn(cs *connState) {
	cs.liveIdx = len(w.liveConns)
	w.liveConns = append(w.liveConns, cs.fd)
}

// removeLiveConn removes cs's FD from liveConns in O(1) by swapping the
// last entry into cs.liveIdx and truncating. The swapped-in connState's
// liveIdx is updated to its new position. Worker-thread-only.
// (celeris#318 / v1.5.0 review 1.8)
//
// Callers MUST pass the live connState (not look it up via w.conns[fd]),
// because the close paths nil w.conns[fd] around this call. The defensive
// guards below make a stale / double call a no-op rather than corrupting
// the slice.
func (w *Worker) removeLiveConn(cs *connState) {
	if cs == nil {
		return
	}
	i := cs.liveIdx
	if i < 0 || i >= len(w.liveConns) || w.liveConns[i] != cs.fd {
		// Not actually in liveConns at the recorded index (already removed,
		// never added, or hijacked). Don't touch the slice.
		return
	}
	n := len(w.liveConns) - 1
	if i != n {
		swappedFD := w.liveConns[n]
		w.liveConns[i] = swappedFD
		// Update the swapped-in element's recorded index. Guard against a
		// niled conns slot (shouldn't happen for a live entry, but keeps
		// this robust against teardown ordering).
		if swappedFD >= 0 && swappedFD < len(w.conns) {
			if scs := w.conns[swappedFD]; scs != nil {
				scs.liveIdx = i
			}
		}
	}
	w.liveConns[n] = 0
	w.liveConns = w.liveConns[:n]
	cs.liveIdx = -1
}

// checkTimeouts scans active connections and closes any that have exceeded
// their configured timeout. Called every 1024 iterations (~100ms). This
// replaces the timer wheel: instead of allocating entries and updating maps
// on every recv/send, we store a single lastActivity timestamp on the
// connState and scan here.
//
// Iterates the dense liveConns slice (celeris#318) rather than the sparse
// 0..maxFD range. The previous O(maxFD) scan was the dominant cost above
// ~8 Ki conns on a 16-core box; with liveConns the scan cost is O(active
// conns) regardless of FD space.
func (w *Worker) checkTimeouts() {
	now := time.Now().UnixNano()
	// Drain the deferred-release queue here too. drainPendingRelease gates on
	// w.cachedNow, which is otherwise only refreshed inside the CQE-processing
	// block; on a fully idle worker (no CQEs) cachedNow never advances and
	// closed connStates would be pinned indefinitely. checkTimeouts runs on
	// the ~100ms idle cadence, so refreshing cachedNow and draining here
	// bounds the hold to the wall-clock window even with zero CQE traffic.
	// NOTE: the enqueue-time stamp stays a fresh time.Now()+hold (see
	// queuePendingRelease) so the hold window remains >= cancellation latency
	// regardless of cachedNow staleness (v1.5.0 review 2.9).
	w.cachedNow = now
	if len(w.pendingRelease) > 0 {
		w.drainPendingRelease()
	}
	// Iterate in REVERSE by index: closeConn → finishClose → removeLiveConn
	// swap-removes the FD with the last live entry and shrinks the slice. A
	// forward `range` would then (a) skip the swapped-in conn (moved into an
	// already-visited slot) and (b) read a stale/zeroed tail slot as fd 0,
	// dereferencing w.conns[0]. Reverse-by-index visits each conn exactly
	// once even as entries are swap-removed from the tail (v1.5.0 review 1.9).
	for i := len(w.liveConns) - 1; i >= 0; i-- {
		fd := w.liveConns[i]
		cs := w.conns[fd]
		if cs == nil || cs.closing {
			continue
		}
		// Detached connections (e.g. WebSocket): honor an explicit deadline
		// supplied by the middleware via SetWSIdleDeadline. Skip the
		// engine-config-driven timeouts since the middleware owns the
		// I/O lifecycle. Async-mode conns set detachMu up front without
		// a real detach — fall through to the normal timeout scan for
		// those.
		if cs.h1State != nil && cs.h1State.Detached.Load() {
			if dl := cs.h1State.IdleDeadlineNs.Load(); dl > 0 && now > dl {
				w.closeConn(fd)
			}
			continue
		}
		// ReadHeaderTimeout: slowloris defence. Plain unix.Close path
		// (handled by closeConn → finishCloseDetached fastClose branch).
		// See handleHeaderTimer for the rationale (mirrors net/http).
		if cs.h1State != nil {
			if dl := cs.h1State.HeaderDeadlineNs.Load(); dl > 0 && now > dl {
				w.closeConn(fd)
				continue
			}
		}
		elapsed := time.Duration(now - cs.lastActivity)
		if cs.dirty || cs.sending {
			if w.cfg.WriteTimeout > 0 && elapsed > w.cfg.WriteTimeout {
				w.closeConn(fd)
			}
		} else {
			if w.cfg.IdleTimeout > 0 && elapsed > w.cfg.IdleTimeout {
				w.closeConn(fd)
			} else if w.cfg.ReadTimeout > 0 && elapsed > w.cfg.ReadTimeout {
				w.closeConn(fd)
			}
		}
	}
}

func (w *Worker) shutdown() {
	// Fire onClose for every registered driver conn before tearing down
	// ring/listen fd. Otherwise driver callbacks are silently dropped.
	w.shutdownDrivers()
	// Reverse-by-index for the same reason as checkTimeouts (v1.5.0 review
	// 1.9): any teardown path that swap-removes from liveConns must not cause
	// a forward range to skip a swapped-in conn or read a zeroed tail slot
	// as fd 0.
	for i := len(w.liveConns) - 1; i >= 0; i-- {
		fd := w.liveConns[i]
		cs := w.conns[fd]
		if cs == nil {
			continue
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
			cs.detachClosed = true
			if cs.h1State != nil && cs.h1State.OnDetachClose != nil {
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
				conn.CloseH1(cs.h1State)
				cs.detachMu.Unlock()
			} else {
				conn.CloseH1(cs.h1State)
			}
		}
		if cs.h2State != nil {
			conn.CloseH2(cs.h2State)
		}
		if !cs.fixedFile {
			_ = unix.Close(fd)
		}
		// Do NOT releaseConnState here, detached or not. Conns being torn
		// down by shutdown almost always have a recv SQE armed on cs.buf,
		// and the ring is only closed AFTER this loop — recycling cs into
		// the shared connStatePool now could hand cs.buf to a conn on a
		// still-running sibling worker while this ring's kernel side can
		// still write into it (same #256-class UAF, shutdown variant).
		// The conns remain reachable via w.conns until the Worker itself
		// is collected, well after the ring teardown cancels its ops.
	}
	if w.listenFD >= 0 {
		_ = unix.Close(w.listenFD)
	}
	if w.h2EventFD >= 0 {
		_ = unix.Close(w.h2EventFD)
	}
	if w.bufRing != nil && w.ring != nil {
		w.bufRing.Close(w.ring)
	}
	if w.ring != nil {
		_ = w.ring.Close()
	}
	// Join dispatch goroutines; they've been signaled via
	// asyncClosed + Broadcast above. Prevents stale-memory races
	// after the engine claims to have stopped.
	w.asyncWG.Wait()
}

// drainRecvMaxReads bounds drainRecvBuffer so a peer that keeps trickling bytes
// on a half-closed connection cannot pin the worker. 64 × 512 B = 32 KiB is far
// more than any legitimate post-shutdown tail (a queued GOAWAY / WS close echo).
const drainRecvMaxReads = 64

// drainRecvBuffer non-blockingly reads and discards whatever is already in the
// socket receive buffer, so the following close() will not send an RST that
// discards unsent data still staged in the send buffer (a queued GOAWAY / WS
// close frame).
//
// It MUST NOT block. It runs on the io_uring event-loop worker thread and the
// socket is in blocking mode, so a plain unix.Read here waits for data or FIN
// that may never arrive on a churn-closed / half-closed detached connection —
// wedging the worker, and with enough concurrent detached-closes the whole
// engine (it accepts connections and then services nothing). See celeris#311.
// MSG_DONTWAIT makes each read return EAGAIN the moment the buffer is empty.
func drainRecvBuffer(fd int) {
	var buf [512]byte
	for i := 0; i < drainRecvMaxReads; i++ {
		n, _, err := unix.Recvfrom(fd, buf[:], unix.MSG_DONTWAIT)
		if n <= 0 || err != nil {
			return
		}
	}
}

func createListenSocket(addr string) (int, error) {
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
		return -1, fmt.Errorf("SO_REUSEADDR: %w", err)
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("SO_REUSEPORT: %w", err)
	}

	// TCP_DEFER_ACCEPT: kernel holds connections until data arrives,
	// eliminating wasted accept+wait cycles for idle connections.
	_ = unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_DEFER_ACCEPT, 1)
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

func boundAddr(fd int) net.Addr {
	sa, err := unix.Getsockname(fd)
	if err != nil {
		return nil
	}
	switch v := sa.(type) {
	case *unix.SockaddrInet4:
		return &net.TCPAddr{IP: v.Addr[:], Port: v.Port}
	case *unix.SockaddrInet6:
		return &net.TCPAddr{IP: v.Addr[:], Port: v.Port, Zone: fmt.Sprintf("%d", v.ZoneId)}
	}
	return nil
}

// sockaddrString formats a peer address as "ip:port" (IPv4) or
// "[ip]:port" (IPv6). It runs on the accept hot path (once per OnConnect),
// so it avoids the fmt.Sprintf reflection/allocation cost: the IPv4 path
// builds the dotted-quad + port directly into a stack buffer with
// strconv.AppendInt and the IPv6 path appends net.IP's canonical form
// (the one remaining short-lived allocation) without fmt. See v1.5.0
// review 2.11.
func sockaddrString(sa unix.Sockaddr) string {
	switch v := sa.(type) {
	case *unix.SockaddrInet4:
		// Max "255.255.255.255:65535" = 21 bytes.
		var b [21]byte
		buf := b[:0]
		buf = strconv.AppendInt(buf, int64(v.Addr[0]), 10)
		buf = append(buf, '.')
		buf = strconv.AppendInt(buf, int64(v.Addr[1]), 10)
		buf = append(buf, '.')
		buf = strconv.AppendInt(buf, int64(v.Addr[2]), 10)
		buf = append(buf, '.')
		buf = strconv.AppendInt(buf, int64(v.Addr[3]), 10)
		buf = append(buf, ':')
		buf = strconv.AppendInt(buf, int64(v.Port), 10)
		return string(buf)
	case *unix.SockaddrInet6:
		ip := net.IP(v.Addr[:]).String()
		buf := make([]byte, 0, len(ip)+8)
		buf = append(buf, '[')
		buf = append(buf, ip...)
		buf = append(buf, ']', ':')
		buf = strconv.AppendInt(buf, int64(v.Port), 10)
		return string(buf)
	}
	return ""
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
	}

	if host == "" || host == "0.0.0.0" {
		return &unix.SockaddrInet4{Port: port}, nil
	}

	// IPv6 addresses
	if host == "::" {
		return &unix.SockaddrInet6{Port: port}, nil
	}
	ip := net.ParseIP(host)
	if ip != nil {
		if ip6 := ip.To16(); ip6 != nil && ip.To4() == nil {
			sa := &unix.SockaddrInet6{Port: port}
			copy(sa.Addr[:], ip6)
			return sa, nil
		}
	}

	sa := &unix.SockaddrInet4{Port: port}
	parts := [4]byte{}
	partIdx := 0
	val := 0
	for _, c := range host {
		if c == '.' {
			if partIdx >= 3 {
				return nil, fmt.Errorf("invalid addr: %s", addr)
			}
			parts[partIdx] = byte(val)
			partIdx++
			val = 0
		} else if c >= '0' && c <= '9' {
			val = val*10 + int(c-'0')
		} else {
			return nil, fmt.Errorf("invalid addr: %s", addr)
		}
	}
	parts[partIdx] = byte(val)
	sa.Addr = parts
	return sa, nil
}
