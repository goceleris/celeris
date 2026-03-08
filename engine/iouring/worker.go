//go:build linux

package iouring

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/platform"
	"github.com/goceleris/celeris/internal/sockopts"
	"github.com/goceleris/celeris/protocol/detect"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"

	"golang.org/x/sys/unix"
)

// Worker is a per-core io_uring event loop.
type Worker struct {
	id           int
	cpuID        int
	ring         *Ring
	listenFD     int
	tier         TierStrategy
	conns        map[int]*connState
	handler      stream.Handler
	objective    resource.ObjectiveParams
	resolved     resource.ResolvedResources
	sockOpts     sockopts.Options
	bufGroup     *BufferGroup
	logger       *slog.Logger
	cfg          resource.Config
	ready        chan error
	acceptPaused *atomic.Bool

	reqCount    *atomic.Uint64
	activeConns *atomic.Int64
	errCount    *atomic.Uint64
}

func newWorker(id, cpuID int, tier TierStrategy, handler stream.Handler,
	objective resource.ObjectiveParams, resolved resource.ResolvedResources,
	cfg resource.Config, reqCount *atomic.Uint64, activeConns *atomic.Int64, errCount *atomic.Uint64,
	acceptPaused *atomic.Bool) (*Worker, error) {

	// Only create the listen socket here. Ring creation is deferred to run()
	// because SINGLE_ISSUER requires all ring operations from the creating thread.
	listenFD, err := createListenSocket(cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("worker %d: listen socket: %w", id, err)
	}

	return &Worker{
		id:           id,
		cpuID:        cpuID,
		listenFD:     listenFD,
		tier:         tier,
		conns:        make(map[int]*connState),
		handler:      handler,
		objective:    objective,
		resolved:     resolved,
		cfg:          cfg,
		logger:       cfg.Logger,
		reqCount:     reqCount,
		activeConns:  activeConns,
		errCount:     errCount,
		acceptPaused: acceptPaused,
		ready:        make(chan error, 1),
		sockOpts: sockopts.Options{
			TCPNoDelay:  objective.TCPNoDelay,
			TCPQuickAck: objective.TCPQuickAck,
			SOBusyPoll:  objective.SOBusyPoll,
			RecvBuf:     resolved.SocketRecv,
			SendBuf:     resolved.SocketSend,
		},
	}, nil
}

func (w *Worker) run(ctx context.Context) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	_ = platform.PinToCPU(w.cpuID)

	// Create ring after LockOSThread — SINGLE_ISSUER requires all ring
	// operations from the same OS thread.
	ring, err := NewRing(uint32(w.resolved.SQERingSize), w.tier.SetupFlags())
	if err != nil {
		w.ready <- fmt.Errorf("worker %d ring setup: %w", w.id, err)
		return
	}
	w.ring = ring

	if w.tier.SupportsProvidedBuffers() {
		bg, bgErr := NewBufferGroup(w.ring, 0, w.resolved.BufferPool, w.resolved.BufferSize)
		if bgErr != nil {
			w.logger.Warn("failed to setup provided buffers, falling back", "err", bgErr)
		} else {
			w.bufGroup = bg
		}
	}

	w.tier.PrepareAccept(w.ring, w.listenFD)
	if _, err := w.ring.Submit(); err != nil {
		w.ready <- fmt.Errorf("worker %d initial submit: %w", w.id, err)
		return
	}

	w.ready <- nil

	for {
		if ctx.Err() != nil {
			w.shutdown()
			return
		}

		// Non-blocking peek for CQEs, fall back to timed wait.
		if w.ring.peekCQE() == nil {
			// Submit+Wait with timeout so we periodically check for cancellation.
			if err := w.ring.SubmitAndWaitTimeout(100 * time.Millisecond); err != nil {
				w.shutdown()
				return
			}
		}

		processed := 0
		for processed < w.objective.CQBatch {
			entry := w.ring.peekCQE()
			if entry == nil {
				break
			}
			w.processCQE(ctx, entry)
			w.ring.AdvanceCQ()
			processed++
		}

		if _, err := w.ring.Submit(); err != nil {
			w.logger.Error("submit failed", "worker", w.id, "err", err)
		}
	}
}

