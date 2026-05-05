//go:build linux

package epoll

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

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/ctxkit"
	"github.com/goceleris/celeris/internal/platform"
	"github.com/goceleris/celeris/internal/sockopts"
	"github.com/goceleris/celeris/protocol/detect"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// connTableSize is the number of slots in the flat connection array.
// Must accommodate the maximum number of concurrent connections per worker.
const connTableSize = 65536

// errPeerClosed is returned via H1State.OnError when the peer closes the
// connection cleanly (read returns 0 bytes / EOF).
var errPeerClosed = errors.New("celeris: peer closed connection")

// asyncHandlers (legacy env-var fallback) dispatches HTTP handlers to a
// goroutine per connection instead of running them inline on the
// LockOSThread'd worker. The canonical switch is now Config.AsyncHandlers;
// the env var remains as a diagnostic override and is OR'd with the config
// flag inside run().
var asyncHandlersEnv = os.Getenv("CELERIS_ASYNC_HANDLERS") == "1"

// Loop is an epoll-based event loop worker.
type Loop struct {
	id           int
	cpuID        int
	epollFD      int
	listenFD     int
	eventFD      int // eventfd for H2 write queue wakeup (-1 if unavailable)
	events       []unix.EpollEvent
	conns        []*connState
	connCount    int // number of active connections (local, for draining check)
	maxFD        int // upper bound fd for iteration in checkTimeouts/shutdown
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
	reqBatch         uint64 // batched request count, flushed to reqCount per iteration
	tickCounter      uint32
	consecutiveEmpty uint32 // consecutive iterations with no events (for adaptive timeout)
	cachedNow        int64  // cached time.Now().UnixNano(), refreshed every 64 iterations

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
	// Config.AsyncHandlers (OR'd with the CELERIS_ASYNC_HANDLERS env-var
	// override for diagnostic flips). Checked on every drainRead; keep it
	// as a plain bool so the no-async path is a single mov+test.
	async bool
}

