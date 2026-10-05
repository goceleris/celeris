//go:build linux

package iouring

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
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/ctxkit"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/engine/internal/errclass"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#704, the io_uring twin of celeris#669: with AsyncHandlers,
// runAsyncHandler holds cs.detachMu across the whole of ProcessH1, i.e. for as
// long as the user handler runs. Worker-thread sites that took that mutex with
// a blocking Lock therefore parked the LockOSThread'd worker, and every other
// connection of its ring (no CQE processed, no accept, no flush), until the
// handler returned. The sites:
//
//   - closeConn, reached by every close of a conn whose handler still runs;
//   - handleRecv's peer-FIN branch (Res == 0), which delivers OnError under
//     the lock before it closes: a client that gives up mid-handler;
//   - handleRecv's recv-error branch, the same for a reset;
//   - the dirty-list pass (flushDirty), for a conn with a dropped recv arm or
//     pending bytes whose next handler has started.
//
// The unit arms hold detachMu the way a running handler does and ask each site
// to return while it is held. The end-to-end arms are the issue's measurement:
// a slow async handler on one connection and a fast keep-alive connection on
// the SAME worker, whose latency must not carry the handler's duration.

// stallWait704 is how long a unit arm lets a site run before calling it
// parked. A site that does not wait returns in microseconds; one that waits
// returns only when the test releases the lock, which it never does first.
const stallWait704 = 2 * time.Second

// stallRig704 is one promoted async conn on a hand-built worker with a real
// ring and a real socketpair: enough for the close, recv and dirty paths.
type stallRig704 struct {
	w           *Worker
	cs          *connState
	local, peer int
	disconnects atomic.Int64
	notified    []error // OnError calls, in order
}

func newStallRig704(t *testing.T) *stallRig704 {
	t.Helper()
	ring := newTestRing(t)
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	rig := &stallRig704{local: pair[0], peer: pair[1]}
	t.Cleanup(func() { _ = unix.Close(rig.peer) })
	w := &Worker{
		ring:        ring,
		async:       true,
		conns:       make([]*connState, rig.local+1),
		liveConns:   make([]int, 0, 4),
		errs:        &errclass.Counters{},
		activeConns: &atomic.Int64{},
		closeCount:  &atomic.Uint64{},
		cfg:         resource.Config{OnDisconnect: func(string) { rig.disconnects.Add(1) }},
	}
	w.cachedNow = time.Now().UnixNano()
	cs := acquireConnState(context.Background(), rig.local, 4096, true)
	cs.protocol.Store(int32(engine.HTTP1))
	cs.detected = true
	cs.h1State = conn.NewH1State()
	cs.h1State.OnError = func(err error) { rig.notified = append(rig.notified, err) }
	cs.asyncPromoted.Store(true)
	cs.writeFn = w.makeWriteFn(cs)
	cs.lastActivity = w.cachedNow
	w.conns[rig.local] = cs
	w.addLiveConn(cs)
	w.connCount = 1
	w.activeConns.Add(1)
	rig.w, rig.cs = w, cs
	return rig
}

// holdAsHandler704 takes cs.detachMu as runAsyncHandler does around ProcessH1
// and returns the release. running=true marks the dispatch goroutine as
// running, which is what a goroutine inside a handler is; running=false marks
// it parked, so the holder is someone whose hold is bounded by one write: a
// detached conn's guarded writeFn.
func holdAsHandler704(t *testing.T, cs *connState, running bool) (release func()) {
	t.Helper()
	cs.asyncInMu.Lock()
	cs.asyncRun = true
	setParked704(cs, !running)
	cs.asyncInMu.Unlock()
	cs.detachMu.Lock()
	var once sync.Once
	release = func() { once.Do(cs.detachMu.Unlock) }
	t.Cleanup(release)
	return release
}

