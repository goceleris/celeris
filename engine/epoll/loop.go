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
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/platform"
	"github.com/goceleris/celeris/internal/sockopts"
	"github.com/goceleris/celeris/protocol/detect"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// connTableSize is the number of slots in the flat connection array.
// Must accommodate the maximum number of concurrent connections per worker.
const connTableSize = 65536

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

	reqCount         *atomic.Uint64
	activeConns      *atomic.Int64
	errCount         *atomic.Uint64
	reqBatch         uint64 // batched request count, flushed to reqCount per iteration
	tickCounter      uint32
	consecutiveEmpty uint32 // consecutive iterations with no events (for adaptive timeout)
	cachedNow        int64  // cached time.Now().UnixNano(), refreshed every 64 iterations

	dirtyHead    *connState // head of intrusive doubly-linked dirty list
	h2Conns      []int      // FDs of H2 connections (for write queue polling)
	h2cfg        conn.H2Config
	detachQueue  []*connState // detached conns with pending writes (goroutine-safe via detachQMu)
	detachQMu    sync.Mutex
	detachQSpare []*connState // reuse slice to avoid alloc in drainDetachQueue
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

		// ACTIVE → DRAINING: close listen socket to leave SO_REUSEPORT group.
		if l.listenFD >= 0 && l.acceptPaused.Load() {
			_ = unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_DEL, l.listenFD, nil)
			_ = unix.Close(l.listenFD)
			l.listenFD = -1
		}

		// SUSPENDED → ACTIVE: re-create listen socket after ResumeAccept.
		if l.listenFD < 0 && !l.acceptPaused.Load() {
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

		for cs := l.dirtyHead; cs != nil; {
			next := cs.dirtyNext
			if mu := cs.detachMu; mu != nil {
				mu.Lock()
			}
			err := flushWrites(cs)
			if err != nil {
				if mu := cs.detachMu; mu != nil {
					mu.Unlock()
				}
				l.removeDirty(cs)
				l.closeConn(cs.fd)
			} else if cs.writePos >= len(cs.writeBuf) {
				cs.pendingBytes = 0
				if mu := cs.detachMu; mu != nil {
					mu.Unlock()
				}
				l.removeDirty(cs)
			} else {
				// Sync pendingBytes with actual buffer state after partial write.
				cs.pendingBytes = len(cs.writeBuf) - cs.writePos
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

		// Drain detached goroutine writes. Goroutines append to the queue
		// instead of calling markDirty directly (dirtyHead is loop-local).
		l.drainDetachQueue()

		// Flush batched request count to the shared atomic counter. This
		// replaces per-request atomic.Add with one atomic per event loop
		// iteration, eliminating cache-line bouncing under multi-worker
		// contention.
		if l.reqBatch > 0 {
			l.reqCount.Add(l.reqBatch)
			l.reqBatch = 0
		}

		// Check connection timeouts every 1024 iterations (~100ms).
		l.tickCounter++
		if l.tickCounter&0x3FF == 0 {
			l.checkTimeouts()
		}

		// DRAINING → SUSPENDED: no listen socket, no connections, events processed.
		if l.listenFD < 0 && l.connCount == 0 && l.acceptPaused.Load() {
			l.wakeMu.Lock()
			if !l.acceptPaused.Load() {
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

		cs := acquireConnState(ctx, newFD, l.resolved.BufferSize)
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

		if l.cfg.Protocol != engine.Auto {
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
		n, err := unix.Read(fd, cs.buf)
		if err != nil {
			if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
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
		if n == 0 {
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

		data := cs.buf[:n]

		cs.lastActivity = now

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

		var processErr error
		switch cs.protocol {
		case engine.HTTP1:
			processErr = conn.ProcessH1(cs.ctx, data, cs.h1State, l.handler, writeFn)
		case engine.H2C:
			processErr = conn.ProcessH2(cs.ctx, data, cs.h2State, l.handler, writeFn, l.h2cfg)
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
		if cs.writePos < len(cs.writeBuf) {
			if fErr := flushWrites(cs); fErr != nil {
				if mu := cs.detachMu; mu != nil {
					mu.Unlock()
				}
				if cs.dirty {
					l.removeDirty(cs)
				}
				l.closeConn(fd)
				return
			}
			if cs.writePos >= len(cs.writeBuf) {
				// Fully flushed — no dirty list needed.
				cs.pendingBytes = 0
				if cs.dirty {
					l.removeDirty(cs)
				}
			} else {
				// Partial write — sync pendingBytes with actual buffer state.
				cs.pendingBytes = len(cs.writeBuf) - cs.writePos
				l.markDirty(cs)
			}
		}
		if mu := cs.detachMu; mu != nil {
			mu.Unlock()
		}

		if cs.pendingBytes > maxPendingBytes {
			l.closeConn(fd)
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
		cs.h1State.OnDetach = func() {
			cs.h1State.Detached = true
			mu := &sync.Mutex{}
			cs.detachMu = mu
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

func (l *Loop) makeWriteFn(cs *connState) func([]byte) {
	return func(data []byte) {
		if cs.pendingBytes > maxPendingBytes {
			return
		}
		cs.writeBuf = append(cs.writeBuf, data...)
		cs.pendingBytes += len(data)
		// Don't markDirty here — drainRead's inline flush handles the
		// happy path. Only markDirty if the inline flush partially
		// completes, avoiding linked-list overhead per request.
	}
}

func (l *Loop) drainDetachQueue() {
	l.detachQMu.Lock()
	l.detachQSpare, l.detachQueue = l.detachQueue, l.detachQSpare[:0]
	l.detachQMu.Unlock()
	for _, cs := range l.detachQSpare {
		if cs.detachClosed {
			continue
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
		return d
	default:
		d := base * 4
		if d > 500 {
			d = 500
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
		// Signal the detached goroutine's writeFn to stop writing.
		// The mutex serializes with any in-progress write — if the
		// goroutine is mid-write, we block until it finishes.
		cs.detachMu.Lock()
		cs.detachClosed = true
		cs.detachMu.Unlock()
	}
	l.removeDirty(cs)
	if !detached {
		// Only release the H1 stream when NOT detached — the goroutine
		// still holds a reference through its Context/StreamWriter.
		if cs.h1State != nil {
			conn.CloseH1(cs.h1State)
		}
	}
	if cs.h2State != nil {
		conn.CloseH2(cs.h2State)
		l.removeH2Conn(fd)
	}
	_ = unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_DEL, fd, nil)
	_ = unix.Shutdown(fd, unix.SHUT_WR)
	drainRecvBuffer(fd)
	_ = unix.Close(fd)
	l.conns[fd] = nil
	l.connCount--
	l.activeConns.Add(-1)

	if l.cfg.OnDisconnect != nil {
		l.cfg.OnDisconnect(cs.remoteAddr)
	}

	if !detached {
		releaseConnState(cs)
	}
	// Detached: connState is NOT returned to the pool. The goroutine's
	// closures still reference it. GC collects it after the goroutine
	// finishes and all closure references are dropped.
}

func (l *Loop) shutdown() {
	for fd := 0; fd <= l.maxFD; fd++ {
		cs := l.conns[fd]
		if cs == nil {
			continue
		}
		detached := cs.detachMu != nil
		if detached {
			cs.detachMu.Lock()
			cs.detachClosed = true
			cs.detachMu.Unlock()
		}
		if !detached && cs.h1State != nil {
			conn.CloseH1(cs.h1State)
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
