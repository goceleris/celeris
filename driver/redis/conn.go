package redis

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
	"github.com/goceleris/celeris/engine"
)

// spinIterations is the number of spin loops the caller performs before
// parking on doneCh. The mix of tight spins and Gosched yields is tuned so
// the first few checks happen immediately (catching ultra-fast in-memory
// Redis responses without any scheduler overhead) before falling back to
// cooperative yields that let the epoll worker goroutine run.
const spinIterations = 64

// syncRoundTripper is the optional interface that the Linux event-loop worker
// implements. The Redis conn caches it at dial time to avoid repeated type
// assertions on the hot path.
type syncRoundTripper interface {
	WriteAndPoll(fd int, data []byte, rbuf []byte, onRecv func([]byte)) (ok bool, err error)
}

// syncBusyRoundTripper is the no-yield variant of syncRoundTripper. Drivers
// opened WithEngine(srv) — where the caller is the celeris HTTP engine's
// LockOSThread'd worker goroutine — use this path. Its Phase B skips
// runtime.Gosched() to avoid the stoplockedm+startlockedm futex storm
// that locked Ms incur on every Gosched.
type syncBusyRoundTripper interface {
	WriteAndPollBusy(fd int, data []byte, rbuf []byte, onRecv func([]byte)) (ok bool, err error)
}

// syncMultiRoundTripper extends syncRoundTripper for pipeline workloads.
// WriteAndPollMulti writes data, then repeatedly polls until isDone returns
// true. beforeRearm runs under recvMu before EPOLLIN is re-armed, allowing
// the driver to transition dispatch state atomically. Used by execManyInto
// to eliminate event-loop hops for bulk responses.
type syncMultiRoundTripper interface {
	WriteAndPollMulti(fd int, data []byte, rbuf []byte, onRecv func([]byte), isDone func() bool, beforeRearm func()) (ok bool, err error)
}

// redisConn is a single connection driven by a celeris event loop. It owns a
// redisState decoder and exposes helper methods for encoding commands and
// awaiting replies.
//
// Two I/O modes coexist. useDirect discriminates; fields for the unused
// mode are zero.
//
//   - Engine-integrated (cfg.Engine != nil, !AsyncHandlers): fd + loop +
//     sync round-trips via WriteAndPoll / WriteAndPollBusy. Driver FDs
//     share the celeris HTTP engine's LockOSThread'd worker. Direct
//     net.Conn.Read on that locked M would futex-storm.
//   - Direct (cfg.Engine == nil OR AsyncHandlers=true, cmd pool only):
//     tcp holds the live *net.TCPConn; exec() writes and reads directly
//     on the caller's goroutine via Go's netpoll. Pub/sub conns still
//     use the mini-loop path — unsolicited server-push messages need
//     event-loop delivery.
type redisConn struct {
	useDirect bool

	// Direct-mode fields.
	tcp *net.TCPConn
	// tcpFd caches the raw fd for MSG_DONTWAIT peeks before falling
	// through to tcp.Read (which parks the G via Go's netpoll). A
	// single non-blocking peek catches loopback-fast responses without
	// paying the netpoll wakeup round-trip. Zero when unavailable.
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
	// When useBusy is true, we prefer syncBusy and fall back to sync.
	sync syncRoundTripper
	// syncBusy is non-nil when the worker supports the no-yield busy-poll
	// variant. Drivers opened WithEngine dispatch through this to skip
	// the locked-M futex storm. Cached at dial time.
	syncBusy syncBusyRoundTripper
	// useBusy routes exec through syncBusy when set — true when the driver
	// was opened WithEngine (caller is a LockOSThread'd engine worker).
	useBusy bool
	// syncMulti is non-nil when the worker supports multi-response poll.
	// Used by execManyInto for pipeline workloads. Cached at dial time.
	syncMulti syncMultiRoundTripper
	// syncBuf is the per-conn read buffer used by the sync round-trip path.
	// Allocated lazily on first use.
	syncBuf []byte

	state *redisState
	cfg   Config

	// perConnWriter protects Writer + Write path serialization.
	writerMu sync.Mutex

	createdAt  time.Time
	lastUsedAt atomic.Int64

	maxLifetime time.Duration
	maxIdleTime time.Duration

	closeOnce sync.Once
	closed    atomic.Bool
	closeErr  atomic.Pointer[errBox]

	// dirty marks the conn as potentially inside a MULTI. Set by commands.go
	// when the caller issues MULTI; cleared on EXEC/DISCARD; consulted by
	// [resetSession] to send DISCARD only when needed.
	dirty atomic.Bool

	// pinned tracks how many callers acquired this conn via pinnedConnKey.
	// releaseCmd is a no-op while pinned > 0; the original acquirer handles
	// the real release.
	pinned atomic.Int32

	// onPubSubCloseMu guards onPubSubClose. Registered by [PubSub] so a
	// transport-level close can wake the PubSub's reconnect goroutine.
	onPubSubCloseMu sync.Mutex
	onPubSubClose   func(error)
}