// returnsWhileHeld704 runs site on its own goroutine and reports whether it
// returned within stallWait704 while the caller still holds detachMu. On a
// timeout it releases the lock, so the site can finish, and waits for it.
func returnsWhileHeld704(t *testing.T, release func(), site func()) bool {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		site()
	}()
	select {
	case <-done:
		return true
	case <-time.After(stallWait704):
		release()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("site never returned even after detachMu was released")
		}
		return false
	}
}

// exitDispatch704 runs the REAL runAsyncHandler tail a dispatch goroutine
// takes after its handler returns on a conn whose close was requested: the
// loop top observes asyncClosed and exits. Synchronous, so whatever it hands
// back is on the detach queue when it returns.
func exitDispatch704(w *Worker, cs *connState) {
	w.asyncWG.Add(1)
	w.runAsyncHandler(cs)
}

func fdOpen704(fd int) bool {
	_, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	return err == nil
}

// expectWhole704: nothing may be torn down while the handler still runs; it is
// writing its response into this conn under the lock the site did not wait
// for.
func (rig *stallRig704) expectWhole(t *testing.T) {
	t.Helper()
	w := rig.w
	if w.conns[rig.local] != rig.cs || w.connCount != 1 || w.closeCount.Load() != 0 || rig.disconnects.Load() != 0 {
		t.Fatalf("the conn was torn down under a running handler: slot=%p connCount=%d closeCount=%d hooks=%d",
			w.conns[rig.local], w.connCount, w.closeCount.Load(), rig.disconnects.Load())
	}
	if !fdOpen704(rig.local) {
		t.Fatalf("fd %d closed while the handler still owns it", rig.local)
	}
}

// expectClosedOnce704: the close ran, exactly once, with its hook.
func (rig *stallRig704) expectClosedOnce(t *testing.T) {
	t.Helper()
	w := rig.w
	if w.conns[rig.local] != nil || w.connCount != 0 || w.closeCount.Load() != 1 || rig.disconnects.Load() != 1 {
		t.Errorf("the close did not complete exactly once: slot=%p connCount=%d closeCount=%d hooks=%d",
			w.conns[rig.local], w.connCount, w.closeCount.Load(), rig.disconnects.Load())
	}
	if fdOpen704(rig.local) {
		t.Errorf("fd %d still open after the close", rig.local)
	}
}

// TestIouringCloseConnDoesNotWaitForARunningAsyncHandler: closeConn, reached by
// every close of a conn whose handler still runs, must not park the worker on
// the handler's detachMu. The close is still owed: once the handler returns
// and its goroutine exits, the conn is closed exactly once, with its hook.
func TestIouringCloseConnDoesNotWaitForARunningAsyncHandler(t *testing.T) {
	rig := newStallRig704(t)
	release := holdAsHandler704(t, rig.cs, true)
	if !returnsWhileHeld704(t, release, func() { rig.w.closeConn(rig.local) }) {
		t.Fatalf("celeris#704: closeConn waited %v on cs.detachMu held by a running async handler; "+
			"the worker, and every connection of its ring, is parked until the handler returns", stallWait704)
	}
	rig.expectWhole(t)

	release() // the handler returns; its goroutine exits and hands the close back
	exitDispatch704(rig.w, rig.cs)
	rig.w.drainDetachQueue()
	rig.expectClosedOnce(t)
}

