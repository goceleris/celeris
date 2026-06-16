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
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/engine/internal/bindiag"
	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/ctxkit"
	"github.com/goceleris/celeris/internal/platform"
	"github.com/goceleris/celeris/internal/sockopts"
	"github.com/goceleris/celeris/protocol/detect"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// connTableSize is the number of slots in the flat connection array, and
// thus the HARD CAP on concurrent connections per worker: an accepted fd
// >= connTableSize cannot be indexed into l.conns and is rejected (closed)
// by acceptAll with an errCount bump + a rate-limited Warn. The kernel
// hands out the lowest free fd, so this is reached only when a worker is
// genuinely holding ~65 K live conns (plus the listen/epoll/event/timer
// fds). Raise this if a single worker must sustain more.
const connTableSize = 65536

// errPeerClosed is returned via H1State.OnError when the peer closes the
// connection cleanly (read returns 0 bytes / EOF).
var errPeerClosed = errors.New("celeris: peer closed connection")

// Loop is an epoll-based event loop worker.
type Loop struct {
	id        int
	cpuID     int
	epollFD   int
	listenFD  int
	eventFD   int // eventfd for H2 write queue wakeup (-1 if unavailable)
	timerFD   int // timerfd for kernel-enforced checkTimeouts cadence (-1 if disabled)
	events    []unix.EpollEvent
	conns     []*connState
	connCount int // number of active connections (local, for draining check)
	maxFD     int // upper bound fd for iteration in checkTimeouts/shutdown
	// liveConns is a dense slice of currently-active FDs. checkTimeouts
	// and shutdown iterate it instead of the sparse 0..maxFD range, so
	// their cost is O(active conns) regardless of the FD space (maxFD
	// never shrinks). Maintained by addLiveConn/removeLiveConn on every
	// accept/close path. Worker-thread-only — no synchronization. (#318)
	liveConns []int
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
	// inactive is the per-worker pause flag used by the dynamic worker
	// scaler. ORed with acceptPaused (which is engine-wide) when computing
	// effective paused state. The scaler flips this to deactivate idle
	// loops under low load and reactivate them under burst load.
	inactive atomic.Bool
	// listenFDClosed signals that the loop has closed its listen FD in
	// response to acceptPaused=true. PauseAccept polls this so it only
	// returns once the SO_REUSEPORT group has actually shed this listener
	// — important for the adaptive engine, where the standby's listen
	// sockets must be out of the kernel routing pool before the bound
	// address is exposed to the caller (otherwise the first burst of
	// dials can land on the standby and get RST when it pauses).
	// Reset to false in ResumeAccept so a Pause→Resume→Pause cycle
	// re-arms the signal.
	listenFDClosed atomic.Bool

	reqCount         *atomic.Uint64
	activeConns      *atomic.Int64
	errCount         *atomic.Uint64
	asyncPromoted    *atomic.Uint64 // cumulative inline → dispatch promotions (#300)
	reqBatch         uint64         // batched request count, flushed to reqCount per iteration
	tickCounter      uint32
	consecutiveEmpty uint32 // consecutive iterations with no events (for adaptive timeout)
	cachedNow        int64  // cached time.Now().UnixNano(), refreshed once per events return

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

	// asyncWG tracks runAsyncHandler goroutines so graceful shutdown
	// can Wait on them before returning. Without this the engine's
	// top-level wg.Wait joins only the worker/Listen goroutines, and
	// dispatch Gs can still be in ProcessH1 against released state
	// when shutdown returns.
	asyncWG sync.WaitGroup

	// Driver integration (EventLoopProvider). The hasDriverConns gate is the
	// ONLY check the HTTP hot path pays when no drivers are registered; it
	// must stay an atomic.Bool load, not a map read.
	driverConns    map[int]*driverConn
	driverMu       sync.RWMutex
	hasDriverConns atomic.Bool
	driverReadBuf  []byte // scratch buffer for driver EPOLLIN drains (worker-local)

	// async dispatches HTTP1 handlers to spawned goroutines. Set by
	// Config.AsyncHandlers (the canonical server-level default) or
	// auto-enabled in doPrepare when the router has any .Async() routes.
	// Per-route .Async()/.Async(false) overrides further refine dispatch
	// per request via the InlineMode → ErrAsyncDispatch handoff. Checked
	// on every drainRead; keep it a plain bool so the no-async path is a
	// single mov+test.
	async bool
}

func newLoop(id, cpuID int, handler stream.Handler,
	resolved resource.ResolvedResources,
	cfg resource.Config, reqCount *atomic.Uint64, activeConns *atomic.Int64, errCount *atomic.Uint64,
	asyncPromoted *atomic.Uint64, acceptPaused *atomic.Bool) *Loop {

	return &Loop{
		id:           id,
		cpuID:        cpuID,
		epollFD:      -1,
		listenFD:     -1,
		eventFD:      -1,
		timerFD:      -1,
		events:       make([]unix.EpollEvent, resolved.MaxEvents),
		conns:        make([]*connState, connTableSize),
		liveConns:    make([]int, 0, 1024),
		handler:      handler,
		resolved:     resolved,
		cfg:          cfg,
		logger:       cfg.Logger,
		acceptPaused: acceptPaused,
		wake:         make(chan struct{}),
		ready:        make(chan error, 1),
		sockOpts: sockopts.Options{
			TCPNoDelay:  true,
			TCPQuickAck: true,
			SOBusyPoll:  50 * time.Microsecond,
			RecvBuf:     resolved.SocketRecv,
			SendBuf:     resolved.SocketSend,
		},
		reqCount:      reqCount,
		activeConns:   activeConns,
		errCount:      errCount,
		asyncPromoted: asyncPromoted,
		h2cfg: conn.H2Config{
			MaxConcurrentStreams: cfg.MaxConcurrentStreams,
			InitialWindowSize:    cfg.InitialWindowSize,
			MaxFrameSize:         cfg.MaxFrameSize,
			MaxRequestBodySize:   cfg.MaxRequestBodySize,
		},
		async: cfg.AsyncHandlers,
	}
}

