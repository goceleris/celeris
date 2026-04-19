package memcached

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/goceleris/celeris/driver/internal/async"
	"github.com/goceleris/celeris/driver/memcached/protocol"
	"github.com/goceleris/celeris/engine"
)

// spinIterations is the number of spin loops the caller performs before
// parking on doneCh. The mix of tight spins and Gosched yields catches
// ultra-fast in-memory memcached responses without scheduler overhead.
const spinIterations = 64

// syncRoundTripper is the optional interface that the Linux event-loop worker
// implements. The memcached conn caches it at dial time to avoid repeated
// type assertions on the hot path.
type syncRoundTripper interface {
	WriteAndPoll(fd int, data []byte, rbuf []byte, onRecv func([]byte)) (ok bool, err error)
}

// syncBusyRoundTripper is the no-yield variant used by drivers opened
// WithEngine — caller is a LockOSThread'd celeris HTTP engine worker.
type syncBusyRoundTripper interface {
	WriteAndPollBusy(fd int, data []byte, rbuf []byte, onRecv func([]byte)) (ok bool, err error)
}

// syncMultiRoundTripper extends syncRoundTripper for workloads that poll until
// a completion predicate fires. Memcached uses this for GetMulti, which
// receives N VALUE blocks followed by one END.
type syncMultiRoundTripper interface {
	WriteAndPollMulti(fd int, data []byte, rbuf []byte, onRecv func([]byte), isDone func() bool, beforeRearm func()) (ok bool, err error)
}

// memcachedConn is a single connection driven by a celeris event loop. It
// owns an mcState decoder and exposes helper methods for encoding commands
// and awaiting replies.
//
// Two I/O modes coexist in the same struct:
//
//   - Engine-integrated mode (cfg.Engine != nil): fd + loop + sync-path
//     round-trips via WriteAndPoll / WriteAndPollBusy. Driver FDs share
//     the celeris HTTP engine's worker goroutine.
//   - Direct mode (cfg.Engine == nil): tcp is the live *net.TCPConn; all
//     I/O goes through it on the caller's goroutine. Go's netpoll parks
//     the goroutine on EPOLLIN transparently. Matches gomemcache's
//     shape; no mini-loop overhead.
//
// useDirect discriminates. Fields for the unused mode are zero.
type memcachedConn struct {
	useDirect bool

	// Direct-mode fields.
	tcp *net.TCPConn
	// tcpFd caches the raw fd for MSG_DONTWAIT peeks. Pre-netpoll
	// peek catches loopback-fast responses without the G-park wakeup;
	// zero when SyscallConn failed.
	tcpFd int

	// Engine-integrated fields.
	fd       int
	loop     engine.WorkerLoop
	workerID int
	// file pins the *os.File that wraps fd. Without this reference the File
	// becomes GC-eligible after dial and its finalizer closes fd, producing
	// sporadic "bad file descriptor" errors on subsequent writes.
	file *os.File

	// sync is non-nil when the worker supports synchronous round-trip
	// (write+poll on the caller's goroutine). Cached at dial time.
	sync syncRoundTripper
	// syncBusy is non-nil when the worker supports the no-yield busy-poll
	// variant. Drivers opened WithEngine route through this to avoid the
	// locked-M futex storm.
	syncBusy syncBusyRoundTripper
	// useBusy dispatches exec through syncBusy when set (WithEngine).
	useBusy bool
	// syncMulti is non-nil when the worker supports multi-response poll.
	syncMulti syncMultiRoundTripper
	// syncBuf is the per-conn read buffer used by the sync round-trip path.
	syncBuf []byte

	state *mcState
	cfg   Config

	// binary caches the dialect choice for fast checks on the hot path.
	binary bool

	// opaque is the next binary-protocol opaque token to assign. Only used
	// when binary is true.
	opaque atomic.Uint32

	// writerMu serializes encoder access + Write path on a single conn.
	writerMu sync.Mutex

	createdAt  time.Time
	lastUsedAt atomic.Int64

	maxLifetime time.Duration
	maxIdleTime time.Duration

	closeOnce sync.Once
	closed    atomic.Bool
	closeErr  atomic.Pointer[errBox]
}

