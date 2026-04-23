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
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/goceleris/celeris/engine"
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

// bufRingCount is the number of buffers in the provided buffer ring.
// Must be a power of 2. Only used when multishot recv is opted into via
// CELERIS_IOURING_MULTISHOT_RECV=1; the default single-shot-recv path
// uses per-connection buffers and ignores this ring entirely. 1024
// supports 1024 concurrent in-flight multishot recvs per worker
// (128 was too small for the 1500+ conn churn test patterns —
// ENOBUFS terminations kept burning the per-conn re-arm path).
// With 8 KB buffers: 1024 × 8 KB = 8 MB per worker.
const bufRingCount = 1024

// Worker is a per-core io_uring event loop.
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

	// asyncWG tracks runAsyncHandler goroutines so graceful shutdown
	// can Wait on them before returning. See engine/epoll/loop.go
	// for rationale — keeps dispatch Gs from touching connState
	// memory after the engine claims to have stopped.
	asyncWG      sync.WaitGroup
	conns        []*connState
	connCount    int // number of active connections (local, for draining check)
	maxFD        int // upper bound fd for iteration in checkTimeouts/shutdown
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

	reqCount    *atomic.Uint64
	activeConns *atomic.Int64
	errCount    *atomic.Uint64
	reqBatch    uint64 // batched request count, flushed to reqCount per iteration

	tickCounter uint32
	cachedNow   int64 // cached time.Now().UnixNano(), refreshed every 64 iterations

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
	acceptPaused *atomic.Bool) (*Worker, error) { //nolint:unparam // error return used by callers for future fallible init

	// Listen socket creation is deferred to run() (after CPU pinning and NUMA
	// binding) so that the kernel allocates socket internal buffers on the
	// worker's NUMA node. This eliminates cross-socket access for accept
	// queue operations on multi-socket systems.

	return &Worker{
		id:           id,
		cpuID:        cpuID,
		listenFD:     -1,
		h2EventFD:    -1,
		tier:         tier,
		sqpoll:       tier.SQPollIdle() > 0,
		sendZC:       tier.SupportsSendZC(),
		async:        cfg.AsyncHandlers,
		conns:        make([]*connState, fixedFileTableSize),
		handler:      handler,
		resolved:     resolved,
		cfg:          cfg,
		logger:       cfg.Logger,
		reqCount:     reqCount,
		activeConns:  activeConns,
		errCount:     errCount,
		acceptPaused: acceptPaused,
		wake:         make(chan struct{}),
		ready:        make(chan error, 1),
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
	if w.tier.SupportsMultishotRecv() && os.Getenv("CELERIS_IOURING_MULTISHOT_RECV") == "1" {
		br, err := NewBufferRing(w.ring, bufRingGroupID, bufRingCount, w.resolved.BufferSize)
		if err != nil {
			w.logger.Warn("ring-mapped buffer registration failed, using per-connection buffers",
				"worker", w.id, "err", err)
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
		paused := w.acceptPaused.Load()
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
				// udAccept needs it). Hot ops (recv/send) are checked first.
				switch decodeOp(entry.UserData) {
				case udRecv:
					w.handleRecv(entry, decodeFD(entry.UserData), now)
				case udSend:
					w.handleSend(entry, decodeFD(entry.UserData), now)
				case udAccept:
					w.handleAccept(ctx, entry, decodeFD(entry.UserData), now)
				case udClose:
					w.handleClose(decodeFD(entry.UserData))
				case udH2Wakeup:
					w.handleH2Wakeup()
				case udProvide:
				case udDriverRecv:
					w.handleDriverRecv(entry, decodeFD(entry.UserData))
				case udDriverSend:
					w.handleDriverSend(entry, decodeFD(entry.UserData))
				case udDriverClose:
					w.handleDriverClose(decodeFD(entry.UserData))
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
					if w.prepareRecv(cs.fd, cs.buf) {
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
		// iterations (~100ms under load); when detached conns exist with
		// idle deadlines the gate tightens to every 32 iterations
		// (~50ms idle wall time) so the WS idle-close fires within its
		// configured budget.
		w.tickCounter++
		gate := uint32(0x3FF)
		if w.detachedCount > 0 {
			gate = 0x1F
		}
		if w.tickCounter&gate == 0 {
			w.checkTimeouts()
		}

		// DRAINING → SUSPENDED: no listen socket, no connections, CQEs processed.
		// Checked after CQE processing so accept CQEs for connections that
		// completed before the listen socket close are served, not leaked.
		if w.listenFD < 0 && w.connCount == 0 && w.acceptPaused.Load() {
			w.wakeMu.Lock()
			if !w.acceptPaused.Load() {
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

func (w *Worker) processCQE(ctx context.Context, c *completionEntry, now int64) {
	op := decodeOp(c.UserData)
	fd := decodeFD(c.UserData)

	switch op {
	case udRecv:
		w.handleRecv(c, fd, now)
	case udSend:
		w.handleSend(c, fd, now)
	case udClose:
		w.handleClose(fd)
	case udAccept:
		w.handleAccept(ctx, c, fd, now)
	case udH2Wakeup:
		w.handleH2Wakeup()
	case udProvide:
	case udDriverRecv:
		w.handleDriverRecv(c, fd)
	case udDriverSend:
		w.handleDriverSend(c, fd)
	case udDriverClose:
		w.handleDriverClose(fd)
	}
}

// adaptiveTimeout returns a wait timeout that scales with idle duration.
// Under load (CQEs arriving), returns 1ms for minimal latency. During idle
// periods, backs off to reduce syscall overhead (up to 100ms). When the
// listen socket is closed (draining), uses 1s. When H2 connections exist
// without eventfd, uses 100us for write queue polling. When detached
// conns exist with idle deadlines, the cap drops to 50ms so checkTimeouts
// can fire the WS-supplied IdleDeadline before its expiry.
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
		if w.listenFD >= 0 && !w.tier.SupportsMultishotAccept() {
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
	// upgrades too", so route it through detectProtocol (like Auto) on the
	// first recv rather than locking cs.protocol=H2C on accept. Without
	// this, the first HTTP/1.1 upgrade request was fed to ProcessH2 and
	// the PRI-preface check silently failed, leaving the client with 27
	// bytes of server SETTINGS frame and no 101 Switching Protocols
	// response.
	if w.cfg.Protocol != engine.Auto &&
		(w.cfg.Protocol != engine.H2C || !w.cfg.EnableH2Upgrade) {
		cs.protocol = w.cfg.Protocol
		cs.detected = true
		w.initProtocol(cs)
	}
	if !w.prepareRecv(newFD, cs.buf) {
		cs.needsRecv = true
		w.markDirty(cs)
	}
}

// detectProtocol performs protocol detection on the first received bytes.
// Returns true if detection succeeded and the data should be processed.
func (w *Worker) detectProtocol(cs *connState, data []byte) bool {
	proto, err := detect.Detect(data)
	if err != nil {
		if err == detect.ErrInsufficientData {
			// Need more data — re-arm recv. The data is already in cs.buf
			// so we don't lose it; the next recv appends after it.
			return false
		}
		return false
	}
	cs.protocol = proto
	cs.detected = true
	w.initProtocol(cs)
	return true
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
	w.conns[fd] = nil
	w.connCount--
	w.activeConns.Add(-1)
	releaseConnState(cs)
	f := os.NewFile(uintptr(fd), "tcp")
	c, err := net.FileConn(f)
	_ = f.Close()
	return c, err
}

func (w *Worker) initProtocol(cs *connState) {
	switch cs.protocol {
	case engine.HTTP1:
		cs.h1State = conn.NewH1State()
		cs.h1State.RemoteAddr = cs.remoteAddr
		cs.h1State.MaxRequestBodySize = w.cfg.MaxRequestBodySize
		cs.h1State.OnExpectContinue = w.cfg.OnExpectContinue
		cs.h1State.EnableH2Upgrade = w.cfg.EnableH2Upgrade
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
			cs.h1State.Detached = true
			// Async mode may have already allocated detachMu in
			// acquireConnState; reuse it so the async goroutine and the
			// middleware goroutine share one mutex. Otherwise create
			// a fresh mutex for the WS/SSE detach flow.
			mu := cs.detachMu
			if mu == nil {
				mu = &sync.Mutex{}
				cs.detachMu = mu
			}
			w.detachedCount++
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
			if !w.h2PollArmed && w.h2EventFD >= 0 {
				w.prepareH2Poll()
				w.h2PollArmed = true
			}
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
	cs.protocol = engine.H2C

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
				if !w.prepareRecv(fd, cs.buf) {
					cs.needsRecv = true
					w.markDirty(cs)
				}
			}
			return
		}
		// Surface read failure to detached middleware before closing.
		if cs.detachMu != nil && cs.h1State != nil && cs.h1State.OnError != nil {
			cs.detachMu.Lock()
			if cs.h1State.OnError != nil {
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
				if !w.prepareRecv(fd, w.pickRecvTarget(cs)) {
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
			if !w.prepareRecv(fd, w.pickRecvTarget(cs)) {
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
		if !w.detectProtocol(cs, data) {
			// Need more data or unknown protocol — re-arm recv.
			if hasProvidedBuf {
				// Early return: publish immediately since we're skipping the
				// normal CQE drain loop's batch publish (P0).
				w.bufRing.ReturnBuffer(providedBufID)
			}
			if !cqeHasMore(c.Flags) {
				if !w.prepareRecv(fd, cs.buf) {
					cs.needsRecv = true
					w.markDirty(cs)
				}
			}
			return
		}
	}

	// Async handler dispatch (Config.AsyncHandlers on HTTP1): hand the
	// received bytes to a per-conn goroutine and return to the CQE drain
	// immediately. The goroutine runs ProcessH1 under cs.detachMu and
	// enqueues on detachQueue so this worker submits SEND SQEs on its
	// own goroutine (SINGLE_ISSUER). Mirrors the epoll W3 shape.
	if w.async && cs.protocol == engine.HTTP1 {
		cs.asyncInMu.Lock()
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
		if !cqeHasMore(c.Flags) && !cs.recvPaused {
			if !w.prepareRecv(fd, cs.buf) {
				cs.needsRecv = true
				w.markDirty(cs)
			}
		}
		return
	}

	var processErr error
	switch cs.protocol {
	case engine.HTTP1:
		processErr = conn.ProcessH1(cs.ctx, data, cs.h1State, w.handler, cs.writeFn)
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
				if !w.prepareRecv(fd, cs.buf) {
					cs.needsRecv = true
					w.markDirty(cs)
				}
			}
			return
		}
	case engine.H2C:
		processErr = conn.ProcessH2(cs.ctx, data, cs.h2State, w.handler, cs.writeFn, w.h2cfg)
	}

	// Batch-return the provided buffer after processing. The data has been
	// consumed by the protocol handler. Actual publish happens after the CQE
	// drain loop completes (P0).
	if hasProvidedBuf {
		w.bufRing.PushBuffer(providedBufID)
		w.hasBufReturns = true
	}

	w.reqBatch++

	// lastActivity already set above; timeout checked in checkTimeouts.

	if processErr != nil {
		if errors.Is(processErr, conn.ErrHijacked) {
			return // FD already detached
		}
		// Flush pending writes (e.g. error responses) before closing.
		_ = w.flushSend(cs)
		if cs.detachMu != nil && cs.h1State != nil && cs.h1State.OnError != nil {
			cs.detachMu.Lock()
			if cs.h1State.OnError != nil {
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
	if mu := cs.detachMu; mu != nil {
		mu.Lock()
	}
	// Back-pressure: capture pending size inside the lock so concurrent
	// goroutine writes via the guarded writeFn don't race the read.
	pending := len(cs.writeBuf) + len(cs.sendBuf)
	if pending > cs.sendCap() {
		if mu := cs.detachMu; mu != nil {
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
	if mu := cs.detachMu; mu != nil {
		mu.Unlock()
	}

	// For multishot recv, CQE_F_MORE means the kernel will produce more CQEs
	// without needing a new SQE. Only re-arm if multishot ended.
	// For linked SEND→RECV, the RECV is already queued — skip standalone re-arm.
	// Don't re-arm if recv is paused (WebSocket backpressure).
	if !cqeHasMore(c.Flags) && !cs.recvLinked && !cs.recvPaused {
		if !w.prepareRecv(fd, w.pickRecvTarget(cs)) {
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

	// Regular SEND completion (non-ZC path).
	cs.sending = false

	// SEND_ZC EINVAL fallback: kernel does not support the opcode.
	// Disable ZC for this worker and retry the send with regular SEND.
	if c.Res == -22 && w.sendZC {
		w.sendZC = false
		w.logger.Warn("SEND_ZC not supported (EINVAL), falling back to regular SEND",
			"worker", w.id)
		if w.flushSend(cs) {
			w.markDirty(cs)
		}
		return
	}

	if c.Res < 0 {
		w.errCount.Add(1)
		cs.sendBuf = cs.sendBuf[:0]
		if mu := cs.detachMu; mu != nil {
			mu.Lock()
		}
		cs.writeBuf = cs.writeBuf[:0]
		if cs.h1State != nil && cs.h1State.OnError != nil {
			cs.h1State.OnError(errIORingSend(c.Res))
		}
		if mu := cs.detachMu; mu != nil {
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
			if w.prepareRecv(fd, cs.buf) {
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
		if cs.h1State != nil && cs.h1State.Detached && w.detachedCount > 0 {
			w.detachedCount--
		}
	}
	w.removeDirty(cs)
	// Close H1 state unless a real WS/SSE detach handed ownership to a
	// middleware goroutine. Async-mode HTTP1 conns have detachMu set
	// but h1State.Detached is false — we still own H1 state there.
	trulyDetached := detached && cs.h1State != nil && cs.h1State.Detached
	if !trulyDetached {
		if cs.h1State != nil {
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

func (w *Worker) finishClose(fd int) {
	cs := w.conns[fd]
	w.conns[fd] = nil
	w.connCount--
	w.activeConns.Add(-1)

	if w.cfg.OnDisconnect != nil && cs != nil {
		w.cfg.OnDisconnect(cs.remoteAddr)
	}

	// Capture close-path decisions before releaseConnState wipes cs.
	fixedFile := cs != nil && cs.fixedFile
	fastClose := cs != nil && cs.protocol == engine.HTTP1 && cs.h1State != nil && !cs.h1State.Detached
	if cs != nil {
		releaseConnState(cs)
	}

	if fixedFile {
		// Fixed file: close via io_uring direct close (no real FD to shutdown).
		sqe := w.ring.GetSQE()
		if sqe != nil {
			prepCloseDirect(sqe, fd)
			setSQEUserData(sqe, encodeUserData(udClose, fd))
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
	w.conns[fd] = nil
	w.connCount--
	w.activeConns.Add(-1)

	if w.cfg.OnDisconnect != nil {
		w.cfg.OnDisconnect(cs.remoteAddr)
	}

	fixedFile := cs.fixedFile
	// Do NOT call releaseConnState — goroutine closures still reference cs.

	if fixedFile {
		sqe := w.ring.GetSQE()
		if sqe != nil {
			prepCloseDirect(sqe, fd)
			setSQEUserData(sqe, encodeUserData(udClose, fd))
		}
		_ = w.ring.UpdateFixedFile(fd, -1)
		return
	}

	// Detached connections still use the graceful half-close path —
	// middleware (WebSocket / SSE) may have queued a close-frame echo
	// that we want to let finish cleanly, and the middleware goroutine
	// already drove the protocol handshake to completion before asking
	// the engine to drop the FD. RST close (as used by closeNonFixedFD
	// for plain H1/H2) would cut that echo off. The graceful path is
	// still SYNC (unix.Close directly) to avoid the async-SQE pile-up
	// that plagued the pre-patch version.
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

		cs.detachMu.Lock()
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

// prepareRecv submits a recv SQE. Uses multishot recv with ring-mapped provided
// buffers when available; falls back to single-shot per-connection buffer recv.
// Returns true if the SQE was submitted, false if the SQ ring was full.
func (w *Worker) prepareRecv(fd int, buf []byte) bool {
	sqe := w.ring.GetSQE()
	if sqe == nil {
		return false
	}
	if w.bufRing != nil {
		prepMultishotRecv(sqe, fd, bufRingGroupID, w.fixedFiles)
	} else {
		prepRecv(sqe, fd, buf)
	}
	setSQEUserData(sqe, encodeUserData(udRecv, fd))
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
	// Async mode: the dispatch goroutine owns h1State; the worker cannot
	// safely observe NextRecvBuf without synchronization. Always use
	// cs.buf so the goroutine handles body accumulation on its side.
	if w.async || w.bufRing != nil || cs.protocol != engine.HTTP1 || cs.h1State == nil {
		return cs.buf
	}
	if b := cs.h1State.NextRecvBuf(); b != nil {
		cs.recvIntoBody = true
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
					prepCancelFDSkipSuccess(sqe, cs.fd)
					setSQEUserData(sqe, encodeUserData(udProvide, cs.fd))
				}
			} else {
				if w.prepareRecv(cs.fd, cs.buf) {
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
		setSQEUserData(sqe, encodeUserData(udSend, cs.fd))
		cs.sending = true
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
		setSQEUserData(sqe, encodeUserData(udSend, cs.fd))
		cs.sending = true
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
	setSQEUserData(sqe, encodeUserData(udSend, cs.fd))
	cs.sending = true
	w.sendsPending = true
	return false
}

// prepSendSQE prepares a SEND or SEND_ZC SQE based on worker capabilities.
// SEND_ZC is only used for unlinked sends (the notification CQE would break
// the link chain). Linked sends always use regular SEND.
func (w *Worker) prepSendSQE(sqe unsafe.Pointer, cs *connState, linked bool) {
	if w.sendZC && !linked {
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
		setSQEUserData(sqe, encodeUserData(udSend, cs.fd))
		prepRecv(recvSQE, cs.fd, cs.buf)
		setSQEUserData(recvSQE, encodeUserData(udRecv, cs.fd))
		cs.recvLinked = true
	} else {
		// Only one SQE slot — unlinked send, can use ZC if available.
		w.prepSendSQE(sqe, cs, false)
		setSQEUserData(sqe, encodeUserData(udSend, cs.fd))
	}
	cs.sending = true
	w.sendsPending = true
	return false
}

// checkTimeouts scans active connections and closes any that have exceeded
// their configured timeout. Called every 1024 iterations (~100ms). This
// replaces the timer wheel: instead of allocating entries and updating maps
// on every recv/send, we store a single lastActivity timestamp on the
// connState and scan here.
func (w *Worker) checkTimeouts() {
	now := time.Now().UnixNano()
	for fd := 0; fd <= w.maxFD; fd++ {
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
		if cs.h1State != nil && cs.h1State.Detached {
			if dl := cs.h1State.IdleDeadlineNs.Load(); dl > 0 && now > dl {
				w.closeConn(fd)
			}
			continue
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
	for fd := 0; fd <= w.maxFD; fd++ {
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
		trulyDetached := detached && cs.h1State != nil && cs.h1State.Detached
		if !trulyDetached && cs.h1State != nil {
			conn.CloseH1(cs.h1State)
		}
		if cs.h2State != nil {
			conn.CloseH2(cs.h2State)
		}
		if !cs.fixedFile {
			_ = unix.Close(fd)
		}
		if !detached {
			releaseConnState(cs)
		}
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

// drainRecvBuffer reads and discards any data in the socket receive buffer.
// This prevents close() from sending RST (which discards unsent data like GOAWAY).
func drainRecvBuffer(fd int) {
	var buf [512]byte
	for {
		n, _ := unix.Read(fd, buf[:])
		if n <= 0 {
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

	if err := unix.Bind(fd, sa); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("bind: %w", err)
	}
	if err := unix.Listen(fd, 4096); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("listen: %w", err)
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

func sockaddrString(sa unix.Sockaddr) string {
	switch v := sa.(type) {
	case *unix.SockaddrInet4:
		return fmt.Sprintf("%s:%d", net.IP(v.Addr[:]), v.Port)
	case *unix.SockaddrInet6:
		return fmt.Sprintf("[%s]:%d", net.IP(v.Addr[:]), v.Port)
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