func (l *Loop) run(ctx context.Context) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	_ = platform.PinToCPU(l.cpuID)

	numaNode := platform.CPUForNode(l.cpuID)
	if err := platform.BindNumaNode(numaNode); err == nil {
		defer func() { _ = platform.ResetNumaPolicy() }()
	}

	epollFD, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		l.ready <- fmt.Errorf("loop %d: epoll_create1: %w", l.id, err)
		return
	}
	l.epollFD = epollFD

	listenFD, err := createListenSocket(l.cfg.Addr)
	if err != nil {
		_ = unix.Close(epollFD)
		l.ready <- fmt.Errorf("loop %d: listen socket: %w", l.id, err)
		return
	}
	l.listenFD = listenFD

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
	l.eventFD = efd

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
			// 25ms periodic; first fire at 25ms too.
			spec := unix.ItimerSpec{
				Interval: unix.Timespec{Sec: 0, Nsec: 25 * 1000 * 1000},
				Value:    unix.Timespec{Sec: 0, Nsec: 25 * 1000 * 1000},
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

	l.ready <- nil

	activeTimeoutMs := 1 // 1ms default epoll timeout
	l.cachedNow = time.Now().UnixNano()

	for {
		if ctx.Err() != nil {
			l.shutdown()
			return
		}

		// Cache the atomic load: ACTIVE→DRAINING and SUSPENDED→ACTIVE
		// branches both read it. Saves 1 atomic load per event-loop
		// iteration on the steady-state hot path.
		// OR with the per-loop inactive flag (dynamic scaler).
		paused := l.acceptPaused.Load() || l.inactive.Load()
		if l.listenFD >= 0 && paused {
			// Drain any pending accepts from the kernel's listen queue so
			// they get a clean shutdown (FIN) rather than the RST that
			// close() of the listen FD would send. Loadgen's H2 dial
			// retries handle FIN gracefully; an RST aborts the whole
			// benchmark.
			for {
				connFD, _, accErr := unix.Accept4(l.listenFD, unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)
				if accErr != nil {
					break
				}
				_ = unix.Shutdown(connFD, unix.SHUT_RDWR)
				_ = unix.Close(connFD)
			}
			_ = unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_DEL, l.listenFD, nil)
			_ = unix.Close(l.listenFD)
			l.listenFD = -1
			// No listen fd to drain — clear the re-arm flag so it doesn't
			// pin epoll_wait at 0ms (busy-spin) while paused/suspended.
			l.listenHot = false
		}
		// Maintain the listenFDClosed signal that PauseAccept polls on.
		// Set it whenever paused==true regardless of whether we just
		// closed the FD or it was already -1 from a prior Pause-Resume
		// cycle that hadn't re-created the socket yet (race window:
		// ResumeAccept clears the flag and toggles paused=false; the
		// worker may be mid-iteration when paused flips back to true,
		// so listenFD can transiently be -1 while paused is true). Also
		// clear the flag when paused goes false so a subsequent Pause
		// observes a fresh signal once the new listenFD is created.
		if paused {
			l.listenFDClosed.Store(true)
		} else {
			l.listenFDClosed.Store(false)
		}

		// SUSPENDED → ACTIVE: re-create listen socket after ResumeAccept.
		if l.listenFD < 0 && !paused {
			fd, err := createListenSocket(l.cfg.Addr)
			if err != nil {
				l.logger.Error("re-create listen socket", "loop", l.id, "err", err)
				l.shutdown()
				return
			}
			if err := unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_ADD, fd, &unix.EpollEvent{
				Events: unix.EPOLLIN | unix.EPOLLET,
				Fd:     int32(fd),
			}); err != nil {
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
		// acceptAll stopped with the listen backlog possibly non-empty
		// (per-call cap hit, or EMFILE/ENFILE back-off). The listen socket
		// is edge-triggered, so don't block — poll immediately and re-drain.
		if l.listenHot {
			timeoutMs = 0
		}

		n, err := unix.EpollWait(l.epollFD, l.events, timeoutMs)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			l.logger.Error("epoll_wait error", "loop", l.id, "err", err)
			continue
		}

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
			if fd == l.eventFD && l.eventFD >= 0 {
				var buf [8]byte
				_, _ = unix.Read(l.eventFD, buf[:])
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

			// Driver fast-path: single atomic load when no drivers are
			// registered (zero-cost for pure-HTTP workloads). The map
			// lookup happens only when the gate is true.
			if l.hasDriverConns.Load() {
				if dc := l.lookupDriver(fd); dc != nil {
					l.handleDriverEvent(dc, ev.Events)
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
			// so re-validate the slot. Level-triggered EPOLLOUT keeps firing
			// while writable, so a partial flush simply resumes next edge.
			if ev.Events&unix.EPOLLOUT != 0 {
				if fd >= 0 && fd < len(l.conns) {
					if cs := l.conns[fd]; cs != nil && cs.epollOut {
						l.handleWritable(cs)
					}
				}
			}

			if ev.Events&(unix.EPOLLERR|unix.EPOLLHUP) != 0 {
				if fd >= 0 && fd < len(l.conns) && l.conns[fd] != nil {
					l.closeConn(fd)
				}
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

		// Drain detached goroutine writes BEFORE the dirty flush so that
		// data written by goroutines (e.g. WebSocket responses) is flushed
		// in the same event loop iteration.
		l.drainDetachQueue()

		for cs := l.dirtyHead; cs != nil; {
			next := cs.dirtyNext
			if mu := cs.detachMu; mu != nil {
				mu.Lock()
			}
			err := flushWrites(cs)
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
				if mu := cs.detachMu; mu != nil {
					mu.Unlock()
				}
				l.removeDirty(cs)
			} else {
				// Partial write: kernel send buffer full. Sync pendingBytes,
				// then for a non-detached HTTP/H2 conn hand off to level-
				// triggered EPOLLOUT (armEpollOut removes it from the dirty
				// list) so the loop stops busy-retrying it. Truly-detached
				// WS/SSE conns stay on the dirty list — their writes are
				// goroutine-driven and re-signalled via the eventfd path.
				// `next` was captured above, so the removeDirty inside
				// armEpollOut is safe mid-iteration.
				cs.pendingBytes = csPendingBytes(cs)
				detachedWS := cs.h1State != nil && cs.h1State.Detached.Load()
				if mu := cs.detachMu; mu != nil {
					mu.Unlock()
				}
				if !detachedWS {
					l.armEpollOut(cs)
				}
			}
			cs = next
		}

		// Drain H2 async write queues. Handler goroutines enqueue response
		// frame bytes; we drain them into writeBuf and flush to the wire.
		for _, fd := range l.h2Conns {
			cs := l.conns[fd]
			if cs != nil && cs.h2State != nil && cs.h2State.WriteQueuePending() {
				cs.h2State.DrainWriteQueue(cs.writeFn)
				if cs.writePos < len(cs.writeBuf) {
					if fErr := flushWrites(cs); fErr != nil {
						l.removeDirty(cs)
						l.closeConn(fd)
					} else if cs.writePos < len(cs.writeBuf) {
						// Send buffer full: arm EPOLLOUT instead of the dirty
						// list so we don't busy-poll the backpressured H2 conn.
						l.armEpollOut(cs)
					}
				}
			}
		}

		// Flush batched request count to the shared atomic counter. This
		// replaces per-request atomic.Add with one atomic per event loop
		// iteration, eliminating cache-line bouncing under multi-worker
		// contention.
		if l.reqBatch > 0 {
			l.reqCount.Add(l.reqBatch)
			l.reqBatch = 0
		}

		// Check connection timeouts. Default cadence is every 1024 iterations
		// (~100ms under load); when detached conns exist with idle deadlines
		// the gate tightens to every 32 iterations (~50ms idle wall time)
		// so the WS idle-close fires within its configured budget. When
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
			l.checkTimeouts()
		}

		// DRAINING → SUSPENDED: no listen socket, no connections, events processed.
		// Combined paused: engine-wide OR per-loop (dynamic scaler).
		if l.listenFD < 0 && l.connCount == 0 && (l.acceptPaused.Load() || l.inactive.Load()) {
			l.wakeMu.Lock()
			if !l.acceptPaused.Load() && !l.inactive.Load() {
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

// acceptAllCap bounds how many connections acceptAll drains in a single
// call so one worker can't starve its other ready FDs under an accept
// flood. The listen socket is edge-triggered (EPOLLIN|EPOLLET): with ET we
// MUST drain to EAGAIN or we won't get another readiness edge for already-
// queued SYNs. So when the cap is hit with the backlog possibly non-empty
// we set listenHot, which makes the event loop re-enter acceptAll on the
// next iteration with a 0ms epoll_wait — draining the rest without waiting
// for a fresh SYN edge.
const acceptAllCap = 1024

func (l *Loop) acceptAll(ctx context.Context, now int64) {
	for i := 0; i < acceptAllCap; i++ {
		newFD, sa, err := unix.Accept4(l.listenFD, unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)
		if err != nil {
			switch err {
			case unix.EAGAIN:
				// Listen queue fully drained — ET edge satisfied.
				// (EWOULDBLOCK == EAGAIN on Linux, so one case covers both.)
				l.listenHot = false
				return
			case unix.EINTR, unix.ECONNABORTED:
				// Transient: a SYN was aborted (RST before accept) or the
				// syscall was interrupted. The backlog may still hold other
				// connections, so retry this round rather than returning —
				// a bare return would strand them until the next SYN edge.
				continue
			case unix.EMFILE, unix.ENFILE:
				// Out of file descriptors (per-process / system-wide). The
				// SYN at the head of the queue is un-acceptable right now;
				// bare-continuing would busy-loop on the same EMFILE. Back
				// off this round and re-arm so we retry next iteration once
				// an fd may have freed up — with ET we can't just wait for a
				// new edge, the queued SYNs won't re-signal.
				l.errCount.Add(1)
				l.listenHot = true
				return
			default:
				// Unexpected accept error. Count it and stop this round; the
				// listen fd stays armed (ET) and a fresh edge re-enters.
				l.errCount.Add(1)
				l.listenHot = false
				return
			}
		}

		// Bounds check: reject FDs outside the flat conn array. An fd at or
		// above the table cap cannot be indexed into l.conns, so it would
		// be silently dropped (errCount++) and stay invisible. Latch a
		// one-shot Warn so a worker that has saturated its 64 K conn table
		// is diagnosable rather than just "connections vanishing".
		if newFD < 0 || newFD >= len(l.conns) {
			_ = unix.Close(newFD)
			l.errCount.Add(1)
			l.fdCapDrops++
			if !l.fdCapWarned && l.logger != nil {
				l.fdCapWarned = true
				l.logger.Warn("accepted fd exceeds conn table cap; dropping",
					"loop", l.id, "fd", newFD, "cap", len(l.conns),
					"hint", "worker is at its per-worker connection limit (connTableSize)")
			}
			continue
		}

		_ = sockopts.ApplyFD(newFD, l.sockOpts)

		if err := unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_ADD, newFD, &unix.EpollEvent{
			Events: unix.EPOLLIN | unix.EPOLLET,
			Fd:     int32(newFD),
		}); err != nil {
			_ = unix.Close(newFD)
			l.errCount.Add(1)
			continue
		}

		// Tag the per-conn context with this worker's numeric ID so handlers
		// can call celeris.Context.WorkerID() and forward it to driver
		// pools (postgres.WithWorker / redis.WithWorker / memcached.WithWorker)
		// for per-CPU affinity between the HTTP request and any DB/cache
		// calls it makes.
		connCtx := ctxkit.WithWorkerID(ctx, l.id)
		cs := acquireConnState(connCtx, newFD, l.resolved.BufferSize, l.async)
		cs.remoteAddr = sockaddrString(sa)
		l.conns[newFD] = cs
		l.addLiveConn(cs)
		l.connCount++
		if newFD > l.maxFD {
			l.maxFD = newFD
		}
		cs.writeFn = l.makeWriteFn(cs)
		l.activeConns.Add(1)

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
		}
	}
	// Cap hit without reaching EAGAIN: more SYNs may be queued. With ET we
	// won't get a fresh readiness edge for them, so re-arm to drain the
	// remainder on the next loop iteration (epoll_wait(0) via listenHot).
	l.listenHot = true
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
		recvBuf := cs.buf
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
			if mu := cs.detachMu; mu != nil {
				mu.Lock()
			}
			_ = flushWrites(cs)
			// Surface read failure to detached middleware (e.g. WS).
			if cs.h1State != nil && cs.h1State.OnError != nil {
				cs.h1State.OnError(err)
			}
			if mu := cs.detachMu; mu != nil {
				mu.Unlock()
			}
			l.closeConn(fd)
			return
		}
		if n == 0 {
			if mu := cs.detachMu; mu != nil {
				mu.Lock()
			}
			_ = flushWrites(cs)
			// Surface peer-close (EOF) to detached middleware.
			if cs.h1State != nil && cs.h1State.OnError != nil {
				cs.h1State.OnError(errPeerClosed)
			}
			if mu := cs.detachMu; mu != nil {
				mu.Unlock()
			}
			l.closeConn(fd)
			return
		}

		cs.lastActivity = now

		// Direct-into-bodyBuf completion path: we read straight into
		// H1State.bodyBuf; dispatch the handler if the body is full,
		// otherwise continue the recv loop for the next chunk.
		if intoBody {
			complete := cs.h1State.ConsumeBodyRecv(n)
			if !complete {
				continue
			}
			rest, derr := cs.h1State.DispatchBufferedBody(cs.ctx, l.handler, cs.writeFn)
			if errors.Is(derr, conn.ErrUpgradeH2C) {
				if err := l.switchToH2(cs, cs.writeFn); err != nil {
					l.closeConn(fd)
					return
				}
			} else if derr != nil {
				if errors.Is(derr, conn.ErrHijacked) {
					return
				}
				if mu := cs.detachMu; mu != nil {
					mu.Lock()
				}
				_ = flushWrites(cs)
				if mu := cs.detachMu; mu != nil {
					mu.Unlock()
				}
				l.closeConn(fd)
				return
			}
			if len(rest) > 0 {
				// Tail bytes after the body — feed them back through
				// the normal parser for pipelined request(s).
				if perr := conn.ProcessH1(cs.ctx, rest, cs.h1State, l.handler, cs.writeFn); perr != nil {
					if errors.Is(perr, conn.ErrHijacked) {
						return
					}
					if mu := cs.detachMu; mu != nil {
						mu.Lock()
					}
					_ = flushWrites(cs)
					if mu := cs.detachMu; mu != nil {
						mu.Unlock()
					}
					l.closeConn(fd)
					return
				}
			}
			if mu := cs.detachMu; mu != nil {
				mu.Lock()
			}
			if err := flushWrites(cs); err != nil {
				if mu := cs.detachMu; mu != nil {
					mu.Unlock()
				}
				l.closeConn(fd)
				return
			}
			dirty := len(cs.writeBuf)-cs.writePos > 0 || len(cs.bodyBuf) > 0
			if mu := cs.detachMu; mu != nil {
				mu.Unlock()
			}
			if dirty {
				// Send buffer full after the body-recv response flush. This
				// path is sync-only (intoBody is gated on !l.async), so the
				// conn is never detached — arm EPOLLOUT to avoid busy-polling.
				l.armEpollOut(cs)
			}
			continue
		}

		data := cs.buf[:n]

		if !cs.detected {
			proto, detectErr := detect.Detect(data)
			if detectErr == detect.ErrInsufficientData {
				continue
			}
			if detectErr != nil {
				l.closeConn(fd)
				return
			}
			cs.protocol = proto
			cs.detected = true
			l.initProtocol(cs)
		}

		writeFn := cs.writeFn

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
			continue
		}

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
			if tryInline {
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
				continue
			}
			if errors.Is(processErr, conn.ErrUpgradeH2C) {
				// H1→H2 promotion. switchToH2 consumes the upgrade info,
				// installs H2 state, and feeds any preface bytes synchronously.
				if err := l.switchToH2(cs, writeFn); err != nil {
					l.closeConn(fd)
					return
				}
				// Flush the buffered 101 Switching Protocols response and any
				// H2 server preface + stream 1 response bytes that switchToH2
				// may have queued. The normal inline-flush block below is
				// skipped by the `continue` so we must flush explicitly here,
				// otherwise the client blocks forever waiting for the 101.
				if cs.writePos < len(cs.writeBuf) {
					if fErr := flushWrites(cs); fErr != nil {
						l.closeConn(fd)
						return
					}
					if cs.writePos >= len(cs.writeBuf) {
						cs.pendingBytes = 0
						if cs.dirty {
							l.removeDirty(cs)
						}
					} else {
						// 101 + H2 preface didn't fully drain: send buffer
						// full. Newly-H2 conn (not detached) — arm EPOLLOUT
						// rather than the busy-polling dirty list.
						cs.pendingBytes = len(cs.writeBuf) - cs.writePos
						l.armEpollOut(cs)
					}
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
			// fix in engine/iouring/worker.go).
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
			// Flush any pending writes (e.g. error responses) before closing.
			if mu := cs.detachMu; mu != nil {
				mu.Lock()
			}
			_ = flushWrites(cs)
			cs.pendingBytes = 0
			if cs.h1State != nil && cs.h1State.OnError != nil {
				cs.h1State.OnError(processErr)
			}
			if mu := cs.detachMu; mu != nil {
				mu.Unlock()
			}
			l.closeConn(fd)
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
			if fErr := flushWrites(cs); fErr != nil {
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
				if cs.dirty {
					l.removeDirty(cs)
				}
				l.disarmEpollOut(cs)
			} else {
				// Partial write — the kernel send buffer is full (write
				// backpressure). Sync pendingBytes and arm level-triggered
				// EPOLLOUT instead of busy-retrying via the dirty list, which
				// would spin epoll_wait(0)→write(EAGAIN) at 100% CPU. Truly-
				// detached WS/SSE conns keep the goroutine-driven dirty/
				// detachQueue path (their writes flow through guarded writeFn
				// + eventfd, not this inline HTTP flush). A pending sendfile
				// is resumed by handleWritable on the next EPOLLOUT edge.
				cs.pendingBytes = csPendingBytes(cs)
				if cs.h1State != nil && cs.h1State.Detached.Load() {
					l.markDirty(cs)
				} else {
					l.armEpollOut(cs)
				}
			}
		}
		// Capture pendingBytes inside the lock so the check below is safe
		// against concurrent goroutine writes via the guarded writeFn.
		pending := cs.pendingBytes
		if mu := cs.detachMu; mu != nil {
			mu.Unlock()
		}

		if pending > cs.writeCap() {
			l.closeConn(fd)
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
			_ = unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_MOD, cs.fd, &unix.EpollEvent{
				Events: 0,
				Fd:     int32(cs.fd),
			})
			cs.recvPaused = true
			return
		}

		// Short read: socket is provably drained (read returned fewer bytes
		// than the buffer can hold). Skip the EAGAIN-producing read that would
		// otherwise cost one wasted syscall per request. Edge-triggered epoll
		// will notify when new data arrives on this fd.
		if n < len(cs.buf) {
			return
		}
	}
}