// TestIouringPeerCloseDoesNotWaitForARunningAsyncHandler: handleRecv's peer-FIN
// and recv-error branches (a client that gives up, or resets, mid-handler)
// took detachMu to tell a detached middleware (OnError) and then closed. They
// must not park either, and the notification must still be delivered, once,
// under the lock, when the deferred close runs.
func TestIouringPeerCloseDoesNotWaitForARunningAsyncHandler(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  int32
		want error
	}{
		{"fin", 0, errPeerClosed},
		{"reset", -int32(unix.ECONNRESET), errIORingRecv(-int32(unix.ECONNRESET))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newStallRig704(t)
			release := holdAsHandler704(t, rig.cs, true)
			c := &completionEntry{Res: tc.res}
			if !returnsWhileHeld704(t, release, func() { rig.w.handleRecv(c, rig.local, time.Now().UnixNano()) }) {
				t.Fatalf("celeris#704: handleRecv's %s branch waited %v on cs.detachMu held by a running "+
					"async handler; a client that disconnects mid-handler parks the whole ring", tc.name, stallWait704)
			}
			rig.expectWhole(t)
			if len(rig.notified) != 0 {
				t.Fatalf("OnError ran while the handler held detachMu: %v", rig.notified)
			}

			release()
			exitDispatch704(rig.w, rig.cs)
			rig.w.drainDetachQueue()
			rig.expectClosedOnce(t)
			if len(rig.notified) != 1 || rig.notified[0].Error() != tc.want.Error() {
				t.Errorf("OnError calls = %v, want exactly one %v", rig.notified, tc.want)
			}
		})
	}
}

// TestIouringDirtyPassDoesNotWaitForARunningAsyncHandler: a conn on the dirty
// list (here a recv arm the SQ ring dropped) whose next handler has started.
// The pass must move on, and stop polling the conn: a dirty list that never
// empties holds the ring at a zero wait, a spin. The goroutine owes the conn
// back, and hands it back at its next park; the pass then does its work.
func TestIouringDirtyPassDoesNotWaitForARunningAsyncHandler(t *testing.T) {
	rig := newStallRig704(t)
	w, cs := rig.w, rig.cs
	cs.needsRecv = true
	w.markDirty(cs)
	release := holdAsHandler704(t, cs, true)

	if !returnsWhileHeld704(t, release, w.flushDirty) {
		t.Fatalf("celeris#704: the dirty-list pass waited %v on cs.detachMu held by a running async handler",
			stallWait704)
	}
	if cs.dirty || w.dirtyHead != nil {
		t.Errorf("the pass left the conn on the dirty list: the ring would spin at a zero wait " +
			"for as long as the handler runs")
	}
	if !relinkOwed704(cs) {
		t.Errorf("the pass gave the conn up without a hand-back owed")
	}

	// The handler returns; the REAL dispatch loop reaches its park and hands
	// the conn back.
	release()
	w.asyncWG.Add(1)
	go w.runAsyncHandler(cs)
	for dl := time.Now().Add(5 * time.Second); w.detachQPending.Load() == 0; {
		if time.Now().After(dl) {
			t.Fatal("the dispatch goroutine parked without handing the conn back")
		}
		time.Sleep(time.Millisecond)
	}
	w.drainDetachQueue()
	if !cs.dirty {
		t.Fatal("the hand-back did not put the conn back on the dirty list")
	}
	w.flushDirty()
	if cs.needsRecv || !cs.recvArmed {
		t.Errorf("the pass did not arm the owed recv after the hand-back (needsRecv=%v recvArmed=%v)",
			cs.needsRecv, cs.recvArmed)
	}
	// End the goroutine.
	w.closeConn(rig.local)
	w.asyncWG.Wait()
}

