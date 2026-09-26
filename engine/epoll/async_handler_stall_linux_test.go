//go:build linux

package epoll

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/internal/ctxkit"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// celeris#669: with AsyncHandlers, runAsyncHandler holds cs.detachMu across
// the whole of ProcessH1, i.e. for as long as the user handler runs. Every
// loop-thread site that took that mutex with a blocking Lock therefore parked
// the loop thread — and so every other connection on the loop: no epoll_wait,
// no accept, no flush — until the handler returned. The epoll twin of the
// io_uring celeris#593, which PR #604 fixed with TryLock-and-skip.
//
// The sites, all on the loop thread:
//
//   - closeConn's first detachMu.Lock, reached by the timeout reap
//     (checkTimeouts), EPOLLRDHUP, EPOLLERR/EPOLLHUP and every error branch;
//   - drainRead's read-error and EOF branches, which lock before they flush,
//     notify and close — a client that disconnects mid-handler;
//   - the dirty-list pass (flushDirty);
//   - the EPOLLOUT resume (handleWritable).
//
// The unit arms below hold detachMu the way a running handler does and ask
// each site to return while it is held. The end-to-end arms are the issue's
// own measurement: a slow async handler on one connection and a fast
// keep-alive connection pinned to the SAME loop, whose latency must not carry
// the handler's duration.

// stallWait is how long a unit arm lets a site run before calling it parked.
// A site that does not wait returns in microseconds; one that waits returns
// only when the test releases the lock, which it never does before this.
const stallWait = 2 * time.Second

// holdAsHandler takes cs.detachMu as runAsyncHandler does around ProcessH1 and
// returns the release. running=true marks the dispatch goroutine as running
// (not parked), which is what a goroutine inside a handler is; running=false
// marks it parked, so the holder is someone whose hold is bounded by one
// write — a detached conn's guarded writeFn.
func holdAsHandler(t *testing.T, cs *connState, running bool) (release func()) {
	t.Helper()
	cs.asyncInMu.Lock()
	cs.asyncRun = true
	cs.asyncParked = !running
	cs.asyncInMu.Unlock()
	cs.detachMu.Lock()
	var once sync.Once
	release = func() { once.Do(cs.detachMu.Unlock) }
	t.Cleanup(release)
	return release
}

// returnsWhileHeld runs site on its own goroutine and reports whether it
// returned within stallWait while the caller still holds detachMu. On a
// timeout it releases the lock (so the site can finish and the goroutine does
// not leak) and waits for it.
func returnsWhileHeld(t *testing.T, release func(), site func()) bool {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		site()
	}()
	select {
	case <-done:
		return true
	case <-time.After(stallWait):
		release()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("site never returned even after detachMu was released")
		}
		return false
	}
}

// exitDispatch runs the REAL runAsyncHandler tail a dispatch goroutine takes
// after its handler returns on a conn whose close was requested: the loop top
// observes asyncClosed and exits. Run synchronously, so whatever it hands back
// is on the detach queue when it returns.
func exitDispatch(l *Loop, cs *connState) {
	l.asyncWG.Add(1)
	l.runAsyncHandler(cs)
}

// fdOpen reports whether fd is still an open descriptor.
func fdOpen(fd int) bool {
	_, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	return err == nil
}

// TestReapDoesNotWaitForARunningAsyncHandler is the issue's site 1 at unit
// level: checkTimeouts finds the deadline expired (nothing refreshes
// lastActivity while a handler runs) and calls closeConn, which must not park
// on the handler's detachMu. The close is still owed: once the handler returns
// and its goroutine exits, the conn is closed exactly once, with its hook.
func TestReapDoesNotWaitForARunningAsyncHandler(t *testing.T) {
	rig := hijackRaceConn(t)
	l, cs, local := rig.l, rig.cs, rig.local
	release := holdAsHandler(t, cs, true)

	if !returnsWhileHeld(t, release, l.checkTimeouts) {
		t.Fatalf("celeris#669: checkTimeouts -> closeConn waited %v on cs.detachMu held by a running "+
			"async handler; the loop thread, and every connection on it, is parked until the handler returns",
			stallWait)
	}

	// Nothing may be torn down while the handler still runs: it is writing
	// the response into this conn's buffers under the lock the reap skipped.
	if l.conns[local] != cs || l.connCount != 1 || l.closeCount.Load() != 0 || rig.disconnects.Load() != 0 {
		t.Fatalf("the reap tore the conn down under a running handler: slot=%p connCount=%d closeCount=%d hooks=%d",
			l.conns[local], l.connCount, l.closeCount.Load(), rig.disconnects.Load())
	}
	if !fdOpen(local) {
		t.Fatalf("fd %d closed while the handler still owns it", local)
	}

	// The handler returns; its goroutine exits and hands the close back.
	release()
	exitDispatch(l, cs)
	l.drainDetachQueue()

	if l.conns[local] != nil || l.connCount != 0 || l.closeCount.Load() != 1 || rig.disconnects.Load() != 1 {
		t.Errorf("the deferred close did not complete exactly once: slot=%p connCount=%d closeCount=%d hooks=%d",
			l.conns[local], l.connCount, l.closeCount.Load(), rig.disconnects.Load())
	}
	if fdOpen(local) {
		t.Errorf("fd %d still open after the deferred close", local)
	}
}

