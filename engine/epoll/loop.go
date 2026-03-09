//go:build linux

package epoll

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"runtime"
	"sync/atomic"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/platform"
	"github.com/goceleris/celeris/internal/sockopts"
	"github.com/goceleris/celeris/protocol/detect"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// Loop is an epoll-based event loop worker.
type Loop struct {
	id           int
	cpuID        int
	epollFD      int
	listenFD     int
	events       []unix.EpollEvent
	conns        map[int]*connState
	handler      stream.Handler
	objective    resource.ObjectiveParams
	resolved     resource.ResolvedResources
	sockOpts     sockopts.Options
	cfg          resource.Config
	logger       *slog.Logger
	acceptPaused *atomic.Bool

	reqCount    *atomic.Uint64
	activeConns *atomic.Int64
	errCount    *atomic.Uint64
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
		conns:        make(map[int]*connState),
		handler:      handler,
		objective:    objective,
		resolved:     resolved,
		cfg:          cfg,
		logger:       cfg.Logger,
		acceptPaused: acceptPaused,
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

		timeoutMs := activeTimeoutMs
		if l.acceptPaused.Load() && len(l.conns) == 0 {
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

		for i := range n {
			ev := &l.events[i]
			fd := int(ev.Fd)

			if fd == l.listenFD {
				l.acceptAll(ctx)
				continue
			}

			// Process EPOLLIN before EPOLLHUP: a peer may send data and
			// immediately close, producing both flags in one event.
			if ev.Events&unix.EPOLLIN != 0 {
				l.drainRead(fd)
			}

			if ev.Events&(unix.EPOLLERR|unix.EPOLLHUP) != 0 {
				if _, ok := l.conns[fd]; ok {
					l.closeConn(fd)
				}
			}
		}

		for _, cs := range l.conns {
			if err := flushWrites(cs); err != nil {
				l.closeConn(cs.fd)
			} else {
				cs.pendingBytes = 0
			}
		}
	}
}

func (l *Loop) acceptAll(ctx context.Context) {
	if l.acceptPaused.Load() {
		return
	}

	for {
		newFD, _, err := unix.Accept4(l.listenFD, unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)
		if err != nil {
			if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
				return
			}
			l.errCount.Add(1)
			return
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
		l.conns[newFD] = cs
		l.activeConns.Add(1)

		if l.cfg.Protocol != engine.Auto {
			cs.protocol = l.cfg.Protocol
			cs.detected = true
			l.initProtocol(cs)
		}
	}
}

func (l *Loop) drainRead(fd int) {
	cs, ok := l.conns[fd]
	if !ok {
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

		writeFn := l.makeWriteFn(cs)

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

		l.reqCount.Add(1)

		if processErr != nil {
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

func (l *Loop) initProtocol(cs *connState) {
	switch cs.protocol {
	case engine.HTTP1:
		cs.h1State = conn.NewH1State()
	case engine.H2C:
		writeFn := l.makeWriteFn(cs)
		cs.h2State = conn.NewH2State(l.handler, conn.H2Config{
			MaxConcurrentStreams: l.cfg.MaxConcurrentStreams,
			InitialWindowSize:    l.cfg.InitialWindowSize,
			MaxFrameSize:         l.cfg.MaxFrameSize,
		}, writeFn)
	}
}

func (l *Loop) makeWriteFn(cs *connState) func([]byte) {
	return func(data []byte) {
		if cs.pendingBytes > maxPendingBytes {
			return
		}
		copied := make([]byte, len(data))
		copy(copied, data)
		cs.pending = append(cs.pending, copied)
		cs.pendingBytes += len(copied)
	}
}

func (l *Loop) closeConn(fd int) {
	cs, ok := l.conns[fd]
	if !ok {
		return
	}
	if cs.h2State != nil {
		conn.CloseH2(cs.h2State)
	}
	cs.cancel()
	_ = unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_DEL, fd, nil)
	_ = unix.Shutdown(fd, unix.SHUT_WR)
	// Drain receive buffer to prevent RST from discarding unsent data
	// (close() with unread data in recv buffer causes RST instead of FIN).
	drainRecvBuffer(fd)
	_ = unix.Close(fd)
	delete(l.conns, fd)
	l.activeConns.Add(-1)
}

func (l *Loop) shutdown() {
	for fd, cs := range l.conns {
		if cs.h2State != nil {
			conn.CloseH2(cs.h2State)
		}
		cs.cancel()
		_ = unix.Close(fd)
	}
	_ = unix.Close(l.listenFD)
	_ = unix.Close(l.epollFD)
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

func parseAddr(addr string) (unix.Sockaddr, error) {
	host, portStr := "", addr
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			host = addr[:i]
			portStr = addr[i+1:]
			break
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