// TestIouringHandBackAfterAHandOffArmsNothing: the dirty pass's hand-back
// (relinkOwed) is enqueued at the top of the dispatch loop, and in the same
// asyncInMu section the park boundary can claim an io_uring->epoll hand-off
// (#383 reverse) and enqueue the conn a second time. The drain hands the conn
// off on one entry: handOff clears its slot and closes its fd. The other entry
// must not put it back on the dirty list, where the pass would arm the recv
// the conn was owed on the closed fd number, which a new socket can hold by
// then, and take that socket's bytes (the celeris#527/#657 fd-lifetime class;
// found in review of PR #745).
//
// Arm dirty_at_handoff is main's flow and a control: the pass never met the
// handler, so the conn is still on the dirty list when handOff unlinks it
// (celeris#527's removeDirty). Arm relink_owed is celeris#704's: the pass gave
// the conn up while its handler held detachMu.
func TestIouringHandBackAfterAHandOffArmsNothing(t *testing.T) {
	for _, arm := range []string{"dirty_at_handoff", "relink_owed"} {
		t.Run(arm, func(t *testing.T) {
			rig := newStallRig704(t)
			w, cs, oldFD := rig.w, rig.cs, rig.local
			w.asyncCancelFlags = true // a worker that can reap: the async hand-off is offered
			tgt := &recordingTarget{}

			// A recv arm the SQ ring dropped for this promoted conn.
			cs.needsRecv = true
			w.markDirty(cs)
			release := holdAsHandler704(t, cs, true)
			if arm == "relink_owed" {
				w.flushDirty() // meets the running handler: gives the conn up
				if !relinkOwed704(cs) || cs.dirty {
					t.Fatalf("apparatus: the pass did not give the conn up (relinkOwed=%v dirty=%v)",
						relinkOwed704(cs), cs.dirty)
				}
			}
			// An io_uring->epoll drain is active when the handler returns.
			w.transplant.Store(&transplantTargetHolder{target: tgt})
			release()

			// The REAL dispatch loop after the handler: its loop top, then its
			// park boundary, which claims the hand-off and exits.
			done := make(chan struct{})
			go func() {
				defer close(done)
				exitDispatch704(w, cs)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("apparatus: the dispatch goroutine parked instead of claiming the hand-off")
			}
			w.drainDetachQueue()
			if tgt.adopted.Load() != 1 || w.conns[oldFD] != nil || fdOpen704(oldFD) {
				t.Fatalf("apparatus: no hand-off (adopted=%d slot=%p fd_open=%v)",
					tgt.adopted.Load(), w.conns[oldFD], fdOpen704(oldFD))
			}
			t.Logf("celeris#704 HANDOFF arm=%s after_drain dirty=%v needsRecv=%v", arm, cs.dirty, cs.needsRecv)
			if cs.dirty {
				t.Errorf("a queue entry put the handed-off conn back on the dirty list")
			}

			// A new socket takes the number the hand-off closed, as the next
			// accept would (dup3 onto it, so the test does not depend on which
			// free number the kernel picks).
			pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
			if err != nil {
				t.Fatalf("socketpair: %v", err)
			}
			stranger, strangerPeer := pair[0], pair[1]
			if stranger != oldFD {
				if err := unix.Dup3(pair[0], oldFD, 0); err != nil {
					t.Fatalf("dup3: %v", err)
				}
				_ = unix.Close(pair[0])
				stranger = oldFD
			}
			t.Cleanup(func() { _ = unix.Close(stranger); _ = unix.Close(strangerPeer) })

			w.flushDirty()
			if _, err := w.ring.Submit(); err != nil {
				t.Fatalf("submit: %v", err)
			}
			if _, err := unix.Write(strangerPeer, []byte("STRANGER")); err != nil {
				t.Fatalf("write: %v", err)
			}
			if cs.recvArmed {
				// Show what the stray arm does: wait for its completion.
				if err := w.ring.WaitCQETimeout(2 * time.Second); err == nil {
					head, tail := w.ring.BeginCQ()
					for h := head; h != tail; h++ {
						c := w.ring.cqeAt(h)
						t.Logf("celeris#704 HANDOFF arm=%s cqe op=%#x fd=%d res=%d",
							arm, decodeOp(c.UserData)>>56, decodeFD(c.UserData), c.Res)
					}
					w.ring.EndCQ(tail)
				}
			}
			var b [16]byte
			n, _, rerr := unix.Recvfrom(stranger, b[:], unix.MSG_DONTWAIT)
			got := ""
			if n > 0 {
				got = string(b[:n])
			}
			t.Logf("celeris#704 HANDOFF arm=%s stray_arm=%v stranger_read=%q err=%v", arm, cs.recvArmed, got, rerr)
			if cs.recvArmed || got != "STRANGER" {
				t.Errorf("the handed-off conn's owed recv was armed on fd %d, which another socket now "+
					"holds (stray_arm=%v); that socket read %q (err %v), want \"STRANGER\"",
					oldFD, cs.recvArmed, got, rerr)
			}
		})
	}
}