// TestPeerCloseDoesNotWaitForARunningAsyncHandler: drainRead's EOF branch (a
// client that gives up mid-handler) took detachMu before flushing, notifying
// and closing. It must not park either, and the notification it owes a
// detached middleware (OnError(errPeerClosed)) must still be delivered, under
// the lock, when the deferred close runs.
func TestPeerCloseDoesNotWaitForARunningAsyncHandler(t *testing.T) {
	rig := hijackRaceConn(t)
	l, cs, local, peer := rig.l, rig.cs, rig.local, rig.peer
	l.async = true
	cs.buf = make([]byte, 4096)
	var notified []error
	cs.h1State.OnError = func(err error) { notified = append(notified, err) }
	release := holdAsHandler(t, cs, true)

	if err := unix.Shutdown(peer, unix.SHUT_WR); err != nil {
		t.Fatalf("shutdown peer: %v", err)
	}
	if !returnsWhileHeld(t, release, func() { l.drainRead(local, time.Now().UnixNano()) }) {
		t.Fatalf("celeris#669: drainRead's EOF branch waited %v on cs.detachMu held by a running async "+
			"handler; a client that disconnects mid-handler parks the whole loop", stallWait)
	}
	if l.conns[local] != cs || l.closeCount.Load() != 0 {
		t.Fatalf("the EOF tore the conn down under a running handler: slot=%p closeCount=%d",
			l.conns[local], l.closeCount.Load())
	}

	release()
	exitDispatch(l, cs)
	l.drainDetachQueue()

	if l.conns[local] != nil || l.closeCount.Load() != 1 || rig.disconnects.Load() != 1 {
		t.Errorf("the deferred close did not complete exactly once: slot=%p closeCount=%d hooks=%d",
			l.conns[local], l.closeCount.Load(), rig.disconnects.Load())
	}
	if len(notified) != 1 || !errors.Is(notified[0], errPeerClosed) {
		t.Errorf("OnError calls = %v, want exactly one errPeerClosed", notified)
	}
}

// TestDirtyPassDoesNotWaitForARunningAsyncHandler: the issue's site 2. A conn
// left on the dirty list by a partial flush is locked by the pass every
// iteration; with its dispatch goroutine inside a handler the pass must move
// on, and stop polling the conn (a dirty list that never empties holds the
// loop at a 0 ms epoll_wait, a spin). The goroutine flushes writeBuf itself
// when its handler returns and hands back any remainder.
func TestDirtyPassDoesNotWaitForARunningAsyncHandler(t *testing.T) {
	rig := hijackRaceConn(t)
	l, cs := rig.l, rig.cs
	cs.writeBuf = append(cs.writeBuf[:0], "pending"...)
	cs.pendingBytes = len(cs.writeBuf)
	l.markDirty(cs)
	release := holdAsHandler(t, cs, true)

	if !returnsWhileHeld(t, release, l.flushDirty) {
		t.Fatalf("celeris#669: the dirty-list pass waited %v on cs.detachMu held by a running async handler",
			stallWait)
	}
	if cs.dirty || l.dirtyHead != nil {
		t.Errorf("the pass left the conn on the dirty list: the loop would spin at a 0 ms epoll_wait " +
			"for as long as the handler runs")
	}
}

// TestEPOLLOUTResumeDoesNotWaitForARunningAsyncHandler: a conn that hit write
// backpressure is on level-triggered EPOLLOUT; a pipelined request can start a
// handler before the socket drains. The resume must not park, and must drop
// the level-triggered interest, which would otherwise fire on every
// epoll_wait until the handler returns.
func TestEPOLLOUTResumeDoesNotWaitForARunningAsyncHandler(t *testing.T) {
	rig := hijackRaceConn(t)
	l, cs := rig.l, rig.cs
	cs.writeBuf = append(cs.writeBuf[:0], "pending"...)
	cs.pendingBytes = len(cs.writeBuf)
	l.armEpollOut(cs)
	if !cs.epollOut {
		t.Fatal("setup: EPOLLOUT not armed")
	}
	release := holdAsHandler(t, cs, true)

	if !returnsWhileHeld(t, release, func() { l.handleWritable(cs) }) {
		t.Fatalf("celeris#669: the EPOLLOUT resume waited %v on cs.detachMu held by a running async handler",
			stallWait)
	}
	if cs.epollOut {
		t.Errorf("EPOLLOUT still armed: level-triggered, it fires on every epoll_wait until the handler returns")
	}
}