func (w *Worker) processCQE(ctx context.Context, c *completionEntry) {
	op := decodeOp(c.UserData)
	fd := decodeFD(c.UserData)

	switch op {
	case udAccept:
		w.handleAccept(ctx, c, fd)
	case udRecv:
		w.handleRecv(c, fd)
	case udSend:
		w.handleSend(c, fd)
	case udClose:
		w.handleClose(fd)
	case udPeek:
		w.handlePeek(c, fd)
	case udProvide:
		// Buffer provide completion, no action needed
	}
}

func (w *Worker) handleAccept(ctx context.Context, c *completionEntry, listenFD int) {
	if c.Res < 0 {
		w.errCount.Add(1)
		if !w.tier.SupportsMultishotAccept() {
			w.tier.PrepareAccept(w.ring, listenFD)
		}
		return
	}

	newFD := int(c.Res)

	if w.acceptPaused.Load() {
		_ = unix.Close(newFD)
		if !cqeHasMore(c.Flags) && !w.tier.SupportsMultishotAccept() {
			w.tier.PrepareAccept(w.ring, listenFD)
		}
		return
	}

	_ = sockopts.ApplyFD(newFD, w.sockOpts)

	cs := newConnState(ctx, newFD, w.resolved.BufferSize)
	w.conns[newFD] = cs
	w.activeConns.Add(1)

	if w.cfg.Protocol == engine.Auto {
		sqe := w.ring.GetSQE()
		if sqe != nil {
			prepRecvPeek(sqe, newFD, cs.buf[:detect.MinPeekBytes])
			setSQEUserData(sqe, encodeUserData(udPeek, newFD))
		}
	} else {
		cs.protocol = w.cfg.Protocol
		cs.detected = true
		w.initProtocol(cs)
		w.tier.PrepareRecv(w.ring, newFD, cs.buf)
	}

	if !cqeHasMore(c.Flags) && !w.tier.SupportsMultishotAccept() {
		w.tier.PrepareAccept(w.ring, listenFD)
	}
}

func (w *Worker) handlePeek(c *completionEntry, fd int) {
	cs, ok := w.conns[fd]
	if !ok {
		return
	}

	if c.Res < int32(detect.MinPeekBytes) {
		sqe := w.ring.GetSQE()
		if sqe != nil {
			prepRecvPeek(sqe, fd, cs.buf[:24])
			setSQEUserData(sqe, encodeUserData(udPeek, fd))
		}
		return
	}

	proto, err := detect.Detect(cs.buf[:c.Res])
	if err == detect.ErrInsufficientData {
		sqe := w.ring.GetSQE()
		if sqe != nil {
			prepRecvPeek(sqe, fd, cs.buf[:24])
			setSQEUserData(sqe, encodeUserData(udPeek, fd))
		}
		return
	}
	if err != nil {
		w.closeConn(fd)
		return
	}

	cs.protocol = proto
	cs.detected = true
	w.initProtocol(cs)
	w.tier.PrepareRecv(w.ring, fd, cs.buf)
}

func (w *Worker) initProtocol(cs *connState) {
	switch cs.protocol {
	case engine.HTTP1:
		cs.h1State = conn.NewH1State()
	case engine.H2C:
		writeFn := w.makeWriteFn(cs.fd)
		cs.h2State = conn.NewH2State(w.handler, conn.H2Config{
			MaxConcurrentStreams: w.cfg.MaxConcurrentStreams,
			InitialWindowSize:    w.cfg.InitialWindowSize,
			MaxFrameSize:         w.cfg.MaxFrameSize,
		}, writeFn)
	}
}