// TestIouringCloseStillWaitsForABoundedHolder is the negative control for the
// unit arms. The dispatch goroutine is PARKED, so whoever holds detachMu is a
// guarded writeFn in the middle of one write, a hold bounded by a syscall,
// which closeConn has always waited out so the write cannot race the teardown.
// That must not change: the close waits, then completes.
func TestIouringCloseStillWaitsForABoundedHolder(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(cs *connState) (running bool)
	}{
		// A parked goroutine: the holder is someone else, for one write.
		{"parked", func(*connState) bool { return false }},
		// A handler that keeps streaming after Detach no longer holds the
		// lock across ProcessH1: its writes take it one at a time, and a
		// close left to it would wait for a handler that waits for the
		// close's OnDetachClose to stop.
		{"after_detach", func(cs *connState) bool { cs.asyncDetachUnlocked = true; return true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newStallRig704(t)
			release := holdAsHandler704(t, rig.cs, tc.setup(rig.cs))
			done := make(chan struct{})
			go func() {
				defer close(done)
				rig.w.closeConn(rig.local)
			}()
			select {
			case <-done:
				t.Fatal("closeConn returned while a bounded holder held detachMu; the write could race the teardown")
			case <-time.After(200 * time.Millisecond):
			}
			release()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("closeConn never completed after the holder released detachMu")
			}
			rig.expectClosedOnce(t)
		})
	}
}

// ---- end to end: the issue's measurement --------------------------------

// stallSlow704 is the async handler's duration; the trigger fires 50 ms into
// it, so a parked worker holds the fast conn for ~750 ms.
const stallSlow704 = 800 * time.Millisecond

// stallBudget704 is the most one fast request may take. A worker that is not
// parked serves it in well under a millisecond; the gap absorbs a loaded
// -race run.
const stallBudget704 = 300 * time.Millisecond

// stallHandler704 answers /fast inline with the serving worker's id, and /slow
// on the dispatch goroutine after stallSlow704.
type stallHandler704 struct{}

func (stallHandler704) HandleStream(ctx context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	body := "slow"
	if s.Path == "/slow" {
		time.Sleep(stallSlow704)
	} else {
		id, _ := ctxkit.WorkerIDFrom(ctx)
		body = "w=" + strconv.Itoa(id)
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", strconv.Itoa(len(body))}},
		[]byte(body))
}
func (stallHandler704) RouteAsync(_, path string) bool { return path == "/slow" }
func (stallHandler704) HasAsyncRoutes() bool           { return true }

var _ stream.AsyncRouteResolver = stallHandler704{}

type stallConn704 struct {
	c  net.Conn
	br *bufio.Reader
}

func stallDial704(t *testing.T, addr string) *stallConn704 {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &stallConn704{c: c, br: bufio.NewReader(c)}
}