// TestCloseStillWaitsForABoundedHolder is the negative control for the unit
// arms. The dispatch goroutine is PARKED, so whoever holds detachMu is a
// guarded writeFn in the middle of one write — a hold bounded by a syscall,
// which closeConn has always waited out so the write cannot race the
// teardown. That must not change: the close waits, then completes.
func TestCloseStillWaitsForABoundedHolder(t *testing.T) {
	rig := hijackRaceConn(t)
	l, cs, local := rig.l, rig.cs, rig.local
	release := holdAsHandler(t, cs, false)

	done := make(chan struct{})
	go func() {
		defer close(done)
		l.closeConn(local)
	}()
	select {
	case <-done:
		t.Fatal("closeConn returned while a bounded holder (a parked dispatch goroutine's writer) held " +
			"detachMu; the write could race the teardown")
	case <-time.After(200 * time.Millisecond):
	}
	release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("closeConn never completed after the holder released detachMu")
	}
	if l.conns[local] != nil || l.closeCount.Load() != 1 || rig.disconnects.Load() != 1 {
		t.Errorf("close incomplete: slot=%p closeCount=%d hooks=%d",
			l.conns[local], l.closeCount.Load(), rig.disconnects.Load())
	}
}

// ---- end to end: the issue's measurement --------------------------------

// stallSlow is the async handler's duration. The reap fires ~ReadTimeout
// after the request, the peer close 50 ms after it; either way a parked loop
// holds the fast conn for most of this.
const stallSlow = 800 * time.Millisecond

// stallBudget is the most one fast request may take. A healthy loop serves it
// in well under a millisecond; a parked one takes ~stallSlow minus the
// trigger's offset (650-750 ms). The gap absorbs a loaded -race runner.
const stallBudget = 300 * time.Millisecond

// stallHandler answers /fast inline with the serving loop's id and /slow on
// the dispatch goroutine after stallSlow.
type stallHandler struct{}

func (stallHandler) HandleStream(ctx context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	body := "slow"
	if s.Path == "/slow" {
		time.Sleep(stallSlow)
	} else {
		id, _ := ctxkit.WorkerIDFrom(ctx)
		body = "w=" + strconv.Itoa(id)
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", strconv.Itoa(len(body))}},
		[]byte(body))
}
func (stallHandler) RouteAsync(_, path string) bool { return path == "/slow" }
func (stallHandler) HasAsyncRoutes() bool           { return true }

var _ stream.AsyncRouteResolver = stallHandler{}

func startStallEngine(t *testing.T, cfg resource.Config) *Engine {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	cfg.Addr = ln.Addr().String()
	_ = ln.Close()
	cfg.Protocol = engine.HTTP1
	cfg.Resources = resource.Resources{Workers: 2}
	cfg.AsyncHandlers = true
	e, err := New(cfg, stallHandler{})
	if err != nil {
		t.Fatalf("epoll engine: %v", err) // not a skip: a skip here would take the witness out of CI silently
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- e.Listen(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
		}
	})
	for dl := time.Now().Add(10 * time.Second); e.Addr() == nil && time.Now().Before(dl); {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Fatal("engine did not bind")
	}
	return e
}

type stallConn struct {
	c  net.Conn
	br *bufio.Reader
}

func stallDial(t *testing.T, addr string) *stallConn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &stallConn{c: c, br: bufio.NewReader(c)}
}

// get sends one request and returns the body.
func (s *stallConn) get(path string, deadline time.Duration) (string, error) {
	_ = s.c.SetDeadline(time.Now().Add(deadline))
	if _, err := fmt.Fprintf(s.c, "GET %s HTTP/1.1\r\nHost: x\r\n\r\n", path); err != nil {
		return "", err
	}
	resp, err := http.ReadResponse(s.br, nil)
	if err != nil {
		return "", err
	}
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return string(b), err
}

// colocate dials until a connection lands on the loop that serves slow, which
// SO_REUSEPORT decides per connection. The loop id comes back in the body.
func colocate(t *testing.T, addr string, slow *stallConn) (*stallConn, string) {
	t.Helper()
	want, err := slow.get("/fast", 3*time.Second)
	if err != nil {
		t.Fatalf("probe slow conn: %v", err)
	}
	for range 64 {
		f := stallDial(t, addr)
		got, err := f.get("/fast", 3*time.Second)
		if err != nil {
			t.Fatalf("probe candidate: %v", err)
		}
		if got == want {
			return f, want
		}
		_ = f.c.Close()
	}
	t.Fatalf("no connection landed on loop %s in 64 dials", want)
	return nil, ""
}

