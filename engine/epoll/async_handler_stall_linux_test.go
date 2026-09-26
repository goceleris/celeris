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
// loop at a 0 ms epoll_wait, a spin). The goroutine owes the conn back
// (relink); TestAConnGivenUpMidHandlerIsHandedBack follows it home.
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
	cs.asyncInMu.Lock()
	owed := cs.relinkOwed
	cs.asyncInMu.Unlock()
	if !owed || !cs.relinkPending {
		t.Errorf("the pass gave the conn up without a hand-back owed (relinkOwed=%v relinkPending=%v)",
			owed, cs.relinkPending)
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

// TestAConnGivenUpMidHandlerIsHandedBack follows a conn the dirty pass or the
// EPOLLOUT resume gave up while a handler held detachMu. What those passes do
// when a flush completes — close a conn whose peer half-closed (peerClosed),
// drop the EPOLLOUT interest — must still happen, including when the peer's
// half-close is only noticed after the conn was given up, and although the
// holder's own flush may complete and leave no remainder to hand back. The
// REAL dispatch loop hands the conn back at its park; the loop puts it on the
// dirty list, flushes it and closes it.
func TestAConnGivenUpMidHandlerIsHandedBack(t *testing.T) {
	for _, site := range []string{"dirty", "epollout"} {
		t.Run(site, func(t *testing.T) {
			rig := hijackRaceConn(t)
			l, cs, local := rig.l, rig.cs, rig.local
			l.async = true
			cs.writeBuf = append(cs.writeBuf[:0], "pending"...)
			cs.pendingBytes = len(cs.writeBuf)
			pass := l.flushDirty
			if site == "dirty" {
				l.markDirty(cs)
			} else {
				l.armEpollOut(cs)
				pass = func() { l.handleWritable(cs) }
			}
			release := holdAsHandler(t, cs, true)
			if !returnsWhileHeld(t, release, pass) {
				t.Fatalf("the %s pass waited on a running handler (celeris#669)", site)
			}
			if cs.dirty || cs.epollOut || !cs.relinkPending {
				t.Fatalf("the %s pass did not give the conn up (dirty=%v epollOut=%v relinkPending=%v)",
					site, cs.dirty, cs.epollOut, cs.relinkPending)
			}
			// The peer half-closes; the RDHUP branch finds a response still
			// queued and defers the close until it is flushed.
			cs.peerClosed = true
			release() // the handler returns

			// The dispatch goroutine reaches its park.
			l.asyncWG.Add(1)
			go l.runAsyncHandler(cs)
			for dl := time.Now().Add(5 * time.Second); l.detachQPending.Load() == 0; {
				if time.Now().After(dl) {
					t.Fatal("the dispatch goroutine parked without handing the conn back")
				}
				time.Sleep(time.Millisecond)
			}
			l.drainDetachQueue()
			if !cs.dirty || cs.relinkPending {
				t.Fatalf("the hand-back did not put the conn back on the dirty list (dirty=%v relinkPending=%v)",
					cs.dirty, cs.relinkPending)
			}
			l.flushDirty()
			l.asyncWG.Wait() // the close woke the parked goroutine, which exits

			hijackRaceExpectRead(t, rig.peer, "pending", "the queued response reached the peer")
			if l.conns[local] != nil || rig.disconnects.Load() != 1 {
				t.Errorf("the deferred peer close was lost: slot=%p hooks=%d", l.conns[local], rig.disconnects.Load())
			}
		})
	}
}

// TestATransplantWaitsForARelink: between the pass giving a conn up and the
// loop draining the goroutine's hand-back, a queue entry names cs, and a
// transplant would return cs to the pool under it. The conn must not move
// until the hand-back is drained, and then it may.
func TestATransplantWaitsForARelink(t *testing.T) {
	rig := hijackRaceConn(t)
	l, cs, local := rig.l, rig.cs, rig.local
	l.async = true
	cs.protocol = engine.HTTP1
	cs.detected = true
	cs.lastActivity = time.Now().UnixNano()
	l.markDirty(cs)
	release := holdAsHandler(t, cs, true)
	if !returnsWhileHeld(t, release, l.flushDirty) {
		t.Fatal("the dirty pass waited on a running handler (celeris#669)")
	}
	release()
	// The goroutine exits (a later close, say) after queuing its hand-back;
	// the loop has not drained it yet.
	cs.asyncInMu.Lock()
	cs.relinkOwed = false
	cs.asyncRun = false
	cs.asyncInMu.Unlock()
	l.detachQMu.Lock()
	l.detachQueue = append(l.detachQueue, cs)
	l.detachQPending.Store(1)
	l.detachQMu.Unlock()

	target := &countingTarget{}
	t.Cleanup(target.closeAll)
	l.transplant.Store(&transplantState{target: target})
	l.tryTransplant(local)
	if n := target.count(); n != 0 {
		t.Fatalf("a conn with a hand-back still queued was transplanted (%d adopted)", n)
	}
	l.drainDetachQueue() // the hand-back: back on the dirty list
	l.flushDirty()       // nothing queued: off it again
	l.tryTransplant(local)
	if n := target.count(); n != 1 {
		t.Errorf("after the hand-back the conn was not transplanted (%d adopted): the refusal was not the relink's", n)
	}
}

// TestPostDetachHandlerIsWaitedOut: after Detach the dispatch goroutine may
// keep running — a handler that streams inline — but it never holds detachMu
// across ProcessH1 again, so whoever holds the lock is a guarded writeFn in
// one write. closeConn must wait for it as before, not leave the close to a
// goroutine that will not look until its handler returns, and the handler
// may be waiting for exactly that close's OnDetachClose.
func TestPostDetachHandlerIsWaitedOut(t *testing.T) {
	rig := hijackRaceConn(t)
	l, cs, local := rig.l, rig.cs, rig.local
	cs.asyncInMu.Lock()
	cs.asyncDetachUnlocked = true
	cs.asyncInMu.Unlock()
	release := holdAsHandler(t, cs, true)

	done := make(chan struct{})
	go func() {
		defer close(done)
		l.closeConn(local)
	}()
	select {
	case <-done:
		t.Fatal("closeConn left the close to a post-Detach goroutine instead of waiting out a guarded write")
	case <-time.After(200 * time.Millisecond):
	}
	release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("closeConn never completed after the writer released detachMu")
	}
	if l.conns[local] != nil || rig.disconnects.Load() != 1 {
		t.Errorf("close incomplete: slot=%p hooks=%d", l.conns[local], rig.disconnects.Load())
	}
}

