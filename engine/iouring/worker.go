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

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/internal/conn"
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

// bufRingGroupID is the provided buffer ring group ID.
const bufRingGroupID = 0

// bufRingCount is the number of buffers in the provided buffer ring.
// Must be a power of 2.
const bufRingCount = 4096

// Worker is a per-core io_uring event loop.
type Worker struct {
	id           int
	cpuID        int
	ring         *Ring
	listenFD     int
	tier         TierStrategy
	fixedFiles   bool // runtime flag: true if ACCEPT_DIRECT is working
	sqpoll       bool // true when SQPOLL is active (kernel submits SQEs)
	conns        []*connState
	connCount    int // number of active connections (local, for draining check)
	maxFD        int // upper bound fd for iteration in checkTimeouts/shutdown
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
	reqBatch    uint64 // batched request count, flushed to reqCount per iteration
	tickCounter uint32

	dirtyHead     *connState // head of intrusive doubly-linked dirty list
	hasBufReturns bool       // set when provided buffers need publishing
	h2cfg         conn.H2Config
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
		sqpoll:       tier.SQPollIdle() > 0,
		conns:        make([]*connState, fixedFileTableSize),
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
		h2cfg: conn.H2Config{
			MaxConcurrentStreams: cfg.MaxConcurrentStreams,
			InitialWindowSize:    cfg.InitialWindowSize,
			MaxFrameSize:         cfg.MaxFrameSize,
		},
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
	ring, err := NewRing(uint32(w.resolved.SQERingSize), w.tier.SetupFlags(), w.tier.SQPollIdle())
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

		// Submit pending SENDs from previous iteration + wait for CQEs in
		// a single io_uring_enter syscall. Skip when CQEs are already ready
		// and nothing is pending (pure userspace fast path).
		hasPending := w.ring.Pending() > 0
		cqHead, cqTail := w.ring.BeginCQ()
		if hasPending || cqHead == cqTail {
			waitTimeout := 100 * time.Millisecond
			if w.listenFD < 0 {
				waitTimeout = 1 * time.Second
			}
			if err := w.ring.SubmitAndWaitTimeout(waitTimeout); err != nil {
				w.shutdown()
				return
			}
			cqHead, cqTail = w.ring.BeginCQ()
		}

		if cqHead != cqTail {
			now := time.Now().UnixNano()
			for processed := 0; processed < w.objective.CQBatch && cqHead != cqTail; processed++ {
				entry := w.ring.cqeAt(cqHead)
				w.processCQE(ctx, entry, now)
				cqHead++
			}
		}
		w.ring.EndCQ(cqHead)

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

		// Retry pending sends on dirty connections (SQ ring was full earlier).
		for cs := w.dirtyHead; cs != nil; {
			next := cs.dirtyNext
			if !cs.sending {
				w.flushSend(cs)
			}
			// Remove from dirty list if fully flushed (no pending data).
			if len(cs.sendBuf) == 0 && len(cs.writeBuf) == 0 {
				w.removeDirty(cs)
			}
			cs = next
		}

		// Submit SEND SQEs. With SQPOLL, the kernel thread polls the SQ ring
		// automatically — no Submit syscall needed. Without SQPOLL, submit
		// immediately to avoid deferring SENDs to the next iteration (~10%
		// throughput loss under H1 keep-alive).
		if w.sqpoll {
			w.ring.ClearPending()
		} else if w.ring.Pending() > 0 {
			if _, err := w.ring.Submit(); err != nil {
				w.logger.Error("submit failed", "worker", w.id, "err", err)
			}
		}

		// Check connection timeouts every 1024 iterations to amortize
		// time.Now() cost. Scans active connections directly — no timer
		// wheel entries, no allocations, no map writes on the hot path.
		w.tickCounter++
		if w.tickCounter&0x3FF == 0 {
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
	case udProvide:
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
	isFixedFile := w.fixedFiles

	// Bounds check: reject FDs outside the flat conn array.
	if newFD < 0 || newFD >= len(w.conns) {
		w.errCount.Add(1)
		return
	}

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

	bufSize := w.resolved.BufferSize
	if w.bufRing != nil {
		bufSize = 0
	}
	cs := acquireConnState(ctx, newFD, bufSize)
	cs.fixedFile = isFixedFile

	if !isFixedFile {
		if sa, err := unix.Getpeername(newFD); err == nil {
			cs.remoteAddr = sockaddrString(sa)
		}
	}
	// For fixed files (ACCEPT_DIRECT), the CQE result is a fixed file index,
	// not a real FD. getpeername is not available without a real FD.

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
		if !cs.fixedFile {
			cs.h1State.HijackFn = func() (net.Conn, error) {
				return w.hijackConn(cs.fd)
			}
		}
	case engine.H2C:
		cs.h2State = conn.NewH2State(w.handler, w.h2cfg, cs.writeFn)
		cs.h2State.SetRemoteAddr(cs.remoteAddr)
	}
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
		// ENOBUFS (-105): provided buffer ring exhausted. The multishot recv
		// is terminated by the kernel but the connection is healthy. Re-arm
		// multishot recv — buffers will be available after current batch is
		// returned via PublishBuffers().
		if c.Res == -105 && w.bufRing != nil {
			w.bufRing.PublishBuffers()
			sqe := w.ring.GetSQE()
			if sqe != nil {
				prepMultishotRecv(sqe, fd, bufRingGroupID, cs.fixedFile)
				setSQEUserData(sqe, encodeUserData(udRecv, fd))
			}
			return
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

	cs.lastActivity = now

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

	var processErr error
	switch cs.protocol {
	case engine.HTTP1:
		processErr = conn.ProcessH1(cs.ctx, data, cs.h1State, w.handler, cs.writeFn)
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
		w.flushSend(cs)
		w.closeConn(fd)
		return
	}

	// Back-pressure: close connection if pending data grew too large.
	if len(cs.writeBuf)+len(cs.sendBuf) > maxSendQueueBytes {
		w.closeConn(fd)
		return
	}

	w.flushSend(cs)

	// For multishot recv, CQE_F_MORE means the kernel will produce more CQEs
	// without needing a new SQE. Only re-arm if multishot ended.
	if !cqeHasMore(c.Flags) {
		w.prepareRecv(fd, cs.buf)
	}
}

func (w *Worker) handleSend(c *completionEntry, fd int, now int64) {
	cs := w.conns[fd]
	if cs == nil {
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

	// All data sent — remove from dirty list, update activity timestamp.
	if len(cs.sendBuf) == 0 && len(cs.writeBuf) == 0 {
		w.removeDirty(cs)
		cs.lastActivity = now
	}

	// Re-send remainder or flush new data.
	w.flushSend(cs)
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
	w.removeDirty(cs)
	if cs.h1State != nil {
		conn.CloseH1(cs.h1State)
	}
	if cs.h2State != nil {
		conn.CloseH2(cs.h2State)
	}

	// Defer actual close until all in-flight and pending SENDs complete,
	// so GOAWAY / RST_STREAM data reaches the client.
	if cs.sending || len(cs.sendBuf) > 0 || len(cs.writeBuf) > 0 {
		cs.closing = true
		w.flushSend(cs)
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

	fixedFile := cs != nil && cs.fixedFile
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

func (w *Worker) makeWriteFn(cs *connState) func([]byte) {
	return func(data []byte) {
		if cs.closing {
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
func (w *Worker) flushSend(cs *connState) {
	if cs.sending {
		return
	}

	// If sendBuf still has data (partial send remainder), re-send it.
	if len(cs.sendBuf) > 0 {
		sqe := w.ring.GetSQE()
		if sqe == nil {
			return // SQ ring full — will retry next iteration
		}
		if cs.fixedFile {
			prepSendFixed(sqe, cs.fd, cs.sendBuf, false)
		} else {
			prepSendPlain(sqe, cs.fd, cs.sendBuf, false)
		}
		setSQEUserData(sqe, encodeUserData(udSend, cs.fd))
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
	if cs.fixedFile {
		prepSendFixed(sqe, cs.fd, cs.sendBuf, false)
	} else {
		prepSendPlain(sqe, cs.fd, cs.sendBuf, false)
	}
	setSQEUserData(sqe, encodeUserData(udSend, cs.fd))
	cs.sending = true
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
	for fd := 0; fd <= w.maxFD; fd++ {
		cs := w.conns[fd]
		if cs == nil {
			continue
		}
		if cs.h1State != nil {
			conn.CloseH1(cs.h1State)
		}
		if cs.h2State != nil {
			conn.CloseH2(cs.h2State)
		}
		if !cs.fixedFile {
			_ = unix.Close(fd)
		}
		releaseConnState(cs)
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