// pingWhile pings the fast conn back to back until stop closes, and returns
// every latency, plus the first error if the fast conn broke.
func pingWhile(f *stallConn, stop <-chan struct{}) (lat []time.Duration, err error) {
	for {
		select {
		case <-stop:
			return lat, nil
		default:
		}
		t0 := time.Now()
		if _, err := f.get("/fast", 5*time.Second); err != nil {
			return lat, err
		}
		lat = append(lat, time.Since(t0))
		time.Sleep(2 * time.Millisecond)
	}
}

// runStall drives one trigger: the slow conn sends /slow, trigger fires
// whatever makes the loop act on that conn mid-handler, and the co-located
// fast conn is pinged throughout. The slow conn must still get its whole
// response and then the close.
func runStall(t *testing.T, name string, cfg resource.Config, trigger func(*stallConn)) {
	e := startStallEngine(t, cfg)
	addr := e.Addr().String()
	slow := stallDial(t, addr)
	fast, loop := colocate(t, addr, slow)

	stop := make(chan struct{})
	type result struct {
		lat []time.Duration
		err error
	}
	pinged := make(chan result, 1)
	go func() {
		lat, err := pingWhile(fast, stop)
		pinged <- result{lat, err}
	}()

	_ = slow.c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := slow.c.Write([]byte("GET /slow HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatalf("send /slow: %v", err)
	}
	trigger(slow)

	resp, rerr := http.ReadResponse(slow.br, nil)
	var body []byte
	if rerr == nil {
		body, rerr = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	// The close follows the response: the reap and the peer close both still
	// end this connection, once the handler has answered.
	_, eofErr := slow.br.ReadByte()
	time.Sleep(50 * time.Millisecond)
	close(stop)
	r := <-pinged

	var worst time.Duration
	for _, d := range r.lat {
		worst = max(worst, d)
	}
	sorted := slices.Clone(r.lat)
	slices.Sort(sorted)
	var p50 time.Duration
	if len(sorted) > 0 {
		p50 = sorted[len(sorted)/2]
	}
	t.Logf("celeris669 STALL trigger=%s loop=%s samples=%d max_ms=%.1f p50_ms=%.2f fast_err=%v slow_body=%q slow_eof=%v",
		name, strings.TrimPrefix(loop, "w="), len(r.lat), float64(worst)/1e6, float64(p50)/1e6, r.err, body, eofErr)

	if r.err != nil {
		t.Errorf("celeris#669: the fast conn on the same loop broke while the slow handler ran: %v", r.err)
	}
	if worst > stallBudget {
		t.Errorf("celeris#669: a fast request on loop %s took %v (budget %v) while a %v async handler "+
			"ran on another conn of the same loop: the loop thread was parked on that conn's detachMu",
			loop, worst, stallBudget, stallSlow)
	}
	if rerr != nil || string(body) != "slow" {
		t.Errorf("slow conn: response %q, err %v; want the handler's full answer", body, rerr)
	}
	if !errors.Is(eofErr, io.EOF) {
		t.Errorf("slow conn: after the response got %v, want EOF (the close was lost)", eofErr)
	}
}

// TestTimeoutReapOfASlowAsyncHandlerDoesNotStallItsLoop is the issue's rig:
// ReadTimeout below the handler's duration, so the reap fires mid-handler.
// ReadHeaderTimeout arms the 25 ms timerfd cadence that runs the reap on time.
func TestTimeoutReapOfASlowAsyncHandlerDoesNotStallItsLoop(t *testing.T) {
	runStall(t, "reap", resource.Config{
		ReadTimeout:       100 * time.Millisecond,
		ReadHeaderTimeout: 10 * time.Second,
	}, func(*stallConn) {})
}

// TestPeerCloseDuringASlowAsyncHandlerDoesNotStallItsLoop is the same stall
// through drainRead's EOF branch: the client half-closes 50 ms into the
// handler, as a client that stops waiting does. No timeouts are configured,
// so nothing but the FIN acts on the conn.
func TestPeerCloseDuringASlowAsyncHandlerDoesNotStallItsLoop(t *testing.T) {
	runStall(t, "peerclose", resource.Config{}, func(s *stallConn) {
		time.Sleep(50 * time.Millisecond)
		if err := s.c.(*net.TCPConn).CloseWrite(); err != nil {
			t.Fatalf("half-close: %v", err)
		}
	})
}