func (s *stallConn704) get(path string, deadline time.Duration) (string, error) {
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

// colocate704 dials until a connection lands on the worker that serves slow,
// which SO_REUSEPORT decides per connection (one worker at 8 MiB memlock).
func colocate704(t *testing.T, addr string, slow *stallConn704) (*stallConn704, string) {
	t.Helper()
	want, err := slow.get("/fast", 3*time.Second)
	if err != nil {
		t.Fatalf("probe slow conn: %v", err)
	}
	for range 64 {
		f := stallDial704(t, addr)
		got, err := f.get("/fast", 3*time.Second)
		if err != nil {
			t.Fatalf("probe candidate: %v", err)
		}
		if got == want {
			return f, want
		}
		_ = f.c.Close()
	}
	t.Fatalf("no connection landed on worker %s in 64 dials", want)
	return nil, ""
}

// runStall704 drives one trigger: the slow conn sends /slow, trigger acts on
// it mid-handler, and the co-located fast conn is pinged throughout.
func runStall704(t *testing.T, name string, trigger func(*stallConn704), expectResponse bool) {
	e, addr := startFDLEngine(t, stallHandler704{}, func(c *resource.Config) {
		c.AsyncHandlers = true
	})
	slow := stallDial704(t, addr)
	fast, worker := colocate704(t, addr, slow)

	stop := make(chan struct{})
	type result struct {
		lat []time.Duration
		err error
	}
	pinged := make(chan result, 1)
	go func() {
		var r result
		for {
			select {
			case <-stop:
				pinged <- r
				return
			default:
			}
			t0 := time.Now()
			if _, err := fast.get("/fast", 5*time.Second); err != nil {
				r.err = err
				pinged <- r
				return
			}
			r.lat = append(r.lat, time.Since(t0))
			time.Sleep(2 * time.Millisecond)
		}
	}()

	_ = slow.c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := slow.c.Write([]byte("GET /slow HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatalf("send /slow: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	trigger(slow)

	var body []byte
	var rerr, eofErr error
	if expectResponse {
		resp, err := http.ReadResponse(slow.br, nil)
		rerr = err
		if err == nil {
			body, rerr = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		}
		_, eofErr = slow.br.ReadByte() // the close follows the response
	} else {
		time.Sleep(stallSlow704)
	}
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
	t.Logf("celeris704 STALL trigger=%s worker=%s workers=%d samples=%d max_ms=%.1f p50_ms=%.2f fast_err=%v slow_body=%q slow_eof=%v",
		name, strings.TrimPrefix(worker, "w="), e.NumWorkers(), len(r.lat), float64(worst)/1e6, float64(p50)/1e6, r.err, body, eofErr)

	if r.err != nil {
		t.Errorf("celeris#704: the fast conn on the same worker broke while the slow handler ran: %v", r.err)
	}
	if worst > stallBudget704 {
		t.Errorf("celeris#704: a fast request on worker %s took %v (budget %v) while a %v async handler "+
			"ran on another conn of the same worker: the worker was parked on that conn's detachMu",
			worker, worst, stallBudget704, stallSlow704)
	}
	if expectResponse {
		if rerr != nil || string(body) != "slow" {
			t.Errorf("slow conn: response %q, err %v; want the handler's full answer", body, rerr)
		}
		if !errors.Is(eofErr, io.EOF) {
			t.Errorf("slow conn: after the response got %v, want EOF (the close was lost)", eofErr)
		}
	}
}

// TestIouringPeerCloseDuringASlowAsyncHandlerDoesNotStallItsWorker: the client
// half-closes 50 ms into the handler, as a client that stops waiting does.
// The FIN's recv completion reaches the peer-close branch and closeConn.
func TestIouringPeerCloseDuringASlowAsyncHandlerDoesNotStallItsWorker(t *testing.T) {
	runStall704(t, "peerclose", func(s *stallConn704) {
		if err := s.c.(*net.TCPConn).CloseWrite(); err != nil {
			t.Fatalf("half-close: %v", err)
		}
	}, true)
}

// TestIouringPeerResetDuringASlowAsyncHandlerDoesNotStallItsWorker: the client
// resets 50 ms into the handler. The recv completes with -ECONNRESET, the
// recv-error branch.
func TestIouringPeerResetDuringASlowAsyncHandlerDoesNotStallItsWorker(t *testing.T) {
	runStall704(t, "reset", func(s *stallConn704) {
		_ = s.c.(*net.TCPConn).SetLinger(0)
		_ = s.c.Close()
	}, false)
}