func (l *Loop) hijackConn(fd int) (net.Conn, error) {
	cs := l.conns[fd]
	if cs == nil {
		return nil, errors.New("celeris: connection not found")
	}
	// Detach the conn from the engine synchronously: drop it from epoll,
	// the live set, and the conn table, and update the counters. All of
	// this is worker-state that's only safe to touch from this thread —
	// and in async mode hijackConn runs ON the dispatch goroutine (inside
	// ProcessH1 → ErrHijacked). That's tolerable because the worker won't
	// see another EPOLLIN for fd after EPOLL_CTL_DEL, and these l.* fields
	// are only read by the worker between epoll_wait returns, never while a
	// dispatch goroutine is mid-ProcessH1.
	_ = unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_DEL, fd, nil)
	l.removeLiveConn(cs)
	l.conns[fd] = nil
	l.connCount--
	l.activeConns.Add(-1)

	f := os.NewFile(uintptr(fd), "tcp")
	c, err := net.FileConn(f)
	_ = f.Close()

	// CRITICAL (#3.1): defer the pool release whenever a dispatch goroutine
	// (runAsyncHandler) is alive for this conn. When the handler calls
	// Hijack from inside that goroutine's ProcessH1, the goroutine STILL
	// touches cs after ProcessH1 returns ErrHijacked — it Unlocks
	// cs.detachMu, resyncs cs.pendingBytes, sets cs.asyncClosed, clears
	// cs.asyncInBuf, and enqueues cs on detachQueue. Recycling cs now would
	// hand pooled-and-reissued memory to that goroutine. Mark it hijacked
	// and let the worker-thread teardown (drainDetachQueue, reached via the
	// goroutine's asyncClosed + detachQueue handoff) run the pool release
	// once the goroutine has exited.
	//
	// When no dispatch goroutine is running, hijackConn was called inline on
	// the worker thread (sync mode, or an async-mode conn not yet promoted
	// running its first request inline). drainRead returns immediately on
	// ErrHijacked without touching cs again and nothing will enqueue cs, so
	// release synchronously here — gating only on detachMu would leak the
	// connState in the inline-async case. asyncRun is read under asyncInMu
	// (the same lock the dispatch goroutine sets it under).
	deferRelease := false
	if cs.detachMu != nil {
		cs.asyncInMu.Lock()
		deferRelease = cs.asyncRun
		cs.asyncInMu.Unlock()
	}
	if deferRelease {
		cs.hijacked = true
	} else {
		releaseConnState(cs)
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
		cs.h1State.ReadHeaderTimeoutNs = int64(l.cfg.ReadHeaderTimeout)
		cs.h1State.ArmHeaderDeadline()
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
		// Scatter-gather body writer: handler hands large bodies to the
		// engine as a zero-copy slice; flushWrites emits writev(2) with
		// [headers, body] so we save the respBuf → writeBuf memcpy.
		// Disabled in async mode because cs.bodyBuf access would race
		// with the dispatch goroutine without a mutex.
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
				mu.Unlock()
				// Signal the event loop to flush. Do NOT call markDirty
				// from this goroutine — dirtyHead is event-loop-local.
				l.detachQMu.Lock()
				l.detachQueue = append(l.detachQueue, cs)
				l.detachQPending.Store(1)
				l.detachQMu.Unlock()
				if l.eventFD >= 0 {
					var val [8]byte
					val[0] = 1
					_, _ = unix.Write(l.eventFD, val[:])
				}
			}
			cs.writeFn = guarded
			// Also update the response adapter so StreamWriter writes
			// go through the guarded path (not the stale pre-Detach writeFn).
			cs.h1State.UpdateWriteFn(guarded)
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
				l.detachQPending.Store(1)
				l.detachQMu.Unlock()
				if l.eventFD >= 0 {
					var val [8]byte
					val[0] = 1
					_, _ = unix.Write(l.eventFD, val[:])
				}
			}
			cs.h1State.ResumeRecv = func() {
				if !cs.recvPauseDesired.Swap(false) {
					return // already not paused
				}
				l.detachQMu.Lock()
				l.detachQueue = append(l.detachQueue, cs)
				l.detachQPending.Store(1)
				l.detachQMu.Unlock()
				if l.eventFD >= 0 {
					var val [8]byte
					val[0] = 1
					_, _ = unix.Write(l.eventFD, val[:])
				}
			}
			// Ensure eventfd is available for wakeup. Sync mode is
			// safe to mutate l.eventFD inline (event-loop thread).
			// Async mode defers to drainDetachQueue (worker thread)
			// via asyncDetachPending — see the field's comment.
			if !l.async && l.eventFD < 0 {
				efd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
				if err == nil {
					l.eventFD = efd
					_ = unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_ADD, efd, &unix.EpollEvent{
						Events: unix.EPOLLIN | unix.EPOLLET,
						Fd:     int32(efd),
					})
				}
			}
			// Async mode (HTTP1): the dispatch goroutine took detachMu
			// around ProcessH1 so writeBuf access serialises with the
			// event loop's flushWrites. Now that we've installed
			// `guarded` (which re-acquires detachMu on every call),
			// keeping the lock held would deadlock the very next write
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
			if l.async && cs.asyncPromoted && cs.detachMu != nil && !cs.asyncDetachUnlocked {
				cs.asyncDetachUnlocked = true
				cs.detachMu.Unlock()
			}
			// Async mode: enqueue cs so drainDetachQueue picks up the
			// deferred bookkeeping (asyncDetachPending). The first
			// guarded() write will also enqueue+signal, but the
			// explicit signal here covers the rare case where the
			// middleware returns without writing.
			if l.async {
				l.detachQMu.Lock()
				l.detachQueue = append(l.detachQueue, cs)
				l.detachQPending.Store(1)
				l.detachQMu.Unlock()
				if l.eventFD >= 0 {
					var val [8]byte
					val[0] = 1
					_, _ = unix.Write(l.eventFD, val[:])
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
		}
		cs.h1State.HijackFn = func() (net.Conn, error) {
			return l.hijackConn(cs.fd)
		}
	case engine.H2C:
		cs.h2State = conn.NewH2State(l.handler, l.h2cfg, cs.writeFn, l.eventFD)
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
	h2State, err := conn.NewH2StateFromUpgrade(l.handler, l.h2cfg, writeFn, l.eventFD, info)
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
		if cs.pendingBytes+len(cs.bodyBuf) > cs.writeCap() {
			return
		}
		cs.writeBuf = append(cs.writeBuf, data...)
		cs.pendingBytes += len(data)
		// Don't markDirty here — drainRead's inline flush handles the
		// happy path. Only markDirty if the inline flush partially
		// completes, avoiding linked-list overhead per request.
	}
}