func (w *Worker) handleRecv(c *completionEntry, fd int) {
	cs, ok := w.conns[fd]
	if !ok || cs.closing {
		return
	}

	if c.Res <= 0 {
		w.closeConn(fd)
		return
	}

	data := cs.buf[:c.Res]

	if cqeHasBuffer(c.Flags) && w.bufGroup != nil {
		bufID := cqeBufferID(c.Flags)
		provBuf := w.bufGroup.GetBuffer(bufID)
		if provBuf != nil {
			data = provBuf[:c.Res]
			defer func() { _ = w.bufGroup.ReturnBuffer(w.ring, bufID) }()
		}
	}

	writeFn := w.makeWriteFn(fd)

	var processErr error
	switch cs.protocol {
	case engine.HTTP1:
		processErr = conn.ProcessH1(cs.ctx, data, cs.h1State, w.handler, writeFn)
	case engine.H2C:
		processErr = conn.ProcessH2(cs.ctx, data, cs.h2State, w.handler, writeFn, conn.H2Config{
			MaxConcurrentStreams: w.cfg.MaxConcurrentStreams,
			InitialWindowSize:    w.cfg.InitialWindowSize,
			MaxFrameSize:         w.cfg.MaxFrameSize,
		})
	}

	w.reqCount.Add(1)

	if processErr != nil {
		// Submit pending SEND SQEs (e.g. error responses) before closing
		// so the kernel processes the send before the close.
		if _, err := w.ring.Submit(); err != nil {
			w.logger.Error("submit failed", "worker", w.id, "err", err)
		}
		w.closeConn(fd)
		return
	}

	if !cqeHasMore(c.Flags) {
		w.tier.PrepareRecv(w.ring, fd, cs.buf)
	}
}

func (w *Worker) handleSend(c *completionEntry, fd int) {
	cs, ok := w.conns[fd]
	if !ok {
		return
	}

	if c.Res < 0 {
		w.errCount.Add(1)
		cs.sendQueue = cs.sendQueue[:0]
		if cs.closing {
			w.finishClose(fd)
		} else {
			w.closeConn(fd)
		}
		return
	}

	// Handle partial sends by re-sending the remainder.
	sent := int(c.Res)
	if len(cs.sendQueue) > 0 {
		buf := cs.sendQueue[0]
		if sent < len(buf) {
			remaining := buf[sent:]
			newBuf := make([]byte, len(remaining))
			copy(newBuf, remaining)
			cs.sendQueue[0] = newBuf
			w.tier.PrepareSend(w.ring, fd, newBuf, false)
			return
		}
		cs.sendQueue = cs.sendQueue[1:]
	}

	if cs.closing && len(cs.sendQueue) == 0 {
		w.finishClose(fd)
	}
}

func (w *Worker) handleClose(fd int) {
	delete(w.conns, fd)
	w.activeConns.Add(-1)
}

func (w *Worker) closeConn(fd int) {
	cs, ok := w.conns[fd]
	if !ok {
		return
	}
	if cs.h2State != nil {
		conn.CloseH2(cs.h2State)
	}
	cs.cancel()

	// Defer actual close until all in-flight SEND SQEs complete,
	// so GOAWAY / RST_STREAM data reaches the client.
	if len(cs.sendQueue) > 0 {
		cs.closing = true
		return
	}

	w.finishClose(fd)
}

func (w *Worker) finishClose(fd int) {
	delete(w.conns, fd)
	w.activeConns.Add(-1)

	// Half-close before full close: shutdown(SHUT_WR) sends FIN after all
	// pending data in the socket buffer, preventing RST from discarding
	// unsent GOAWAY / RST_STREAM frames.
	_ = unix.Shutdown(fd, unix.SHUT_WR)
	// Drain receive buffer to prevent RST from discarding unsent data
	// (close() with unread data in recv buffer causes RST instead of FIN).
	drainRecvBuffer(fd)

	sqe := w.ring.GetSQE()
	if sqe != nil {
		prepClose(sqe, fd)
		setSQEUserData(sqe, encodeUserData(udClose, fd))
	}
}

func (w *Worker) makeWriteFn(fd int) func([]byte) {
	return func(data []byte) {
		// Copy data — the caller may reuse the underlying buffer (e.g. sync.Pool)
		// before the kernel processes the SEND SQE.
		copied := make([]byte, len(data))
		copy(copied, data)
		if cs, ok := w.conns[fd]; ok {
			cs.sendQueue = append(cs.sendQueue, copied)
		}
		w.tier.PrepareSend(w.ring, fd, copied, false)
	}
}

func (w *Worker) shutdown() {
	for fd, cs := range w.conns {
		if cs.h2State != nil {
			conn.CloseH2(cs.h2State)
		}
		cs.cancel()
		_ = unix.Close(fd)
	}
	_ = unix.Close(w.listenFD)
	if w.ring != nil {
		_ = w.ring.Close()
	}
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

	if host == "::" {
		return &unix.SockaddrInet6{Port: port}, nil
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