func newLoop(id, cpuID int, handler stream.Handler,
	resolved resource.ResolvedResources,
	cfg resource.Config, reqCount *atomic.Uint64, activeConns *atomic.Int64, errCount *atomic.Uint64,
	acceptPaused *atomic.Bool) *Loop {

	return &Loop{
		id:           id,
		cpuID:        cpuID,
		epollFD:      -1,
		listenFD:     -1,
		eventFD:      -1,
		events:       make([]unix.EpollEvent, resolved.MaxEvents),
		conns:        make([]*connState, connTableSize),
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
		reqCount:    reqCount,
		activeConns: activeConns,
		errCount:    errCount,
		h2cfg: conn.H2Config{
			MaxConcurrentStreams: cfg.MaxConcurrentStreams,
			InitialWindowSize:    cfg.InitialWindowSize,
			MaxFrameSize:         cfg.MaxFrameSize,
			MaxRequestBodySize:   cfg.MaxRequestBodySize,
		},
		async: cfg.AsyncHandlers || asyncHandlersEnv,
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
			// Refresh cached timestamp every 64 iterations to amortize
			// time.Now() vDSO cost (~50ns on ARM64). Timeout detection
			// uses multi-second windows so ~1ms resolution is sufficient.
			if l.tickCounter&0x3F == 0 {
				l.cachedNow = time.Now().UnixNano()
			}
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

			if ev.Events&(unix.EPOLLERR|unix.EPOLLHUP) != 0 {
				if fd >= 0 && fd < len(l.conns) && l.conns[fd] != nil {
					l.closeConn(fd)
				}
			}
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
			} else if cs.writePos >= len(cs.writeBuf) && len(cs.bodyBuf) == 0 {
				cs.pendingBytes = 0
				if mu := cs.detachMu; mu != nil {
					mu.Unlock()
				}
				l.removeDirty(cs)
			} else {
				// Sync pendingBytes with actual buffer state after partial write.
				cs.pendingBytes = len(cs.writeBuf) - cs.writePos + len(cs.bodyBuf)
				if mu := cs.detachMu; mu != nil {
					mu.Unlock()
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
						l.markDirty(cs)
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
		// so the WS idle-close fires within its configured budget.
		l.tickCounter++
		gate := uint32(0x3FF)
		if l.detachedCount > 0 {
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

func (l *Loop) acceptAll(ctx context.Context, now int64) {
	for i := 0; i < 64; i++ {
		newFD, sa, err := unix.Accept4(l.listenFD, unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)
		if err != nil {
			if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
				return
			}
			l.errCount.Add(1)
			return
		}

		// Bounds check: reject FDs outside the flat conn array.
		if newFD < 0 || newFD >= len(l.conns) {
			_ = unix.Close(newFD)
			l.errCount.Add(1)
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
				l.markDirty(cs)
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
		// Gated by Config.AsyncHandlers (+ CELERIS_ASYNC_HANDLERS env
		// override for ops diagnostics). The inline path below is
		// unchanged and zero-cost when async is off.
		if l.async && cs.protocol == engine.HTTP1 {
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
		switch cs.protocol {
		case engine.HTTP1:
			processErr = conn.ProcessH1(cs.ctx, data, cs.h1State, l.handler, writeFn)
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
						cs.pendingBytes = len(cs.writeBuf) - cs.writePos
						l.markDirty(cs)
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
		if cs.writePos < len(cs.writeBuf) || len(cs.bodyBuf) > 0 {
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
			if cs.writePos >= len(cs.writeBuf) && len(cs.bodyBuf) == 0 {
				// Fully flushed — no dirty list needed.
				cs.pendingBytes = 0
				if cs.dirty {
					l.removeDirty(cs)
				}
			} else {
				// Partial write — sync pendingBytes with actual buffer state.
				cs.pendingBytes = len(cs.writeBuf) - cs.writePos + len(cs.bodyBuf)
				l.markDirty(cs)
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
	_ = unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_DEL, fd, nil)
	releaseConnState(cs)
	l.conns[fd] = nil
	l.connCount--
	l.activeConns.Add(-1)
	f := os.NewFile(uintptr(fd), "tcp")
	c, err := net.FileConn(f)
	_ = f.Close()
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
		}
		cs.h1State.OnDetach = func() {
			cs.h1State.Detached = true
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
			l.detachedCount++
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
			// Ensure eventfd is available for wakeup.
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

		cs.detachMu.Lock()
		// Re-check asyncClosed under detachMu; closeConn sets it
		// BEFORE tearing down cs.h1State. Mirrors the iouring fix
		// (see engine/iouring/worker.go:runAsyncHandler).
		if cs.asyncClosed.Load() {
			cs.detachMu.Unlock()
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
		var flushErr error
		if processErr == nil && (cs.writePos < len(cs.writeBuf) || len(cs.bodyBuf) > 0) {
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
func (l *Loop) checkTimeouts() {
	now := time.Now().UnixNano()
	for fd := 0; fd <= l.maxFD; fd++ {
		cs := l.conns[fd]
		if cs == nil {
			continue
		}
		// Detached connections (e.g. WebSocket): honor an explicit deadline
		// supplied by the middleware via SetWSIdleDeadline. Skip the
		// engine-config-driven idle/read/write timeouts since the middleware
		// owns the I/O lifecycle. Async-mode conns set detachMu up front
		// without a true detach — fall through to the normal scan for those.
		if cs.h1State != nil && cs.h1State.Detached {
			if dl := cs.h1State.IdleDeadlineNs.Load(); dl > 0 && now > dl {
				l.closeConn(fd)
			}
			continue
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
		if cs.h1State != nil && cs.h1State.Detached && l.detachedCount > 0 {
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
	trulyDetached := detached && cs.h1State != nil && cs.h1State.Detached
	l.removeDirty(cs)
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
	// Fast-close path for plain H1 connections: close() alone. Saves
	// shutdown(SHUT_WR) + drainRecvBuffer syscalls (~2 per close) in
	// the Connection:close churn hot path. Graceful path retained for
	// H2 (GOAWAY / RST_STREAM flushing) and WS/SSE detach (middleware-
	// queued close frames).
	fastClose := !detached && cs.protocol == engine.HTTP1 && cs.h1State != nil && !trulyDetached
	if fastClose {
		_ = unix.Close(fd)
	} else {
		_ = unix.Shutdown(fd, unix.SHUT_WR)
		drainRecvBuffer(fd)
		_ = unix.Close(fd)
	}
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
	for fd := 0; fd <= l.maxFD; fd++ {
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
		trulyDetached := detached && cs.h1State != nil && cs.h1State.Detached
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
		_ = unix.Close(fd)
		if !detached {
			releaseConnState(cs)
		}
		l.conns[fd] = nil
	}
	if l.listenFD >= 0 {
		_ = unix.Close(l.listenFD)
	}
	if l.eventFD >= 0 {
		_ = unix.Close(l.eventFD)
	}
	_ = unix.Close(l.epollFD)
	// Wait for async dispatch goroutines to exit. They've been
	// signaled via asyncClosed + Broadcast above; this join ensures
	// the engine has no outstanding Gs touching connState memory
	// before Listen returns.
	l.asyncWG.Wait()
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

	if err := unix.Bind(fd, sa); err != nil {
		diag := bindDiag(fd, sa)
		_ = unix.Close(fd)
		return -1, fmt.Errorf("bind: %w [%s]", err, diag)
	}
	if err := unix.Listen(fd, 4096); err != nil {
		diag := bindDiag(fd, sa)
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