// TestAnOwedCloseIsNeverTransplanted: between the dispatch goroutine's exit
// and the loop draining its hand-back, the conn is still in the table with no
// goroutine — the shape tryTransplant hands straight to the other engine. It
// must not: the move would release the connState the queued hand-back then
// closes through.
func TestAnOwedCloseIsNeverTransplanted(t *testing.T) {
	rig := hijackRaceConn(t)
	l, cs, local := rig.l, rig.cs, rig.local
	l.async = true
	cs.protocol = engine.HTTP1
	cs.detected = true
	release := holdAsHandler(t, cs, true)
	if !returnsWhileHeld(t, release, l.checkTimeouts) {
		t.Fatal("the reap waited on a running handler (celeris#669)")
	}
	release()
	exitDispatch(l, cs) // the goroutine is gone; its hand-back is queued, not drained

	target := &countingTarget{}
	t.Cleanup(target.closeAll)
	l.transplant.Store(&transplantState{target: target})
	l.tryTransplant(local)
	l.transplant.Store(nil)
	if n := target.count(); n != 0 {
		t.Fatalf("a conn whose close is owed was handed to the other engine (%d adopted)", n)
	}
	l.drainDetachQueue()
	if l.conns[local] != nil || l.closeCount.Load() != 1 || rig.disconnects.Load() != 1 {
		t.Errorf("the owed close did not complete: slot=%p closeCount=%d hooks=%d",
			l.conns[local], l.closeCount.Load(), rig.disconnects.Load())
	}
}

// TestPeerHalfCloseLeavesAClosingConnToItsHandler: the FIN that drainRead's
// EOF branch turned into an owed close rides the same epoll event as
// EPOLLRDHUP, so onPeerHalfClose runs next, while the handler still holds
// detachMu and is writing its response. It must leave the conn alone: its
// csWritePending would read those buffers without the lock (a data race,
// the "race" arm under -race), and act on what it read (the "pending" arm).
func TestPeerHalfCloseLeavesAClosingConnToItsHandler(t *testing.T) {
	for _, arm := range []string{"pending", "race"} {
		t.Run(arm, func(t *testing.T) {
			rig := hijackRaceConn(t)
			l, cs, local := rig.l, rig.cs, rig.local
			release := holdAsHandler(t, cs, true)
			if !returnsWhileHeld(t, release, l.checkTimeouts) {
				t.Fatal("the reap waited on a running handler (celeris#669)")
			}
			if !cs.asyncClosed.Load() {
				t.Fatal("setup: no close is owed")
			}
			if arm == "pending" {
				// The handler has already queued part of its response.
				cs.writeBuf = append(cs.writeBuf[:0], "response"...)
				cs.pendingBytes = len(cs.writeBuf)
				l.onPeerHalfClose(local)
			} else {
				done := make(chan struct{})
				go func() {
					defer close(done)
					l.onPeerHalfClose(local)
				}()
				// The handler writes its response; nothing orders this
				// after the loop's look at the conn.
				time.Sleep(20 * time.Millisecond)
				cs.writeBuf = append(cs.writeBuf[:0], "response"...)
				cs.pendingBytes = len(cs.writeBuf)
				<-done
			}
			release()
			if cs.peerClosed {
				t.Error("onPeerHalfClose acted on a conn whose close is owed to its handler")
			}
		})
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