// makeWriteBodyFn stages a zero-copy body slice for scatter-gather
// writev at flush time. The body is NOT copied — it must remain valid
// and unmutated until flushWrites drains it. Used by the H1 response
// adapter for bodies ≥ 8 KiB to skip the respBuf → writeBuf memcpy.
func (l *Loop) makeWriteBodyFn(cs *connState) func([]byte) {
	return func(body []byte) {
		if cs.pendingBytes+len(cs.bodyBuf)+len(body) > cs.writeCap() {
			return
		}
		if cs.bodyBuf != nil {
			// A second large-body write in the same request: fall back
			// to copying (we only carry one writev body slot per flush).
			cs.writeBuf = append(cs.writeBuf, body...)
			cs.pendingBytes += len(body)
			return
		}
		cs.bodyBuf = body
		cs.pendingBytes += len(body)
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
// Pipelined file requests in a single recv (cs.sendfile already set) and
// rare dup failures fall back to a buffered copy of this response into
// writeBuf, so the engine never needs two concurrent sendfile slots and
// response ordering is always preserved.
func (l *Loop) makeSendFileFn(cs *connState) func(header []byte, file *os.File, offset, length int64) error {
	return func(header []byte, file *os.File, offset, length int64) error {
		if cs.sendfile != nil {
			return bufferedFileFallback(cs, header, file, offset, length)
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
// Config.AsyncHandlers is enabled. It takes the currently-buffered
// bytes via a double-buffer swap, runs ProcessH1 under detachMu (so
// write paths serialize with worker-initiated flushes), flushes the
// response, and parks on asyncCond.Wait when the buffer is empty —
// keeping the goroutine alive across keep-alive requests. The worker
// calls asyncCond.Signal after each append, and asyncCond.Broadcast
// in closeConn so the parked goroutine exits cleanly.
//
// This preserves HTTP/1.1 pipelining order guarantees: a pipelined
// burst arrives in one worker read, ProcessH1's offset loop drains
// every request from that slice in order, and responses appear in the
// same order on cs.writeBuf before the flush. The "one goroutine at a
// time per conn" invariant is enforced by cs.asyncRun.
func (l *Loop) runAsyncHandler(cs *connState) {
	defer l.asyncWG.Done()
	// Last-resort panic safety net. User handlers SHOULD use recovery
	// middleware, but a goroutine spawned by the engine must not let an
	// unrecovered panic crash the process. routerAdapter has its own
	// recover for the sync path; async dispatch needs symmetric
	// protection because the panic would otherwise unwind here,
	// outside any router code. See #240.
	defer func() {
		if r := recover(); r != nil {
			if l.logger != nil {
				l.logger.Error("async handler panicked",
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
			// Signal worker via detachQueue + eventfd (never close the
			// fd from this goroutine — races with drainRead on the
			// worker's stale l.conns slot).
			l.detachQMu.Lock()
			l.detachQueue = append(l.detachQueue, cs)
			l.detachQPending.Store(1)
			l.detachQMu.Unlock()
			if l.eventFD >= 0 {
				var val [8]byte
				val[0] = 1
				_, _ = unix.Write(l.eventFD, val[:])
			}
		}
	}()
	for {
		cs.asyncInMu.Lock()
		for len(cs.asyncInBuf) == 0 && !cs.asyncClosed.Load() {
			// Park until the worker appends more bytes or the conn is
			// being torn down. Stays alive across keep-alive requests
			// so we don't pay the ~1.5µs goroutine spawn cost per
			// request — profile showed this accounted for ~3% of
			// epoll+async CPU on high-rps redis workloads.
			cs.asyncCond.Wait()
		}
		if cs.asyncClosed.Load() {
			cs.asyncRun = false
			cs.asyncInMu.Unlock()
			return
		}
		// Double-buffer swap: hand asyncInBuf to the goroutine, reuse
		// the emptied asyncOutBuf as the worker's new asyncInBuf. No
		// allocation on steady state — append(out[:0], ...) grows into
		// the retained backing array.
		cs.asyncInBuf, cs.asyncOutBuf = cs.asyncOutBuf[:0], cs.asyncInBuf
		data := cs.asyncOutBuf
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
		}
		// Re-check asyncClosed under detachMu when we acquired it;
		// closeConn sets asyncClosed BEFORE tearing down cs.h1State.
		// Mirrors the iouring fix.
		if cs.asyncClosed.Load() {
			if acquiredDetachMu {
				cs.detachMu.Unlock()
			}
			cs.asyncInMu.Lock()
			cs.asyncRun = false
			cs.asyncInMu.Unlock()
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
				if err := flushWrites(cs); err != nil {
					promoteErr = err
				}
			}
			cs.detachMu.Unlock()
			if promoteErr != nil {
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
			l.detachQMu.Lock()
			l.detachQueue = append(l.detachQueue, cs)
			l.detachQPending.Store(1)
			l.detachQMu.Unlock()
			if l.eventFD >= 0 {
				var val [8]byte
				val[0] = 1
				_, _ = unix.Write(l.eventFD, val[:])
			}
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
				cs.asyncRun = false
				cs.asyncInMu.Unlock()
				l.detachQMu.Lock()
				l.detachQueue = append(l.detachQueue, cs)
				l.detachQPending.Store(1)
				l.detachQMu.Unlock()
				if l.eventFD >= 0 {
					var val [8]byte
					val[0] = 1
					_, _ = unix.Write(l.eventFD, val[:])
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
			flushErr = flushWrites(cs)
		}
		// Resync pendingBytes with actual buffer state. makeWriteFn uses
		// pendingBytes to enforce writeCap backpressure; without this
		// reset, it grows by len(response) on every request and after
		// ~writeCap bytes of cumulative responses every subsequent
		// makeWriteFn call silently drops its payload — the client then
		// blocks forever on the missing response. Mirror the inline-flush
		// path (drainRead) and dirty-loop accounting.
		if flushErr == nil {
			if cs.writePos >= len(cs.writeBuf) && len(cs.bodyBuf) == 0 {
				cs.pendingBytes = 0
			} else {
				cs.pendingBytes = len(cs.writeBuf) - cs.writePos + len(cs.bodyBuf)
			}
		}
		partial := flushErr == nil && (cs.writePos < len(cs.writeBuf) || len(cs.bodyBuf) > 0)
		cs.detachMu.Unlock()

		if partial {
			l.detachQMu.Lock()
			l.detachQueue = append(l.detachQueue, cs)
			l.detachQPending.Store(1)
			l.detachQMu.Unlock()
			if l.eventFD >= 0 {
				var val [8]byte
				val[0] = 1
				_, _ = unix.Write(l.eventFD, val[:])
			}
		}

		if processErr != nil || flushErr != nil {
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
			cs.asyncRun = false
			cs.asyncInMu.Unlock()
			l.detachQMu.Lock()
			l.detachQueue = append(l.detachQueue, cs)
			l.detachQPending.Store(1)
			l.detachQMu.Unlock()
			if l.eventFD >= 0 {
				var val [8]byte
				val[0] = 1
				_, _ = unix.Write(l.eventFD, val[:])
			}
			return
		}
	}
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
		if cs.detachClosed {
			continue
		}
		// Hijacked conn (#3.1): hijackConn already detached the fd from
		// epoll/live set/conn table and handed it to the caller as a
		// net.Conn (the original fd is closed; the caller owns a dup). It
		// deferred ONLY the pool release to here, where the dispatch
		// goroutine that referenced cs has now exited (it enqueued cs on
		// its way out). Release cs and skip closeConn — there is no fd to
		// close and l.conns[fd] is already nil.
		if cs.hijacked {
			releaseConnState(cs)
			continue
		}
		// Dispatch goroutine signaled close via asyncClosed. Only the
		// worker can safely touch l.conns / dirty list, so we handle
		// the teardown here.
		if cs.asyncClosed.Load() {
			l.closeConn(cs.fd)
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
			l.markDirty(cs)
			continue
		}
		// Async-mode Detach finalisation: OnDetach ran on the
		// dispatch goroutine and cannot touch event-loop-owned state
		// (l.detachedCount or l.eventFD). It set asyncDetachPending
		// and enqueued cs so we land here on the event-loop thread
		// and run those mutations safely. Idempotent — the flag is
		// cleared before the bookkeeping so a second drainDetachQueue
		// pass (from the same enqueue burst) is a no-op.
		if cs.asyncDetachPending {
			cs.asyncDetachPending = false
			l.detachedCount++
			if l.eventFD < 0 {
				efd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
				if err == nil {
					l.eventFD = efd
					_ = unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_ADD, efd, &unix.EpollEvent{
						Events: unix.EPOLLIN | unix.EPOLLET,
						Fd:     int32(efd),
					})
				}
			}
		}
		// Apply any pending pause/resume request from the WS middleware.
		// EPOLL_CTL_MOD with Events=0 stops EPOLLIN delivery; restoring
		// EPOLLIN|EPOLLET re-enables it. The kernel applies TCP-level
		// backpressure (zero-window) when its recv buffer fills.
		if desired := cs.recvPauseDesired.Load(); desired != cs.recvPaused {
			var events uint32 = unix.EPOLLIN | unix.EPOLLET
			if desired {
				events = 0
			}
			_ = unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_MOD, cs.fd, &unix.EpollEvent{
				Events: events,
				Fd:     int32(cs.fd),
			})
			cs.recvPaused = desired
		}
		l.markDirty(cs)
	}
	l.detachQSpare = l.detachQSpare[:0]
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
// a level-triggered EPOLLOUT to the conn's interest set and DROP it from the
// dirty list. The worker then blocks in epoll_wait until the socket is
// writable again, at which point handleWritable flushes and disarms.
//
// EPOLLIN stays edge-triggered (EPOLLET applies only to EPOLLIN); EPOLLOUT
// is level-triggered so it keeps firing while the socket is writable and
// there is no missed-wakeup risk. Mirrors driver.go's flushDriverSendLocked.
func (l *Loop) armEpollOut(cs *connState) {
	// Backpressure replaces the dirty-list retry; the two must not coexist
	// or adaptiveTimeoutMs would still return 0 and busy-poll.
	l.removeDirty(cs)
	if cs.epollOut {
		return
	}
	if err := unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_MOD, cs.fd, &unix.EpollEvent{
		Events: unix.EPOLLIN | unix.EPOLLET | unix.EPOLLOUT,
		Fd:     int32(cs.fd),
	}); err == nil {
		cs.epollOut = true
	} else {
		// MOD failed (should not happen for a registered fd); fall back to
		// the dirty-list retry so the pending bytes still get flushed.
		l.markDirty(cs)
	}
}

// disarmEpollOut removes the level-triggered EPOLLOUT interest once a conn's
// pending writes have fully drained, restoring the read-only edge-triggered
// interest so an idle fd doesn't wake the loop on every writable signal.
func (l *Loop) disarmEpollOut(cs *connState) {
	if !cs.epollOut {
		return
	}
	_ = unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_MOD, cs.fd, &unix.EpollEvent{
		Events: unix.EPOLLIN | unix.EPOLLET,
		Fd:     int32(cs.fd),
	})
	cs.epollOut = false
}

// handleWritable resumes a backpressured conn on an EPOLLOUT event: flush
// the pending bytes and, if fully drained, disarm EPOLLOUT and restore the
// read-only interest. A still-partial flush leaves EPOLLOUT armed so the
// next writable edge resumes. Returns false if the conn was closed.
func (l *Loop) handleWritable(cs *connState) {
	if mu := cs.detachMu; mu != nil {
		mu.Lock()
	}
	err := flushWrites(cs)
	drained := err == nil && !csWritePending(cs)
	if err == nil {
		if drained {
			cs.pendingBytes = 0
		} else {
			cs.pendingBytes = csPendingBytes(cs)
		}
	}
	if err != nil && cs.h1State != nil && cs.h1State.OnError != nil {
		cs.h1State.OnError(err)
	}
	if mu := cs.detachMu; mu != nil {
		mu.Unlock()
	}
	if err != nil {
		l.closeConn(cs.fd)
		return
	}
	if drained {
		l.disarmEpollOut(cs)
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
	l.liveConns = append(l.liveConns, cs.fd)
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
	if i < 0 || i >= len(l.liveConns) || l.liveConns[i] != cs.fd {
		return
	}
	last := len(l.liveConns) - 1
	if i != last {
		movedFD := l.liveConns[last]
		l.liveConns[i] = movedFD
		if moved := l.conns[movedFD]; moved != nil {
			moved.liveIdx = i
		}
	}
	l.liveConns = l.liveConns[:last]
	cs.liveIdx = -1
}

func (l *Loop) adaptiveTimeoutMs(base int) int {
	// When dirty list has unflushed data, poll immediately.
	if l.dirtyHead != nil {
		return 0
	}
	// When H2 connections exist and no eventfd is available, fall back to
	// 1ms polling for write queue draining. With eventfd, handler goroutines
	// signal directly and epoll_wait returns event-driven — no polling needed.
	if len(l.h2Conns) > 0 && l.eventFD < 0 {
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

// checkTimeouts scans active connections and closes any that have exceeded
// their configured timeout. Called every 1024 iterations (~100ms).
//
// Iterates the dense liveConns slice (#318) rather than the sparse
// 0..maxFD range, so the cost is O(active conns) regardless of FD space.
// Iterating by REVERSE index is load-bearing: closeConn → removeLiveConn
// swaps the closed conn's slot with the last entry, so a forward scan
// would skip the swapped-in element. Walking high→low means any swap only
// touches indices we've already visited.
func (l *Loop) checkTimeouts() {
	now := time.Now().UnixNano()
	for i := len(l.liveConns) - 1; i >= 0; i-- {
		fd := l.liveConns[i]
		cs := l.conns[fd]
		if cs == nil {
			continue
		}
		// Detached connections (e.g. WebSocket): honor an explicit deadline
		// supplied by the middleware via SetWSIdleDeadline. Skip the
		// engine-config-driven idle/read/write timeouts since the middleware
		// owns the I/O lifecycle. Async-mode conns set detachMu up front
		// without a true detach — fall through to the normal scan for those.
		if cs.h1State != nil && cs.h1State.Detached.Load() {
			if dl := cs.h1State.IdleDeadlineNs.Load(); dl > 0 && now > dl {
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
		if cs.h1State != nil {
			if dl := cs.h1State.HeaderDeadlineNs.Load(); dl > 0 && now > dl {
				l.closeConn(fd)
				continue
			}
		}
		elapsed := time.Duration(now - cs.lastActivity)
		if cs.dirty {
			if l.cfg.WriteTimeout > 0 && elapsed > l.cfg.WriteTimeout {
				l.closeConn(fd)
			}
		} else {
			if l.cfg.IdleTimeout > 0 && elapsed > l.cfg.IdleTimeout {
				l.closeConn(fd)
			} else if l.cfg.ReadTimeout > 0 && elapsed > l.cfg.ReadTimeout {
				l.closeConn(fd)
			}
		}
	}
}

func (l *Loop) closeConn(fd int) {
	cs := l.conns[fd]
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
		// goroutine is mid-write, we block until it finishes.
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
		// Async-mode HTTP1 conns pre-allocate detachMu without bumping
		// detachedCount, so decrementing would underflow here.
		if cs.h1State != nil && cs.h1State.Detached.Load() && l.detachedCount > 0 {
			l.detachedCount--
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
			conn.CloseH1(cs.h1State)
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
	// H1 close: SHUT_WR + Close. The shutdown call forces FIN regardless
	// of the kernel recv-buffer state — important under slowloris where
	// the peer is still writing drips and a plain Close on a non-empty
	// recv buffer would send RST instead (which is not retransmitted by
	// TCP, so a single packet loss strands the walker).
	//
	// No drainRecvBuffer between SHUT_WR and Close: an empty drain leaves
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
		drainRecvBuffer(fd)
		_ = unix.Close(fd)
	}
	l.removeLiveConn(cs)
	l.conns[fd] = nil
	l.connCount--
	l.activeConns.Add(-1)

	if l.cfg.OnDisconnect != nil {
		l.cfg.OnDisconnect(cs.remoteAddr)
	}

	// Never release the connState while a goroutine may still hold a
	// reference (WS/SSE detach OR async dispatch). GC collects cs once
	// the goroutine finishes and all closure references are dropped.
	if !detached {
		releaseConnState(cs)
	}
}

func (l *Loop) shutdown() {
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
		fd := l.liveConns[i]
		cs := l.conns[fd]
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
	}

	// Phase 2: wait for async dispatch goroutines to exit. They were
	// signaled (asyncClosed + Broadcast) in phase 1; this join guarantees
	// no goroutine is still inside ProcessH1 / writeFn against cs before we
	// close fds or recycle connState below.
	l.asyncWG.Wait()

	// Phase 3: now that the async dispatch goroutines have exited (phase 2),
	// close the fds and release the connState back to the pool. The pool
	// release stays gated on !detached for the same reason as closeConn: a
	// truly-detached WS/SSE conn's middleware goroutine is NOT tracked by
	// asyncWG and may still hold a reference, so we let GC reclaim cs once
	// that goroutine drops its closure refs rather than recycling it here.
	for i := len(l.liveConns) - 1; i >= 0; i-- {
		fd := l.liveConns[i]
		cs := l.conns[fd]
		if cs == nil {
			continue
		}
		_ = unix.Close(fd)
		if cs.detachMu == nil {
			releaseConnState(cs)
		}
		l.conns[fd] = nil
	}
	l.liveConns = l.liveConns[:0]

	if l.listenFD >= 0 {
		_ = unix.Close(l.listenFD)
	}
	if l.eventFD >= 0 {
		_ = unix.Close(l.eventFD)
	}
	if l.timerFD >= 0 {
		_ = unix.Close(l.timerFD)
	}
	_ = unix.Close(l.epollFD)
}

// drainRecvBuffer reads and discards any data in the socket receive buffer.
// This prevents close() from sending RST (which discards unsent data like GOAWAY).
func drainRecvBuffer(fd int) {
	var buf [4096]byte
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
		return -1, err
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
		_ = unix.Close(fd)
		return -1, err
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

func sockaddrString(sa unix.Sockaddr) string {
	switch v := sa.(type) {
	case *unix.SockaddrInet4:
		// "xxx.xxx.xxx.xxx:ppppp" — max 21 bytes. Formatting by hand
		// eliminates fmt.Sprintf + net.IP.String allocations that would
		// otherwise hit on every accepted conn.
		var buf [21]byte
		b := strconv.AppendUint(buf[:0], uint64(v.Addr[0]), 10)
		b = append(b, '.')
		b = strconv.AppendUint(b, uint64(v.Addr[1]), 10)
		b = append(b, '.')
		b = strconv.AppendUint(b, uint64(v.Addr[2]), 10)
		b = append(b, '.')
		b = strconv.AppendUint(b, uint64(v.Addr[3]), 10)
		b = append(b, ':')
		b = strconv.AppendUint(b, uint64(uint16(v.Port)), 10)
		return string(b)
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