// errBox wraps an error for atomic.Pointer — atomic.Value panics on type
// mismatch (io.EOF, *net.OpError, *errors.errorString hit the close path), so
// we normalize through a pointer-friendly wrapper.
type errBox struct{ err error }

// dialMemcachedConn dials a TCP connection, sets it non-blocking, registers it
// with the event loop, and (on binary protocol) sends a NOOP to validate the
// handshake. Text protocol is handshake-free; we trust the connect.
func dialMemcachedConn(ctx context.Context, prov engine.EventLoopProvider, cfg Config, workerHint int) (*memcachedConn, error) {
	timeout := cfg.DialTimeout
	if timeout <= 0 {
		timeout = defaultDialTimeout
	}
	dialer := net.Dialer{Timeout: timeout}
	raw, err := dialer.DialContext(ctx, "tcp", cfg.Addr)
	if err != nil {
		return nil, err
	}
	tcp, ok := raw.(*net.TCPConn)
	if !ok {
		_ = raw.Close()
		return nil, fmt.Errorf("celeris-memcached: expected *net.TCPConn, got %T", raw)
	}
	_ = tcp.SetNoDelay(true)

	file, err := tcp.File()
	if err != nil {
		_ = tcp.Close()
		return nil, err
	}
	fd := int(file.Fd())
	if err := syscall.SetNonblock(fd, true); err != nil {
		_ = tcp.Close()
		_ = file.Close()
		return nil, err
	}
	tuneConnSocket(fd)
	_ = tcp.Close()

	nw := prov.NumWorkers()
	if nw <= 0 {
		// Close via file.Close() so the *os.File's runtime finalizer is
		// disarmed. syscall.Close(fd) alone would leave the finalizer
		// armed; GC could then call syscall.Close(fd) a second time on an
		// fd the kernel has since reassigned.
		_ = file.Close()
		return nil, errors.New("celeris-memcached: event loop has 0 workers")
	}
	if workerHint < 0 || workerHint >= nw {
		workerHint = fd % nw
		if workerHint < 0 {
			workerHint = -workerHint
		}
	}
	loop := prov.WorkerLoop(workerHint)

	bin := cfg.Protocol == ProtocolBinary
	c := &memcachedConn{
		fd:        fd,
		loop:      loop,
		workerID:  workerHint,
		file:      file,
		state:     newMCState(bin),
		cfg:       cfg,
		binary:    bin,
		createdAt: time.Now(),
	}
	if srt, ok := loop.(syncRoundTripper); ok {
		c.sync = srt
	}
	if sbrt, ok := loop.(syncBusyRoundTripper); ok {
		c.syncBusy = sbrt
	}
	c.useBusy = cfg.Engine != nil && c.syncBusy != nil
	if smrt, ok := loop.(syncMultiRoundTripper); ok {
		c.syncMulti = smrt
	}
	c.lastUsedAt.Store(time.Now().UnixNano())

	if err := loop.RegisterConn(fd, c.onRecv, c.onClose); err != nil {
		_ = file.Close()
		return nil, err
	}

	if err := c.handshake(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// dialDirectMemcachedConn dials a TCP connection in direct mode — no
// mini-loop registration, no fd extraction. The conn keeps the live
// *net.TCPConn and performs Write/Read directly on the caller's goroutine.
func dialDirectMemcachedConn(ctx context.Context, cfg Config) (*memcachedConn, error) {
	timeout := cfg.DialTimeout
	if timeout <= 0 {
		timeout = defaultDialTimeout
	}
	dialer := net.Dialer{Timeout: timeout}
	raw, err := dialer.DialContext(ctx, "tcp", cfg.Addr)
	if err != nil {
		return nil, err
	}
	tcp, ok := raw.(*net.TCPConn)
	if !ok {
		_ = raw.Close()
		return nil, fmt.Errorf("celeris-memcached: expected *net.TCPConn, got %T", raw)
	}
	_ = tcp.SetNoDelay(true)

	var rawFd int
	if sc, scErr := tcp.SyscallConn(); scErr == nil {
		_ = sc.Control(func(fd uintptr) { rawFd = int(fd) })
	}

	bin := cfg.Protocol == ProtocolBinary
	c := &memcachedConn{
		useDirect: true,
		tcp:       tcp,
		tcpFd:     rawFd,
		state:     newMCState(bin),
		cfg:       cfg,
		binary:    bin,
		createdAt: time.Now(),
	}
	c.lastUsedAt.Store(time.Now().UnixNano())

	if err := c.handshake(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// onRecv runs on the worker goroutine.
func (c *memcachedConn) onRecv(data []byte) {
	if err := c.state.processRecv(data); err != nil {
		c.closeErr.Store(&errBox{err: err})
		c.closed.Store(true)
		c.state.drainWithError(err)
	}
}

// onClose runs on the worker goroutine when the FD closes.
func (c *memcachedConn) onClose(err error) {
	if err == nil {
		err = io.EOF
	}
	c.closeErr.Store(&errBox{err: err})
	c.closed.Store(true)
	c.state.drainWithError(err)
}

// handshake runs per-protocol validation. Text protocol has no handshake;
// for binary we issue a VERSION opcode to confirm the server speaks binary.
// (Some older stacks accept the TCP connection but send back a text ERROR
// for an unexpected binary magic byte — catching that here turns a mysterious
// "first command fails" into a dial-time error.)
func (c *memcachedConn) handshake(ctx context.Context) error {
	if !c.binary {
		return nil
	}
	req, err := c.execBinary(ctx, protocol.OpVersion, func(w *protocol.BinaryWriter, opaque uint32) []byte {
		return w.AppendSimple(protocol.OpVersion, opaque)
	})
	if err != nil {
		return err
	}
	defer putRequest(req)
	if req.resultErr != nil {
		return req.resultErr
	}
	return nil
}

// writeAndEnqueue enqueues req on the bridge and writes its serialized bytes
// onto the wire. The caller MUST hold writerMu and MUST have populated
// req.kind before calling.
func (c *memcachedConn) writeAndEnqueue(req *mcRequest, encode func() []byte) error {
	c.state.bridge.Enqueue(req)
	buf := encode()
	if err := c.loop.Write(c.fd, buf); err != nil {
		return err
	}
	return nil
}

// execText sends a text-protocol command and waits for the reply. kind is the
// expected reply shape.
func (c *memcachedConn) execText(ctx context.Context, kind replyKind, encode func(w *protocol.TextWriter) []byte) (*mcRequest, error) {
	if c.closed.Load() {
		return nil, ErrClosed
	}
	req := getRequest(ctx)
	req.kind = kind

	if c.useDirect {
		return c.execTextDirect(ctx, req, encode)
	}

	if c.sync != nil {
		c.writerMu.Lock()
		c.state.textW.Reset()
		buf := encode(c.state.textW)
		c.state.bridge.Enqueue(req)
		if c.syncBuf == nil {
			c.syncBuf = make([]byte, 16<<10)
		}
		var ok bool
		var err error
		if c.useBusy {
			ok, err = c.syncBusy.WriteAndPollBusy(c.fd, buf, c.syncBuf, c.onRecv)
		} else {
			ok, err = c.sync.WriteAndPoll(c.fd, buf, c.syncBuf, c.onRecv)
		}
		c.writerMu.Unlock()
		if err != nil {
			_ = c.closeWithErr(err)
			// Return the req so callers can putRequest it back to the pool
			// via `defer putRequest(req)` placed BEFORE the err check. The
			// request has already been finished by closeWithErr's
			// drainWithError; there is nothing more to do on it, but
			// dropping it on the floor would slowly drain reqPool under
			// sustained failures.
			return req, err
		}
		if ok {
			select {
			case <-req.doneCh:
				return req, req.resultErr
			default:
			}
		}
		return req, c.wait(ctx, req)
	}

	c.writerMu.Lock()
	c.state.textW.Reset()
	buf := encode(c.state.textW)
	werr := c.writeAndEnqueue(req, func() []byte { return buf })
	c.writerMu.Unlock()
	if werr != nil {
		_ = c.closeWithErr(werr)
		return req, werr
	}
	return req, c.wait(ctx, req)
}

// execBinary sends a binary-protocol command and waits for the reply. It
// returns the request object regardless of whether the server reported an
// application-level error (e.g. StatusKeyNotFound) — callers inspect
// req.binStatus / req.resultErr to map the reply to a typed sentinel. A
// non-nil err from execBinary indicates only transport-level failures
// (ctx cancellation, write error, closed conn); the req is still returned
// so the caller can release it back to the pool.
func (c *memcachedConn) execBinary(ctx context.Context, opcode byte, encode func(w *protocol.BinaryWriter, opaque uint32) []byte) (*mcRequest, error) {
	if c.closed.Load() {
		return nil, ErrClosed
	}
	_ = opcode // opcode is implicit in the encoder; keep the signature symmetrical for future validation.
	if c.useDirect {
		req := getRequest(ctx)
		req.kind = kindBinary
		req.opaque = c.opaque.Add(1)
		return c.execBinaryDirect(ctx, req, encode)
	}
	req := getRequest(ctx)
	req.kind = kindBinary
	req.opaque = c.opaque.Add(1)

	if c.sync != nil {
		c.writerMu.Lock()
		c.state.binW.Reset()
		buf := encode(c.state.binW, req.opaque)
		c.state.bridge.Enqueue(req)
		if c.syncBuf == nil {
			c.syncBuf = make([]byte, 16<<10)
		}
		var ok bool
		var err error
		if c.useBusy {
			ok, err = c.syncBusy.WriteAndPollBusy(c.fd, buf, c.syncBuf, c.onRecv)
		} else {
			ok, err = c.sync.WriteAndPoll(c.fd, buf, c.syncBuf, c.onRecv)
		}
		c.writerMu.Unlock()
		if err != nil {
			_ = c.closeWithErr(err)
			return req, err
		}
		if ok {
			select {
			case <-req.doneCh:
				return req, nil
			default:
			}
		}
		return req, c.waitTransport(ctx, req)
	}

	c.writerMu.Lock()
	c.state.binW.Reset()
	buf := encode(c.state.binW, req.opaque)
	werr := c.writeAndEnqueue(req, func() []byte { return buf })
	c.writerMu.Unlock()
	if werr != nil {
		_ = c.closeWithErr(werr)
		return req, werr
	}
	return req, c.waitTransport(ctx, req)
}

// execBinaryMulti sends a binary-protocol request that elicits multiple
// response packets terminated by a sentinel (GetMulti via OpGetKQ+OpNoop,
// Stats via OpStat). encode writes the full request bytes into w (which the
// caller has reset for them) and returns the serialized buffer to send.
// isTerm reports whether a response packet is the terminator. The driver
// holds the request on the bridge until isTerm returns true; intermediate
// packets accumulate into req.values / req.stats.
func (c *memcachedConn) execBinaryMulti(
	ctx context.Context,
	encode func(w *protocol.BinaryWriter) []byte,
	isTerm func(p protocol.BinaryPacket) bool,
) (*mcRequest, error) {
	if c.closed.Load() {
		return nil, ErrClosed
	}
	req := getRequest(ctx)
	req.kind = kindBinaryMulti
	// A unique starting opaque for the batch — individual sub-requests in the
	// batch (GetKQ per key, Noop terminator) choose their own opaques via
	// c.opaque.Add inside encode if needed, but dispatch does not rely on
	// opaque matching (it uses the terminator predicate instead).
	req.opaque = c.opaque.Add(1)
	req.binIsTerm = isTerm

	if c.useDirect {
		return c.execBinaryMultiDirect(ctx, req, encode)
	}

	if c.sync != nil {
		c.writerMu.Lock()
		c.state.binW.Reset()
		buf := encode(c.state.binW)
		c.state.bridge.Enqueue(req)
		if c.syncBuf == nil {
			c.syncBuf = make([]byte, 16<<10)
		}
		// Prefer WriteAndPollMulti on platforms that support it: we do not
		// know up-front how many packets the server will emit, so the poll
		// must continue until the terminator is seen. The isDone predicate
		// piggybacks on req.finished, which dispatchBinaryMulti flips once
		// the terminator packet is parsed.
		if c.syncMulti != nil {
			isDone := func() bool { return req.finished.Load() }
			ok, err := c.syncMulti.WriteAndPollMulti(c.fd, buf, c.syncBuf, c.onRecv, isDone, nil)
			c.writerMu.Unlock()
			if err != nil {
				_ = c.closeWithErr(err)
				return req, err
			}
			if ok {
				select {
				case <-req.doneCh:
					return req, nil
				default:
				}
			}
			return req, c.waitTransport(ctx, req)
		}
		var ok bool
		var err error
		if c.useBusy {
			ok, err = c.syncBusy.WriteAndPollBusy(c.fd, buf, c.syncBuf, c.onRecv)
		} else {
			ok, err = c.sync.WriteAndPoll(c.fd, buf, c.syncBuf, c.onRecv)
		}
		c.writerMu.Unlock()
		if err != nil {
			_ = c.closeWithErr(err)
			return req, err
		}
		if ok {
			select {
			case <-req.doneCh:
				return req, nil
			default:
			}
		}
		return req, c.waitTransport(ctx, req)
	}

	c.writerMu.Lock()
	c.state.binW.Reset()
	buf := encode(c.state.binW)
	werr := c.writeAndEnqueue(req, func() []byte { return buf })
	c.writerMu.Unlock()
	if werr != nil {
		_ = c.closeWithErr(werr)
		return req, werr
	}
	return req, c.waitTransport(ctx, req)
}

// waitTransport blocks until the request completes or ctx is canceled,
// returning only transport-level errors. Application-level errors are
// encoded in req.resultErr / req.binStatus and must be inspected by the
// caller. Mirrors [wait] but skips the req.resultErr propagation — the
// binary-protocol dispatch paths need to distinguish "server said no"
// from "connection died".
func (c *memcachedConn) waitTransport(ctx context.Context, req *mcRequest) error {
	for i := 0; i < spinIterations/2; i++ {
		select {
		case <-req.doneCh:
			return nil
		default:
		}
	}
	for i := 0; i < spinIterations/2; i++ {
		runtime.Gosched()
		select {
		case <-req.doneCh:
			return nil
		default:
		}
	}
	select {
	case <-req.doneCh:
		return nil
	case <-ctx.Done():
		_ = c.closeWithErr(ctx.Err())
		return ctx.Err()
	}
}

// closeWithErr records err as the close cause and closes the conn. Safe to
// call multiple times.
func (c *memcachedConn) closeWithErr(err error) error {
	c.closeErr.Store(&errBox{err: err})
	return c.Close()
}

// wait blocks until doneCh fires or ctx is canceled. Before parking on the
// channel, it performs a brief spin: the first half are tight non-blocking
// checks (sub-nanosecond each), the second half yield via Gosched to let the
// epoll worker goroutine deliver the response. Memcached replies to simple
// commands typically arrive within microseconds on loopback, so the spin
// window catches most responses without the ~2-5 us futex park/wake overhead.
func (c *memcachedConn) wait(ctx context.Context, req *mcRequest) error {
	for i := 0; i < spinIterations/2; i++ {
		select {
		case <-req.doneCh:
			return req.resultErr
		default:
		}
	}
	for i := 0; i < spinIterations/2; i++ {
		runtime.Gosched()
		select {
		case <-req.doneCh:
			return req.resultErr
		default:
		}
	}
	select {
	case <-req.doneCh:
		return req.resultErr
	case <-ctx.Done():
		// The pending request is still on the bridge. The next server
		// response will dispatch to this orphaned request instead of the
		// caller's next command, silently returning wrong data. The conn
		// is poisoned — close it so the pool discards it.
		_ = c.closeWithErr(ctx.Err())
		return ctx.Err()
	}
}

// touch records a use.
func (c *memcachedConn) touch() {
	c.lastUsedAt.Store(time.Now().UnixNano())
}

// ---------- async.Conn ----------

// Ping satisfies async.Conn. It sends a VERSION request (both protocols
// support it cheaply) and discards the reply.
func (c *memcachedConn) Ping(ctx context.Context) error {
	if c.closed.Load() {
		return ErrClosed
	}
	if c.binary {
		req, err := c.execBinary(ctx, protocol.OpVersion, func(w *protocol.BinaryWriter, opaque uint32) []byte {
			return w.AppendSimple(protocol.OpVersion, opaque)
		})
		if err != nil {
			return err
		}
		defer putRequest(req)
		return req.resultErr
	}
	req, err := c.execText(ctx, kindVersion, func(w *protocol.TextWriter) []byte {
		return w.AppendSimple("version")
	})
	if err != nil {
		return err
	}
	defer putRequest(req)
	return req.resultErr
}

// Close satisfies async.Conn.
func (c *memcachedConn) Close() error {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		if c.useDirect {
			if c.tcp != nil {
				_ = c.tcp.Close()
				c.tcp = nil
			}
		} else {
			if c.loop != nil {
				_ = c.loop.UnregisterConn(c.fd)
			}
			// Close via the pinned *os.File so the underlying fd is closed
			// exactly once. Falling back to syscall.Close would race with the
			// File's finalizer (double-close on an fd that may have been reused
			// by the runtime).
			if c.file != nil {
				_ = c.file.Close()
				c.file = nil
			} else if c.fd > 0 {
				_ = syscall.Close(c.fd)
			}
		}
		drainErr := ErrClosed
		if box := c.closeErr.Load(); box != nil && box.err != nil {
			drainErr = box.err
		}
		c.state.drainWithError(drainErr)
	})
	return nil
}

// execTextDirect is the direct-mode path for text-protocol commands. Writes
// the encoded request on the caller's goroutine via net.TCPConn.Write, then
// drives net.TCPConn.Read until onRecv finishes the request. Go's netpoll
// parks the goroutine on EPOLLIN transparently — no mini-loop overhead.
func (c *memcachedConn) execTextDirect(ctx context.Context, req *mcRequest, encode func(w *protocol.TextWriter) []byte) (*mcRequest, error) {
	c.writerMu.Lock()
	defer c.writerMu.Unlock()

	c.state.textW.Reset()
	buf := encode(c.state.textW)
	c.state.bridge.Enqueue(req)
	if c.syncBuf == nil {
		c.syncBuf = make([]byte, 16<<10)
	}

	if dl, ok := ctx.Deadline(); ok {
		_ = c.tcp.SetDeadline(dl)
		defer func() {
			if c.tcp != nil {
				_ = c.tcp.SetDeadline(time.Time{})
			}
		}()
	}

	if _, err := c.tcp.Write(buf); err != nil {
		_ = c.closeWithErr(err)
		return req, err
	}

	for !req.finished.Load() {
		if c.tcpFd > 0 {
			if n, _, perr := syscall.Recvfrom(c.tcpFd, c.syncBuf, syscall.MSG_DONTWAIT); n > 0 {
				c.onRecv(c.syncBuf[:n])
				continue
			} else if perr != nil && perr != syscall.EAGAIN && perr != syscall.EWOULDBLOCK {
				_ = c.closeWithErr(perr)
				return req, perr
			}
		}
		n, err := c.tcp.Read(c.syncBuf)
		if n > 0 {
			c.onRecv(c.syncBuf[:n])
		}
		if err != nil {
			_ = c.closeWithErr(err)
			return req, err
		}
	}
	return req, req.resultErr
}

// execBinaryDirect is the direct-mode path for binary-protocol commands.
func (c *memcachedConn) execBinaryDirect(ctx context.Context, req *mcRequest, encode func(w *protocol.BinaryWriter, opaque uint32) []byte) (*mcRequest, error) {
	c.writerMu.Lock()
	defer c.writerMu.Unlock()

	c.state.binW.Reset()
	buf := encode(c.state.binW, req.opaque)
	c.state.bridge.Enqueue(req)
	if c.syncBuf == nil {
		c.syncBuf = make([]byte, 16<<10)
	}

	if dl, ok := ctx.Deadline(); ok {
		_ = c.tcp.SetDeadline(dl)
		defer func() {
			if c.tcp != nil {
				_ = c.tcp.SetDeadline(time.Time{})
			}
		}()
	}

	if _, err := c.tcp.Write(buf); err != nil {
		_ = c.closeWithErr(err)
		return req, err
	}

	for !req.finished.Load() {
		if c.tcpFd > 0 {
			if n, _, perr := syscall.Recvfrom(c.tcpFd, c.syncBuf, syscall.MSG_DONTWAIT); n > 0 {
				c.onRecv(c.syncBuf[:n])
				continue
			} else if perr != nil && perr != syscall.EAGAIN && perr != syscall.EWOULDBLOCK {
				_ = c.closeWithErr(perr)
				return req, perr
			}
		}
		n, err := c.tcp.Read(c.syncBuf)
		if n > 0 {
			c.onRecv(c.syncBuf[:n])
		}
		if err != nil {
			_ = c.closeWithErr(err)
			return req, err
		}
	}
	return req, nil
}

// execBinaryMultiDirect is the direct-mode path for multi-reply binary
// commands (GetMulti, Stats). Loops reading until req.finished fires.
func (c *memcachedConn) execBinaryMultiDirect(ctx context.Context, req *mcRequest, encode func(w *protocol.BinaryWriter) []byte) (*mcRequest, error) {
	c.writerMu.Lock()
	defer c.writerMu.Unlock()

	c.state.binW.Reset()
	buf := encode(c.state.binW)
	c.state.bridge.Enqueue(req)
	if c.syncBuf == nil {
		c.syncBuf = make([]byte, 16<<10)
	}

	if dl, ok := ctx.Deadline(); ok {
		_ = c.tcp.SetDeadline(dl)
		defer func() {
			if c.tcp != nil {
				_ = c.tcp.SetDeadline(time.Time{})
			}
		}()
	}

	if _, err := c.tcp.Write(buf); err != nil {
		_ = c.closeWithErr(err)
		return req, err
	}

	for !req.finished.Load() {
		if c.tcpFd > 0 {
			if n, _, perr := syscall.Recvfrom(c.tcpFd, c.syncBuf, syscall.MSG_DONTWAIT); n > 0 {
				c.onRecv(c.syncBuf[:n])
				continue
			} else if perr != nil && perr != syscall.EAGAIN && perr != syscall.EWOULDBLOCK {
				_ = c.closeWithErr(perr)
				return req, perr
			}
		}
		n, err := c.tcp.Read(c.syncBuf)
		if n > 0 {
			c.onRecv(c.syncBuf[:n])
		}
		if err != nil {
			_ = c.closeWithErr(err)
			return req, err
		}
	}
	return req, nil
}

// Worker satisfies async.Conn.
func (c *memcachedConn) Worker() int { return c.workerID }

// IsExpired satisfies async.Conn.
func (c *memcachedConn) IsExpired(now time.Time) bool {
	if c.maxLifetime <= 0 {
		return false
	}
	return now.Sub(c.createdAt) >= c.maxLifetime
}

// IsIdleTooLong satisfies async.Conn.
func (c *memcachedConn) IsIdleTooLong(now time.Time) bool {
	if c.maxIdleTime <= 0 {
		return false
	}
	last := time.Unix(0, c.lastUsedAt.Load())
	return now.Sub(last) >= c.maxIdleTime
}

// Compile-time check.
var _ async.Conn = (*memcachedConn)(nil)
