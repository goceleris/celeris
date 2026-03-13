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
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/platform"
	"github.com/goceleris/celeris/internal/sockopts"
	"github.com/goceleris/celeris/internal/timer"
	"github.com/goceleris/celeris/protocol/detect"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"

	"golang.org/x/sys/unix"
)

// fixedFileTableSize is the number of slots in the fixed file table.
// Must accommodate the maximum number of concurrent connections per worker.
const fixedFileTableSize = 65536

// bufRingGroupID is the provided buffer ring group ID.
const bufRingGroupID = 0

// bufRingCount is the number of buffers in the provided buffer ring.
// Must be a power of 2.
const bufRingCount = 4096

// sendPrepFn prepares a SEND SQE. Two variants exist: one for plain FDs and
// one for fixed files. The worker selects the variant once at startup based on
// tier capabilities, eliminating a per-send branch.
type sendPrepFn func(sqePtr unsafe.Pointer, fd int, buf []byte, linked bool)

// Worker is a per-core io_uring event loop.
type Worker struct {
	id           int
	cpuID        int
	ring         *Ring
	listenFD     int
	tier         TierStrategy
	fixedFiles   bool // runtime flag: true if ACCEPT_DIRECT is working
	conns        map[int]*connState
	handler      stream.Handler
	objective    resource.ObjectiveParams
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
	tw          *timer.Wheel
	tickCounter uint32

	dirtyHead *connState // head of intrusive doubly-linked dirty list
	prepSend  sendPrepFn // selected at startup: plain or fixed-file variant
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
		wake:         make(chan struct{}),
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

	// Register fixed file table if the tier supports it.
	if w.tier.SupportsFixedFiles() {
		if err := w.ring.RegisterFiles(fixedFileTableSize); err != nil {
			w.logger.Warn("fixed file table registration failed, falling back",
				"worker", w.id, "err", err)
		} else {
			w.fixedFiles = true
		}
	}

	// Select send-prep function once based on fixed file support (P3).
	if w.fixedFiles {
		w.prepSend = prepSendFixed
	} else {
		w.prepSend = prepSendPlain
	}

	// Register ring-mapped provided buffers for multishot recv.
	if w.tier.SupportsMultishotRecv() {
		br, err := NewBufferRing(w.ring, bufRingGroupID, bufRingCount, w.resolved.BufferSize)
		if err != nil {
			w.logger.Warn("ring-mapped buffer registration failed, using per-connection buffers",
				"worker", w.id, "err", err)
		} else {
			w.bufRing = br
		}
	}

	w.tw = timer.New(func(fd int, _ timer.TimeoutKind) {
		w.closeConn(fd)
	})

	w.prepareAccept()
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

		// ACTIVE → DRAINING: cancel pending io_uring operations on the listen
		// socket, then close it. The cancel releases the kernel's io_uring
		// reference to the underlying file, allowing the socket to leave the
		// SO_REUSEPORT group immediately. Without this, unix.Close alone
		// leaves a phantom socket that intercepts connections.
		if w.listenFD >= 0 && w.acceptPaused.Load() {
			if sqe := w.ring.GetSQE(); sqe != nil {
				prepCancelFDSkipSuccess(sqe, w.listenFD)
				setSQEUserData(sqe, 0)
				// Submit and wait for the cancel to complete before closing.
				_ = w.ring.SubmitAndWaitTimeout(50 * time.Millisecond)
				// Process CQEs: skip cancel completions (userData=0),
				// handle everything else normally to avoid breaking
				// active connections.
				for {
					entry := w.ring.peekCQE()
					if entry == nil {
						break
					}
					if entry.UserData != 0 {
						w.processCQE(ctx, entry)
					}
					w.ring.AdvanceCQ()
				}
			}
			_ = unix.Close(w.listenFD)
			w.listenFD = -1
		}

		// SUSPENDED → ACTIVE: re-create listen socket after ResumeAccept.
		if w.listenFD < 0 && !w.acceptPaused.Load() {
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

		// Non-blocking peek for CQEs, fall back to timed wait.
		if w.ring.peekCQE() == nil {
			waitTimeout := 100 * time.Millisecond
			if w.listenFD < 0 {
				waitTimeout = 1 * time.Second
			}
			if err := w.ring.SubmitAndWaitTimeout(waitTimeout); err != nil {
				w.shutdown()
				return
			}
		}

		processed := 0
		hasBufReturns := false
		for processed < w.objective.CQBatch {
			entry := w.ring.peekCQE()
			if entry == nil {
				break
			}
			w.processCQE(ctx, entry)
			// Track whether any buffer returns were batched (P0).
			if decodeOp(entry.UserData) == udRecv && cqeHasBuffer(entry.Flags) && w.bufRing != nil {
				hasBufReturns = true
			}
			w.ring.AdvanceCQ()
			processed++
		}

		// Single atomic publish for all batched buffer returns (P0).
		if hasBufReturns {
			w.bufRing.PublishBuffers()
		}

		// Retry pending sends on dirty connections (SQ ring was full earlier).
		for cs := w.dirtyHead; cs != nil; {
			next := cs.dirtyNext
			if !cs.sending {
				w.flushSend(cs.fd)
			}
			// Remove from dirty list if fully flushed (no pending data).
			if len(cs.sendBuf) == 0 && len(cs.writeBuf) == 0 {
				w.removeDirty(cs)
			}
			cs = next
		}

		// Throttle timer wheel tick: 100ms granularity doesn't need per-iteration
		// calls. Tick every 1024 iterations to amortize time.Now() cost (P1).
		w.tickCounter++
		if w.tickCounter&0x3FF == 0 {
			w.tw.Tick()
		}

		if _, err := w.ring.Submit(); err != nil {
			w.logger.Error("submit failed", "worker", w.id, "err", err)
		}

		// DRAINING → SUSPENDED: no listen socket, no connections, CQEs processed.
		// Checked after CQE processing so accept CQEs for connections that
		// completed before the listen socket close are served, not leaked.
		if w.listenFD < 0 && len(w.conns) == 0 && w.acceptPaused.Load() {
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
	case udProvide:
		// Buffer provide completion, no action needed
	}
}

func (w *Worker) handleAccept(ctx context.Context, c *completionEntry, _ int) {
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
	isFixedFile := w.fixedFiles

	// Don't discard accepted connections even when paused — the TCP handshake
	// already completed and the client expects a response. The listen socket
	// will be closed within one event loop iteration to prevent further accepts.

	if !isFixedFile {
		_ = sockopts.ApplyFD(newFD, w.sockOpts)
	}
	// For fixed files, socket options were applied by the kernel at accept time
	// via inherited options. TCP_NODELAY etc. must be set post-accept for
	// non-inherited options — but with fixed files (ACCEPT_DIRECT), the fd field
	// is actually a fixed file index and we can't call setsockopt on it directly.
	// Socket options that require per-connection setsockopt are skipped for fixed files.

	cs := newConnState(ctx, newFD, w.resolved.BufferSize)
	cs.fixedFile = isFixedFile

	// When using multishot recv with ring-mapped buffers, we don't need a
	// per-connection recv buffer.
	if w.bufRing != nil {
		cs.buf = nil
	}

	if !isFixedFile {
		if sa, err := unix.Getpeername(newFD); err == nil {
			cs.remoteAddr = sockaddrString(sa)
		}
	}
	// For fixed files (ACCEPT_DIRECT), the CQE result is a fixed file index,
	// not a real FD. getpeername is not available without a real FD.

	w.conns[newFD] = cs
	w.activeConns.Add(1)

	if w.cfg.OnConnect != nil {
		w.cfg.OnConnect(cs.remoteAddr)
	}

	if w.cfg.IdleTimeout > 0 {
		w.tw.ScheduleAt(newFD, w.cfg.IdleTimeout, timer.IdleTimeout, time.Now().UnixNano())
	}

	if w.cfg.Protocol != engine.Auto {
		cs.protocol = w.cfg.Protocol
		cs.detected = true
		w.initProtocol(cs)
	}
	// For Auto mode, cs.detected is false; the first handleRecv will
	// detect the protocol from the received data before processing it.
	w.prepareRecv(newFD, cs.buf)

	if !cqeHasMore(c.Flags) && !w.tier.SupportsMultishotAccept() && w.listenFD >= 0 {
		w.prepareAccept()
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
	cs, ok := w.conns[fd]
	if !ok {
		return nil, errors.New("celeris: connection not found")
	}
	if cs.fixedFile {
		return nil, errors.New("celeris: cannot hijack fixed file connection")
	}
	if cs.sending || len(cs.sendBuf) > 0 || len(cs.writeBuf) > 0 {
		return nil, errors.New("celeris: cannot hijack with pending sends")
	}
	w.tw.Cancel(fd)
	cs.cancel()
	delete(w.conns, fd)
	w.activeConns.Add(-1)
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
		if !cs.fixedFile {
			cs.h1State.HijackFn = func() (net.Conn, error) {
				return w.hijackConn(cs.fd)
			}
		}
	case engine.H2C:
		writeFn := w.makeWriteFn(cs.fd)
		cs.h2State = conn.NewH2State(w.handler, conn.H2Config{
			MaxConcurrentStreams: w.cfg.MaxConcurrentStreams,
			InitialWindowSize:    w.cfg.InitialWindowSize,
			MaxFrameSize:         w.cfg.MaxFrameSize,
		}, writeFn)
		cs.h2State.SetRemoteAddr(cs.remoteAddr)
	}
}

func (w *Worker) handleRecv(c *completionEntry, fd int) {
	cs, ok := w.conns[fd]
	if !ok || cs.closing {
		// If multishot recv with provided buffers, batch-return the buffer
		// even for unknown/closing connections to prevent buffer leak (P0).
		if cqeHasBuffer(c.Flags) && w.bufRing != nil {
			w.bufRing.PushBuffer(cqeBufferID(c.Flags))
		}
		return
	}

	if c.Res <= 0 {
		if cqeHasBuffer(c.Flags) && w.bufRing != nil {
			w.bufRing.PushBuffer(cqeBufferID(c.Flags))
		}
		w.closeConn(fd)
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

	// Cancel idle timer; we're actively receiving data.
	now := time.Now().UnixNano()
	if w.cfg.ReadTimeout > 0 {
		w.tw.ScheduleAt(fd, w.cfg.ReadTimeout, timer.ReadTimeout, now)
	} else {
		w.tw.Cancel(fd)
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
				w.prepareRecv(fd, cs.buf)
			}
			return
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

	// Batch-return the provided buffer after processing. The data has been
	// consumed by the protocol handler. Actual publish happens after the CQE
	// drain loop completes (P0).
	if hasProvidedBuf {
		w.bufRing.PushBuffer(providedBufID)
	}

	w.reqCount.Add(1)

	// Schedule write timeout if response data is pending; otherwise idle timeout.
	// Reuse the timestamp captured above (P9).
	if w.cfg.WriteTimeout > 0 && len(cs.writeBuf) > 0 {
		w.tw.ScheduleAt(fd, w.cfg.WriteTimeout, timer.WriteTimeout, now)
	} else if w.cfg.IdleTimeout > 0 {
		w.tw.ScheduleAt(fd, w.cfg.IdleTimeout, timer.IdleTimeout, now)
	}

	if processErr != nil {
		if errors.Is(processErr, conn.ErrHijacked) {
			return // FD already detached
		}
		// Flush pending writes (e.g. error responses) before closing.
		w.flushSend(fd)
		if _, err := w.ring.Submit(); err != nil {
			w.logger.Error("submit failed", "worker", w.id, "err", err)
		}
		w.closeConn(fd)
		return
	}

	// Back-pressure: close connection if pending data grew too large.
	if len(cs.writeBuf)+len(cs.sendBuf) > maxSendQueueBytes {
		w.closeConn(fd)
		return
	}

	w.flushSend(fd)

	// For multishot recv, CQE_F_MORE means the kernel will produce more CQEs
	// without needing a new SQE. Only re-arm if multishot ended.
	if !cqeHasMore(c.Flags) {
		w.prepareRecv(fd, cs.buf)
	}
}

func (w *Worker) handleSend(c *completionEntry, fd int) {
	cs, ok := w.conns[fd]
	if !ok {
		return
	}

	cs.sending = false

	if c.Res < 0 {
		w.errCount.Add(1)
		cs.sendBuf = cs.sendBuf[:0]
		cs.writeBuf = cs.writeBuf[:0]
		if cs.closing {
			w.finishClose(fd)
		} else {
			w.closeConn(fd)
		}
		return
	}

	// Handle partial sends by shifting remaining data to the front.
	sent := int(c.Res)
	if sent < len(cs.sendBuf) {
		remaining := len(cs.sendBuf) - sent
		copy(cs.sendBuf, cs.sendBuf[sent:])
		cs.sendBuf = cs.sendBuf[:remaining]
	} else {
		cs.sendBuf = cs.sendBuf[:0]
	}

	if cs.closing && len(cs.sendBuf) == 0 && len(cs.writeBuf) == 0 {
		w.finishClose(fd)
		return
	}

	// All data sent — remove from dirty list, cancel write timeout, restore idle timeout.
	if len(cs.sendBuf) == 0 && len(cs.writeBuf) == 0 {
		w.removeDirty(cs)
		if w.cfg.IdleTimeout > 0 {
			w.tw.ScheduleAt(fd, w.cfg.IdleTimeout, timer.IdleTimeout, time.Now().UnixNano())
		}
	}

	// Re-send remainder or flush new data.
	w.flushSend(fd)
}

func (w *Worker) handleClose(fd int) {
	// finishClose already removed from conns and decremented activeConns.
	// With CQE_SKIP_SUCCESS, this handler may not fire for successful close.
	// Kept as a safety guard for error CQEs.
	delete(w.conns, fd)
}

func (w *Worker) closeConn(fd int) {
	cs, ok := w.conns[fd]
	if !ok {
		return
	}
	w.removeDirty(cs)
	w.tw.Cancel(fd)
	if cs.h2State != nil {
		conn.CloseH2(cs.h2State)
	}
	cs.cancel()

	// Defer actual close until all in-flight and pending SENDs complete,
	// so GOAWAY / RST_STREAM data reaches the client.
	if cs.sending || len(cs.sendBuf) > 0 || len(cs.writeBuf) > 0 {
		cs.closing = true
		w.flushSend(fd)
		return
	}

	w.finishClose(fd)
}

func (w *Worker) finishClose(fd int) {
	cs := w.conns[fd]
	delete(w.conns, fd)
	w.activeConns.Add(-1)

	if w.cfg.OnDisconnect != nil && cs != nil {
		w.cfg.OnDisconnect(cs.remoteAddr)
	}

	if cs != nil && cs.fixedFile {
		// Fixed file: close via io_uring direct close (no real FD to shutdown).
		sqe := w.ring.GetSQE()
		if sqe != nil {
			prepCloseDirect(sqe, fd)
			setSQEUserData(sqe, encodeUserData(udClose, fd))
		}
		return
	}

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
		cs, ok := w.conns[fd]
		if !ok {
			return
		}
		// Back-pressure: drop writes when total pending data exceeds limit.
		// The connection will be closed after processing completes.
		if len(cs.writeBuf)+len(cs.sendBuf) > maxSendQueueBytes {
			return
		}
		// Append to writeBuf — no per-write allocation. The kernel holds
		// sendBuf (not writeBuf), so appending here is safe.
		cs.writeBuf = append(cs.writeBuf, data...)

		// Immediate flush for the common single-response case (P10):
		// If no send is in-flight and connection isn't already dirty, submit
		// the SEND SQE immediately. This bypasses the dirty list traversal
		// for the vast majority of requests.
		if !cs.sending && !cs.dirty {
			w.flushSend(fd)
			return
		}
		w.markDirty(cs)
	}
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
func (w *Worker) prepareRecv(fd int, buf []byte) {
	sqe := w.ring.GetSQE()
	if sqe == nil {
		return
	}
	if w.bufRing != nil {
		prepMultishotRecv(sqe, fd, bufRingGroupID, w.fixedFiles)
	} else {
		prepRecv(sqe, fd, buf)
	}
	setSQEUserData(sqe, encodeUserData(udRecv, fd))
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
func (w *Worker) flushSend(fd int) {
	cs, ok := w.conns[fd]
	if !ok || cs.sending {
		return
	}

	// If sendBuf still has data (partial send remainder), re-send it.
	if len(cs.sendBuf) > 0 {
		sqe := w.ring.GetSQE()
		if sqe == nil {
			return // SQ ring full — will retry next iteration
		}
		w.prepSend(sqe, fd, cs.sendBuf, false)
		setSQEUserData(sqe, encodeUserData(udSend, fd))
		cs.sending = true
		return
	}

	// No in-flight data; swap writeBuf → sendBuf if there's new data.
	if len(cs.writeBuf) == 0 {
		return
	}

	cs.sendBuf, cs.writeBuf = cs.writeBuf, cs.sendBuf[:0]

	sqe := w.ring.GetSQE()
	if sqe == nil {
		// SQ ring full — swap back; will retry next iteration.
		cs.writeBuf, cs.sendBuf = cs.sendBuf, cs.writeBuf
		return
	}
	w.prepSend(sqe, fd, cs.sendBuf, false)
	setSQEUserData(sqe, encodeUserData(udSend, fd))
	cs.sending = true
}

func (w *Worker) shutdown() {
	for fd, cs := range w.conns {
		if cs.h2State != nil {
			conn.CloseH2(cs.h2State)
		}
		cs.cancel()
		if !cs.fixedFile {
			_ = unix.Close(fd)
		}
	}
	if w.listenFD >= 0 {
		_ = unix.Close(w.listenFD)
	}
	if w.bufRing != nil && w.ring != nil {
		w.bufRing.Close(w.ring)
	}
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
