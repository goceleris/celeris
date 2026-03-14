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
	events       []unix.EpollEvent
	conns        []*connState
	connCount    int // number of active connections (local, for draining check)
	maxFD        int // upper bound fd for iteration in checkTimeouts/shutdown
	handler      stream.Handler
	objective    resource.ObjectiveParams
	resolved     resource.ResolvedResources
	sockOpts     sockopts.Options
	cfg          resource.Config
	logger       *slog.Logger
	acceptPaused *atomic.Bool
	wake         chan struct{}
	wakeMu       sync.Mutex
	suspended    atomic.Bool

	reqCount    *atomic.Uint64
	activeConns *atomic.Int64
	errCount    *atomic.Uint64
	reqBatch    uint64 // batched request count, flushed to reqCount per iteration
	tickCounter uint32

	dirtyHead *connState // head of intrusive doubly-linked dirty list
}

func newLoop(id, cpuID int, handler stream.Handler,
	objective resource.ObjectiveParams, resolved resource.ResolvedResources,
	cfg resource.Config, reqCount *atomic.Uint64, activeConns *atomic.Int64, errCount *atomic.Uint64,
	acceptPaused *atomic.Bool) (*Loop, error) {

	epollFD, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("epoll_create1: %w", err)
	}

	listenFD, err := createListenSocket(cfg.Addr)
	if err != nil {
		_ = unix.Close(epollFD)
		return nil, fmt.Errorf("listen socket: %w", err)
	}

	if err := unix.EpollCtl(epollFD, unix.EPOLL_CTL_ADD, listenFD, &unix.EpollEvent{
		Events: unix.EPOLLIN | unix.EPOLLET,
		Fd:     int32(listenFD),
	}); err != nil {
		_ = unix.Close(epollFD)
		_ = unix.Close(listenFD)
		return nil, fmt.Errorf("epoll_ctl listen: %w", err)
	}

	return &Loop{
		id:           id,
		cpuID:        cpuID,
		epollFD:      epollFD,
		listenFD:     listenFD,
		events:       make([]unix.EpollEvent, resolved.MaxEvents),
		conns:        make([]*connState, connTableSize),
		handler:      handler,
		objective:    objective,
		resolved:     resolved,
		cfg:          cfg,
		logger:       cfg.Logger,
		acceptPaused: acceptPaused,
		wake:         make(chan struct{}),
		sockOpts: sockopts.Options{
			TCPNoDelay:  objective.TCPNoDelay,
			TCPQuickAck: objective.TCPQuickAck,
			SOBusyPoll:  objective.SOBusyPoll,
			RecvBuf:     resolved.SocketRecv,
			SendBuf:     resolved.SocketSend,
		},
		reqCount:    reqCount,
		activeConns: activeConns,
		errCount:    errCount,
	}, nil
}

func (l *Loop) run(ctx context.Context) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	_ = platform.PinToCPU(l.cpuID)

	activeTimeoutMs := int(l.objective.EpollTimeout.Milliseconds())
	if activeTimeoutMs <= 0 {
		activeTimeoutMs = 1
	}

	for {
		select {
		case <-ctx.Done():
			l.shutdown()
			return
		default:
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

		timeoutMs := activeTimeoutMs
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

		now := time.Now().UnixNano()
		for i := range n {
			ev := &l.events[i]
			fd := int(ev.Fd)

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

		for cs := l.dirtyHead; cs != nil; {
			next := cs.dirtyNext
			err := flushWrites(cs)
			if err != nil {
				l.removeDirty(cs)
				l.closeConn(cs.fd)
			} else if len(cs.writeBuf) == 0 {
				cs.pendingBytes = 0
				l.removeDirty(cs)
			}
			// else: partial write, keep on dirty list for next iteration
			cs = next
		}

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

		cs := newConnState(ctx, newFD, l.resolved.BufferSize)
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
			_ = flushWrites(cs)
			l.closeConn(fd)
			return
		}
		if n == 0 {
			_ = flushWrites(cs)
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
			processErr = conn.ProcessH2(cs.ctx, data, cs.h2State, l.handler, writeFn, conn.H2Config{
				MaxConcurrentStreams: l.cfg.MaxConcurrentStreams,
				InitialWindowSize:    l.cfg.InitialWindowSize,
				MaxFrameSize:         l.cfg.MaxFrameSize,
			})
		}

		l.reqBatch++

		// lastActivity already set above; timeout checked in checkTimeouts.

		if processErr != nil {
			if errors.Is(processErr, conn.ErrHijacked) {
				return // FD already detached — do not close or flush
			}
			// Flush any pending writes (e.g. error responses) before closing.
			_ = flushWrites(cs)
			cs.pendingBytes = 0
			l.closeConn(fd)
			return
		}

		if cs.pendingBytes > maxPendingBytes {
			l.closeConn(fd)
			return
		}
	}
}

func (l *Loop) hijackConn(fd int) (net.Conn, error) {
	cs := l.conns[fd]
	if cs == nil {
		return nil, errors.New("celeris: connection not found")
	}
	cs.cancel()
	_ = unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_DEL, fd, nil)
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
		cs.h1State.HijackFn = func() (net.Conn, error) {
			return l.hijackConn(cs.fd)
		}
	case engine.H2C:
		cs.h2State = conn.NewH2State(l.handler, conn.H2Config{
			MaxConcurrentStreams: l.cfg.MaxConcurrentStreams,
			InitialWindowSize:    l.cfg.InitialWindowSize,
			MaxFrameSize:         l.cfg.MaxFrameSize,
		}, cs.writeFn)
		cs.h2State.SetRemoteAddr(cs.remoteAddr)
	}
}

func (l *Loop) makeWriteFn(cs *connState) func([]byte) {
	return func(data []byte) {
		if cs.pendingBytes > maxPendingBytes {
			return
		}
		cs.writeBuf = append(cs.writeBuf, data...)
		cs.pendingBytes += len(data)
		l.markDirty(cs)
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
	l.removeDirty(cs)
	if cs.h2State != nil {
		conn.CloseH2(cs.h2State)
	}
	cs.cancel()
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
}

func (l *Loop) shutdown() {
	for fd := 0; fd <= l.maxFD; fd++ {
		cs := l.conns[fd]
		if cs == nil {
			continue
		}
		if cs.h2State != nil {
			conn.CloseH2(cs.h2State)
		}
		cs.cancel()
		_ = unix.Close(fd)
	}
	if l.listenFD >= 0 {
		_ = unix.Close(l.listenFD)
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