// errBox wraps an error for atomic.Pointer — atomic.Value panics on type
// mismatch (io.EOF, *net.OpError, *errors.errorString all hit the driver's
// close path), so we normalize through a pointer-friendly wrapper.
type errBox struct{ err error }

// setPubSubCloseHook registers fn to be invoked once when the conn closes
// while in pubsub mode. Pass nil to clear.
func (c *redisConn) setPubSubCloseHook(fn func(error)) {
	c.onPubSubCloseMu.Lock()
	c.onPubSubClose = fn
	c.onPubSubCloseMu.Unlock()
}

// notifyPubSubClose invokes the registered pubsub close hook, if any.
func (c *redisConn) notifyPubSubClose(err error) {
	c.onPubSubCloseMu.Lock()
	fn := c.onPubSubClose
	c.onPubSubClose = nil
	c.onPubSubCloseMu.Unlock()
	if fn != nil {
		go fn(err)
	}
}

// dialDirectRedisConn dials a TCP connection in direct mode: no mini-loop
// registration, no fd extraction. The conn keeps the live *net.TCPConn
// and does Write/Read on the caller's goroutine via Go's netpoll.
func dialDirectRedisConn(ctx context.Context, cfg Config) (*redisConn, error) {
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
		return nil, fmt.Errorf("celeris-redis: expected *net.TCPConn, got %T", raw)
	}
	_ = tcp.SetNoDelay(true)
	var rawFd int
	if sc, scErr := tcp.SyscallConn(); scErr == nil {
		_ = sc.Control(func(fd uintptr) { rawFd = int(fd) })
	}
	c := &redisConn{
		useDirect: true,
		tcp:       tcp,
		tcpFd:     rawFd,
		state:     newRedisState(),
		cfg:       cfg,
		createdAt: time.Now(),
	}
	c.lastUsedAt.Store(time.Now().UnixNano())
	if err := c.handshake(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// dialRedisConn dials a TCP connection, sets it non-blocking, registers it
// with the event loop, and runs the HELLO handshake.
func dialRedisConn(ctx context.Context, prov engine.EventLoopProvider, cfg Config, workerHint int) (*redisConn, error) {
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
		return nil, fmt.Errorf("celeris-redis: expected *net.TCPConn, got %T", raw)
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
		// fd the kernel has since reassigned to another socket.
		_ = file.Close()
		return nil, errors.New("celeris-redis: event loop has 0 workers")
	}
	if workerHint < 0 || workerHint >= nw {
		workerHint = fd % nw
		if workerHint < 0 {
			workerHint = -workerHint
		}
	}
	loop := prov.WorkerLoop(workerHint)

	c := &redisConn{
		fd:        fd,
		loop:      loop,
		workerID:  workerHint,
		file:      file,
		state:     newRedisState(),
		cfg:       cfg,
		createdAt: time.Now(),
	}
	// Cache SyncRoundTripper capability for the fast path.
	if srt, ok := loop.(syncRoundTripper); ok {
		c.sync = srt
	}
	if sbrt, ok := loop.(syncBusyRoundTripper); ok {
		c.syncBusy = sbrt
	}
	// When the driver was opened WithEngine, the handler that calls into
	// us runs on the celeris HTTP engine's LockOSThread'd worker. Route
	// through the busy variant to avoid Gosched's locked-M futex storm.
	c.useBusy = cfg.Engine != nil && c.syncBusy != nil
	if smrt, ok := loop.(syncMultiRoundTripper); ok {
		c.syncMulti = smrt
	}
	c.lastUsedAt.Store(time.Now().UnixNano())

	if err := loop.RegisterConn(fd, c.onRecv, c.onClose); err != nil {
		// Close via file.Close() to disarm the *os.File finalizer; a plain
		// syscall.Close(fd) would leak the finalizer and risk a double
		// close on a reassigned fd when GC later runs it.
		_ = file.Close()
		return nil, err
	}

	if err := c.handshake(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// onRecv runs on the worker goroutine.
func (c *redisConn) onRecv(data []byte) {
	if err := c.state.processRedis(data); err != nil {
		c.closeErr.Store(&errBox{err: err})
		c.closed.Store(true)
		c.state.drainWithError(err)
		c.notifyPubSubClose(err)
	}
}

// onClose runs on the worker goroutine when the FD closes.
func (c *redisConn) onClose(err error) {
	if err == nil {
		err = io.EOF
	}
	c.closeErr.Store(&errBox{err: err})
	c.closed.Store(true)
	c.state.drainWithError(err)
	c.notifyPubSubClose(err)
}

// writeCommand encodes and sends one command. The writer's internal buffer
// is passed directly to loop.Write — all current engine implementations
// either copy the bytes (iouring / epoll / linux eventloop drivers append
// into their own send buffer) or write synchronously (std eventloop), so the
// slice is safe to reuse once Write returns. writerMu serializes access so
// two concurrent writes on the same conn can't clobber each other's buffer.
//
//nolint:unparam // returned buffer retained for async callers that reuse it
func (c *redisConn) writeCommand(args ...string) ([]byte, error) {
	c.writerMu.Lock()
	defer c.writerMu.Unlock()
	buf := c.state.writer.WriteCommand(args...)
	if err := c.loop.Write(c.fd, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// sendRaw writes a pre-serialized byte slice.
func (c *redisConn) sendRaw(data []byte) error {
	if c.useDirect {
		c.writerMu.Lock()
		_, err := c.tcp.Write(data)
		c.writerMu.Unlock()
		return err
	}
	return c.loop.Write(c.fd, data)
}

// execDirect is the direct-mode exec path. Writes the encoded command on
// the caller's goroutine via net.TCPConn.Write, then drives net.TCPConn.Read
// until onRecv finishes the request. Go's netpoll parks the goroutine on
// EPOLLIN transparently.
func (c *redisConn) execDirect(ctx context.Context, req *redisRequest, args ...string) (*redisRequest, error) {
	c.writerMu.Lock()
	defer c.writerMu.Unlock()
	buf := c.state.writer.WriteCommand(args...)
	if c.syncBuf == nil {
		c.syncBuf = make([]byte, 16<<10)
	}
	if dl, ok := ctx.Deadline(); ok {
		tcp := c.tcp
		_ = tcp.SetDeadline(dl)
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
		// One non-blocking peek before paying netpoll wakeup. For tiny
		// Redis replies (10-byte GET, +PONG\r\n, etc.) the response is
		// usually already in the kernel recv buffer by the time the
		// server has echoed it back on loopback. Peek catches that
		// common case in a single syscall — tcp.Read's G-park adds
		// ~1-2µs per read and costs measurable RPS at 90k+.
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

// execManyDirect is the direct-mode pipeline path. Writes `data` once,
// enqueues all reqs into the bridge, reads until the last req fires.
func (c *redisConn) execManyDirect(ctx context.Context, data []byte, reqs []*redisRequest) ([]*redisRequest, error) {
	n := len(reqs)
	last := reqs[n-1]
	c.writerMu.Lock()
	defer c.writerMu.Unlock()
	for i := 0; i < n; i++ {
		c.state.bridge.Enqueue(reqs[i])
	}
	if c.syncBuf == nil {
		c.syncBuf = make([]byte, 64<<10)
	}
	if dl, ok := ctx.Deadline(); ok {
		tcp := c.tcp
		_ = tcp.SetDeadline(dl)
		defer func() {
			if c.tcp != nil {
				_ = c.tcp.SetDeadline(time.Time{})
			}
		}()
	}
	if _, err := c.tcp.Write(data); err != nil {
		_ = c.closeWithErr(err)
		return reqs, err
	}
	for !last.finished.Load() {
		if c.tcpFd > 0 {
			if rn, _, perr := syscall.Recvfrom(c.tcpFd, c.syncBuf, syscall.MSG_DONTWAIT); rn > 0 {
				c.onRecv(c.syncBuf[:rn])
				continue
			} else if perr != nil && perr != syscall.EAGAIN && perr != syscall.EWOULDBLOCK {
				_ = c.closeWithErr(perr)
				return reqs, perr
			}
		}
		rn, err := c.tcp.Read(c.syncBuf)
		if rn > 0 {
			c.onRecv(c.syncBuf[:rn])
		}
		if err != nil {
			_ = c.closeWithErr(err)
			return reqs, err
		}
	}
	return reqs, nil
}

// exec sends a command and waits for the reply. The caller receives an
// alias'd Value — it must copy out before returning control.
func (c *redisConn) exec(ctx context.Context, args ...string) (*redisRequest, error) {
	if c.closed.Load() {
		return nil, ErrClosed
	}
	req := getRequest(ctx)
	c.state.bridge.Enqueue(req)

	// Direct-mode path: net.TCPConn.Write + Read on the caller's goroutine.
	// Go's netpoll handles EPOLLIN parking. Used when no engine was
	// supplied or when the engine dispatches handlers asynchronously.
	if c.useDirect {
		return c.execDirect(ctx, req, args...)
	}

	// Sync fast path: write the command and poll for the response on the
	// calling goroutine. Eliminates the event-loop context switch and the
	// doneCh futex park/wake for single-command round trips.
	if c.sync != nil {
		c.writerMu.Lock()
		buf := c.state.writer.WriteCommand(args...)
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
			return nil, err
		}
		if ok {
			select {
			case <-req.doneCh:
				return req, req.resultErr
			default:
			}
			// Response processed but doneCh not yet signaled — rare, fall
			// through to normal wait.
		}
		// EAGAIN or partial: fall through to blocking wait.
		return req, c.wait(ctx, req)
	}

	// Async path.
	if _, err := c.writeCommand(args...); err != nil {
		_ = c.closeWithErr(err)
		return nil, err
	}
	return req, c.wait(ctx, req)
}

// execMany writes a pre-serialized multi-command buffer and enqueues n
// sentinel requests. Returns the slice of requests; each fires when its
// reply arrives.
func (c *redisConn) execMany(ctx context.Context, data []byte, n int) ([]*redisRequest, error) {
	return c.execManyInto(ctx, data, make([]*redisRequest, n))
}

// execManyInto is the pooled-slice variant of execMany: the caller supplies
// a []*redisRequest slice (typically reused across calls) sized to n, and
// execManyInto fills it with freshly-acquired requests. Callers MUST pass
// a slice with len == n; any existing contents are overwritten.
func (c *redisConn) execManyInto(ctx context.Context, data []byte, reqs []*redisRequest) ([]*redisRequest, error) {
	if c.closed.Load() {
		return reqs, ErrClosed
	}
	n := len(reqs)
	for i := 0; i < n; i++ {
		reqs[i] = getRequest(ctx)
	}
	return c.execManyCore(ctx, data, reqs)
}

// execManyIntoPrealloc is the pre-allocated variant of execManyInto: the
// caller has already populated reqs via getRequest and may have set
// per-request fields (e.g. expect kind). This avoids double-allocation
// when the caller needs to tag requests before dispatch.
func (c *redisConn) execManyIntoPrealloc(ctx context.Context, data []byte, reqs []*redisRequest) ([]*redisRequest, error) {
	if c.closed.Load() {
		return reqs, ErrClosed
	}
	return c.execManyCore(ctx, data, reqs)
}

// execManyCore is the shared implementation for execManyInto and
// execManyIntoPrealloc. reqs must be pre-populated with valid requests.
func (c *redisConn) execManyCore(ctx context.Context, data []byte, reqs []*redisRequest) ([]*redisRequest, error) {
	n := len(reqs)
	last := reqs[n-1].doneCh

	// Direct-mode pipeline: one write, loop reads until last req fires.
	if c.useDirect {
		return c.execManyDirect(ctx, data, reqs)
	}

	// Sync multi fast path: write all commands and poll for all N responses
	// on the calling goroutine. Uses direct-index dispatch (syncPipeReqs) to
	// skip the bridge queue, eliminating 2N mutex operations. The beforeRearm
	// callback transitions to bridge dispatch under recvMu, so the event loop
	// sees a consistent state when EPOLLIN is re-armed.
	if c.syncMulti != nil {
		c.writerMu.Lock()
		if c.syncBuf == nil {
			c.syncBuf = make([]byte, 64<<10)
		}
		// Install sync pipeline dispatch. Reset the copy slab so this
		// batch's string copies are contiguous in one allocation.
		c.state.copySlab = c.state.copySlab[:0]
		c.state.syncPipeReqs = reqs
		c.state.syncPipeIdx = 0
		lastReq := reqs[n-1]
		isDone := func() bool {
			return lastReq.finished.Load()
		}
		// beforeRearm runs under recvMu before EPOLLIN is re-armed. It
		// transitions from direct-index dispatch to bridge dispatch so the
		// event loop can deliver any remaining responses.
		beforeRearm := func() {
			dispatched := c.state.syncPipeIdx
			c.state.syncPipeReqs = nil
			c.state.syncPipeIdx = 0
			// Enqueue remaining (un-dispatched) requests into the bridge.
			for i := dispatched; i < n; i++ {
				c.state.bridge.Enqueue(reqs[i])
			}
		}
		ok, err := c.syncMulti.WriteAndPollMulti(c.fd, data, c.syncBuf, c.onRecv, isDone, beforeRearm)
		c.writerMu.Unlock()
		if err != nil {
			_ = c.closeWithErr(err)
			return reqs, err
		}
		if ok {
			// All responses parsed synchronously. Sync pipeline dispatch
			// uses atomic Store (no channel send) for all requests, so
			// there is nothing to drain.
			return reqs, nil
		}
		// Partial or EAGAIN: fall through to spin-wait for the rest.
		// Bridge was populated in beforeRearm, event loop will deliver.
	} else {
		// Async path: enqueue all into bridge, write, spin-wait.
		for i := 0; i < n; i++ {
			c.state.bridge.Enqueue(reqs[i])
		}
		if err := c.sendRaw(data); err != nil {
			_ = c.closeWithErr(err)
			return reqs, err
		}
	}

	// Spin-wait before parking — pipeline responses often arrive within the
	// spin window because the entire batch was written atomically and Redis
	// processes commands sequentially.
	for i := 0; i < spinIterations/2; i++ {
		select {
		case <-last:
			return reqs, nil
		default:
		}
	}
	for i := 0; i < spinIterations/2; i++ {
		runtime.Gosched()
		select {
		case <-last:
			return reqs, nil
		default:
		}
	}
	// Park.
	select {
	case <-last:
	case <-ctx.Done():
		cerr := ctx.Err()
		for _, r := range reqs {
			if r.finished.Load() {
				continue
			}
			r.resultErr = cerr
			r.finish()
		}
		_ = c.closeWithErr(cerr)
		return reqs, cerr
	}
	return reqs, nil
}

// closeWithErr records err as the close cause and closes the conn. Safe to
// call multiple times.
func (c *redisConn) closeWithErr(err error) error {
	c.closeErr.Store(&errBox{err: err})
	return c.Close()
}

// wait blocks until doneCh fires or ctx is canceled. Before parking on the
// channel, it performs a brief spin: the first half are tight non-blocking
// checks (sub-nanosecond each), the second half yield via Gosched to let the
// epoll worker goroutine deliver the response. Redis replies to simple
// commands typically arrive within microseconds on loopback, so the spin
// window catches most responses without the ~2-5 us futex park/wake overhead.
func (c *redisConn) wait(ctx context.Context, req *redisRequest) error {
	// Phase 1: tight spin (no yield). Catches responses that arrived while
	// we were encoding/writing.
	for i := 0; i < spinIterations/2; i++ {
		select {
		case <-req.doneCh:
			return req.resultErr
		default:
		}
	}
	// Phase 2: yield-assisted spin. Lets the worker goroutine run.
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

// releaseResult releases the request's pooled result back to the Reader,
// then returns the redisRequest itself to reqPool. Callers MUST NOT retain
// req after this call.
func (c *redisConn) releaseResult(req *redisRequest) {
	if req == nil {
		return
	}
	if req.owned {
		c.state.reader.Release(req.result)
	}
	putRequest(req)
}

// handshake runs HELLO (with optional fallback to AUTH+SELECT) to negotiate
// protocol version and authentication.
func (c *redisConn) handshake(ctx context.Context) error {
	if c.cfg.ForceRESP2 {
		return c.legacyAuth(ctx)
	}
	targetProto := c.cfg.Proto
	if targetProto != 2 && targetProto != 3 {
		targetProto = 3
	}
	// HELLO <proto> [AUTH user pass]
	var helloArgs []string
	helloArgs = append(helloArgs, "HELLO")
	helloArgs = append(helloArgs, fmt.Sprintf("%d", targetProto))
	if c.cfg.Password != "" {
		user := c.cfg.Username
		if user == "" {
			user = "default"
		}
		helloArgs = append(helloArgs, "AUTH", user, c.cfg.Password)
	}
	req, err := c.exec(ctx, helloArgs...)
	if err == nil {
		defer c.releaseResult(req)
		c.state.proto.Store(int32(targetProto))
		// After HELLO OK, select db.
		if c.cfg.DB != 0 {
			if err := c.selectDB(ctx, c.cfg.DB); err != nil {
				return err
			}
		}
		return nil
	}
	// Release the request from the failed exec — it may hold pooled Reader
	// slices that would otherwise leak.
	c.releaseResult(req)
	// If HELLO was rejected, fall back to legacy auth + select.
	var rerr *RedisError
	if errors.As(err, &rerr) {
		c.state.proto.Store(2)
		return c.legacyAuth(ctx)
	}
	return err
}

// legacyAuth runs AUTH and SELECT explicitly.
func (c *redisConn) legacyAuth(ctx context.Context) error {
	c.state.proto.Store(2)
	if c.cfg.Password != "" {
		var args []string
		if c.cfg.Username != "" {
			args = []string{"AUTH", c.cfg.Username, c.cfg.Password}
		} else {
			args = []string{"AUTH", c.cfg.Password}
		}
		req, err := c.exec(ctx, args...)
		if err != nil {
			return err
		}
		c.releaseResult(req)
	}
	if c.cfg.DB != 0 {
		return c.selectDB(ctx, c.cfg.DB)
	}
	return nil
}

func (c *redisConn) selectDB(ctx context.Context, db int) error {
	req, err := c.exec(ctx, "SELECT", fmt.Sprintf("%d", db))
	if err != nil {
		return err
	}
	c.releaseResult(req)
	return nil
}

// touch records a use.
func (c *redisConn) touch() {
	c.lastUsedAt.Store(time.Now().UnixNano())
}

// resetSession clears per-conn dirty state before the conn is returned to
// the pool. When the [redisConn.dirty] flag is set — i.e. a MULTI was
// issued on this conn via [Client.Do] without a matching EXEC/DISCARD —
// a best-effort DISCARD is sent so the next acquirer sees a clean state.
// The DISCARD reply (server error outside of MULTI, or simple "+OK") is
// consumed and dropped. A short timeout bounds the wait.
func (c *redisConn) resetSession() {
	if c.closed.Load() {
		return
	}
	if c.state.currentMode() != modeCmd {
		return
	}
	if !c.dirty.Load() {
		return
	}
	c.dirty.Store(false)
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	req, err := c.exec(ctx, "DISCARD")
	if err != nil {
		return
	}
	c.releaseResult(req)
}

// ---------- async.Conn ----------

// Ping satisfies async.Conn.
func (c *redisConn) Ping(ctx context.Context) error {
	if c.closed.Load() {
		return ErrClosed
	}
	req, err := c.exec(ctx, "PING")
	if err != nil {
		return err
	}
	defer c.releaseResult(req)
	return nil
}

// Close satisfies async.Conn.
func (c *redisConn) Close() error {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		if c.useDirect {
			if c.tcp != nil {
				_ = c.tcp.Close()
				c.tcp = nil
			}
			drainErr := ErrClosed
			if box := c.closeErr.Load(); box != nil && box.err != nil {
				drainErr = box.err
			}
			c.state.drainWithError(drainErr)
			c.notifyPubSubClose(drainErr)
			return
		}
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
		// Surface the most-specific cause to pending requests if one has
		// been recorded (e.g. write error from exec/execMany); otherwise
		// fall back to the generic ErrClosed.
		drainErr := ErrClosed
		if box := c.closeErr.Load(); box != nil && box.err != nil {
			drainErr = box.err
		}
		c.state.drainWithError(drainErr)
		// Notify any registered PubSub so it can reconnect.
		c.notifyPubSubClose(drainErr)
	})
	return nil
}

// Worker satisfies async.Conn.
func (c *redisConn) Worker() int { return c.workerID }

// IsExpired satisfies async.Conn.
func (c *redisConn) IsExpired(now time.Time) bool {
	if c.maxLifetime <= 0 {
		return false
	}
	return now.Sub(c.createdAt) >= c.maxLifetime
}

// IsIdleTooLong satisfies async.Conn.
func (c *redisConn) IsIdleTooLong(now time.Time) bool {
	if c.maxIdleTime <= 0 {
		return false
	}
	last := time.Unix(0, c.lastUsedAt.Load())
	return now.Sub(last) >= c.maxIdleTime
}

// Compile-time check.
var _ async.Conn = (*redisConn)(nil)
