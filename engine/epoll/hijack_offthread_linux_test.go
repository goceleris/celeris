//go:build linux

package epoll

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/engine/internal/errclass"
	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// celeris#668: with AsyncHandlers, Context.Hijack runs hijackConn on the
// connection's dispatch goroutine, and hijackConn used to remove the conn from
// l.liveConns and decrement l.connCount right there. Both are loop-thread-only
// state: checkTimeouts, the post-switch sweep (celeris#657, which added the
// sweepDormant/sweepNext writes removeLiveConn makes) and shutdown walk
// liveConns by index, and acceptAll's connCount++ can lose the goroutine's
// decrement, after which the DRAINING -> SUSPENDED gate (connCount == 0) never
// passes on that loop again.
//
// The fix leaves those two to the loop: the dispatch goroutine exits after the
// hijack and hands the connState back through the detach queue, and
// drainDetachQueue's hijacked branch removes it from the live set and
// decrements connCount on the loop thread. Until then the entry stays in the
// live set with the descriptor already closed — and the kernel may hand the
// number to the next accept — so the walkers skip a hijacked entry and the
// live set is kept by connState, not by descriptor number.
//
// Under -race (CI's root step) the two walker arms fail on the unfixed code
// with a data-race report; without -race they fail on the ownership assertion.

// offThreadRig is the #654 socketpair rig (one async conn whose dispatch
// goroutine is running) plus bystanders: connections the walkers must keep
// visiting while the hijack happens. Bystanders carry fake descriptors; with
// no timeouts configured the walkers never act on them.
func offThreadRig(t *testing.T, bystanders int) (*hijackRaceRig, []*connState) {
	t.Helper()
	rig := hijackRaceConn(t)
	rig.l.cfg.ReadTimeout = 0
	rig.cs.lastActivity = time.Now().UnixNano()
	now := time.Now().UnixNano()
	bs := make([]*connState, bystanders)
	for i := range bs {
		b := &connState{fd: 900 + i, liveIdx: -1, lastActivity: now}
		rig.l.conns[b.fd] = b
		rig.l.addLiveConn(b)
		rig.l.connCount++
		bs[i] = b
	}
	return rig, bs
}

// assertLiveSet checks that l.liveConns holds exactly want, each once, and
// that every entry's liveIdx is its index.
func assertLiveSet(t *testing.T, l *Loop, want ...*connState) {
	t.Helper()
	if len(l.liveConns) != len(want) {
		t.Errorf("len(liveConns) = %d, want %d", len(l.liveConns), len(want))
	}
	seen := make(map[*connState]int)
	for i := range l.liveConns {
		e := liveEntry(l, i)
		if e == nil {
			t.Errorf("liveConns[%d] resolves to no connState (a stale entry)", i)
			continue
		}
		if e.liveIdx != i {
			t.Errorf("liveConns[%d] is fd %d with liveIdx %d", i, e.fd, e.liveIdx)
		}
		seen[e]++
	}
	for _, w := range want {
		if seen[w] != 1 {
			t.Errorf("fd %d appears %d times in liveConns, want once", w.fd, seen[w])
		}
	}
}

// handBack is what runAsyncHandler does on its way out after ProcessH1
// returned ErrHijacked: it clears asyncRun and enqueues the connState. Then
// the loop drains the queue.
func handBack(l *Loop, cs *connState) {
	cs.asyncInMu.Lock()
	cs.asyncRun = false
	cs.asyncInMu.Unlock()
	l.detachQMu.Lock()
	l.detachQueue = append(l.detachQueue, cs)
	l.detachQPending.Store(1)
	l.detachQMu.Unlock()
	l.drainDetachQueue()
}

// hijackWhileWalking runs walk on its own goroutine, standing in for the loop
// thread, while this goroutine — standing in for the dispatch goroutine,
// holding detachMu as runAsyncHandler does across ProcessH1 — hijacks the conn.
func hijackWhileWalking(t *testing.T, rig *hijackRaceRig, walk func()) net.Conn {
	t.Helper()
	l, cs := rig.l, rig.cs
	stop := make(chan struct{})
	var walked atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			walk()
			walked.Add(1)
		}
	}()
	for walked.Load() < 3 {
		time.Sleep(time.Millisecond)
	}
	cs.detachMu.Lock()
	nc, err := l.hijackConn(rig.local)
	for n := walked.Load() + 3; walked.Load() < n; {
		time.Sleep(time.Millisecond)
	}
	cs.detachMu.Unlock()
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatalf("hijackConn: %v", err)
	}
	t.Cleanup(func() { _ = nc.Close() })
	return nc
}

// TestOffThreadHijackLeavesTheLiveSetToTheLoopDuringAReap: the walker is the
// timeout reap.
func TestOffThreadHijackLeavesTheLiveSetToTheLoopDuringAReap(t *testing.T) {
	rig, bs := offThreadRig(t, 16)
	l, cs := rig.l, rig.cs
	hijackWhileWalking(t, rig, l.checkTimeouts)
	checkOffThreadHandBack(t, l, cs, bs)
}

// TestOffThreadHijackLeavesTheLiveSetToTheLoopDuringASweep: the walker is the
// post-switch sweep (celeris#657), whose dormancy fields removeLiveConn also
// writes.
func TestOffThreadHijackLeavesTheLiveSetToTheLoopDuringASweep(t *testing.T) {
	rig, bs := offThreadRig(t, 16)
	l, cs := rig.l, rig.cs
	target := &countingTarget{}
	t.Cleanup(target.closeAll)
	l.transplant.Store(&transplantState{target: target})
	hijackWhileWalking(t, rig, l.sweep)
	checkOffThreadHandBack(t, l, cs, bs)
	if n := target.count(); n != 0 {
		t.Errorf("the sweep handed %d conns to the target; none was movable", n)
	}
}

// checkOffThreadHandBack asserts the ownership rule on both sides of the
// hand-back: the loop's live set and connCount still count the hijacked conn
// until its dispatch goroutine has exited, and drop it exactly once after.
// The public gauges move at the hijack itself.
func checkOffThreadHandBack(t *testing.T, l *Loop, cs *connState, bs []*connState) {
	t.Helper()
	if got := l.activeConns.Load(); got != 0 {
		t.Errorf("activeConns = %d after the hijack, want 0 (the gauge moves at the hijack)", got)
	}
	if got := l.closeCount.Load(); got != 1 {
		t.Errorf("closeCount = %d after the hijack, want 1", got)
	}
	if cs.liveIdx < 0 || l.connCount != len(bs)+1 {
		t.Fatalf("celeris#668: hijackConn changed loop-thread-only state from the dispatch goroutine: "+
			"liveIdx=%d connCount=%d, want the conn still in the live set and connCount %d until the "+
			"goroutine has exited", cs.liveIdx, l.connCount, len(bs)+1)
	}
	handBack(l, cs)
	if l.connCount != len(bs) {
		t.Errorf("connCount = %d after the hand-back, want %d", l.connCount, len(bs))
	}
	assertLiveSet(t, l, bs...)
	if !cs.hijackSettled {
		t.Error("the hand-back did not settle the hijack")
	}
}

// TestHijackedEntryDoesNotAliasTheNextOwnerOfItsDescriptor guards the fix's
// own hazard. Between the hijack and the hand-back the hijacked conn's entry
// is still in the live set, but its descriptor is closed and the kernel may
// give the number to the next accept. A live set kept by descriptor number
// then has two entries for one number: swap-removes fix up the wrong
// connState's liveIdx, the hand-back cannot find the stale entry, and it stays
// forever (shutdown would close that number twice). Leaving removeLiveConn
// where it was and deferring only the call site — the literal "option (b)" —
// fails here.
func TestHijackedEntryDoesNotAliasTheNextOwnerOfItsDescriptor(t *testing.T) {
	rig, bs := offThreadRig(t, 3)
	l, cs, local := rig.l, rig.cs, rig.local
	l.cfg.ReadTimeout = 100 * time.Millisecond
	// Put the hijacked conn at the tail, so a later swap-remove moves it.
	l.removeLiveConn(cs)
	l.addLiveConn(cs)
	// Expired: if its stale entry were read as the next owner, the reap
	// would close that owner.
	cs.lastActivity = time.Now().Add(-time.Hour).UnixNano()

	cs.detachMu.Lock()
	nc, err := l.hijackConn(local)
	if err != nil {
		cs.detachMu.Unlock()
		t.Fatalf("hijackConn: %v", err)
	}
	t.Cleanup(func() { _ = nc.Close() })

	// The number is reissued and accepted again on this loop.
	pipeWrite := hijackRaceRecycle(t, local)
	next := &connState{fd: local, liveIdx: -1, lastActivity: time.Now().UnixNano()}
	next.h1State = conn.NewH1State()
	l.driverMu.Lock()
	l.conns[local] = next
	l.driverMu.Unlock()
	l.addLiveConn(next)
	l.connCount++
	l.activeConns.Add(1)

	// Two other conns leave: the first swap moves the new owner, the second
	// moves the hijacked conn's entry.
	l.removeLiveConn(bs[0])
	l.connCount--
	l.removeLiveConn(bs[1])
	l.connCount--

	l.checkTimeouts()
	if l.conns[local] != next || !fdOpen(local) {
		t.Fatalf("the reap acted on the new owner of fd %d through the hijacked conn's entry", local)
	}

	cs.detachMu.Unlock()
	handBack(l, cs)

	assertLiveSet(t, l, next, bs[2])
	if l.connCount != 2 {
		t.Errorf("connCount = %d, want 2 (the new owner and one bystander)", l.connCount)
	}
	if _, err := unix.Write(pipeWrite, []byte("z")); err != nil {
		t.Errorf("write to the new owner's pipe: %v", err)
	} else {
		hijackRaceExpectRead(t, local, "z", "the new owner's descriptor still works")
	}
}

// ---- end to end: accept churn and async hijacks on a real engine --------

// hijackChurnHandler: /hj is async and hijacks, answering on the raw conn;
// everything else answers inline.
type hijackChurnHandler struct{ hijacked *atomic.Int64 }

func (h hijackChurnHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	if s.Path != "/hj" {
		return s.ResponseWriter.WriteResponse(s, 200,
			[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}}, []byte("ok"))
	}
	// Give the loop time to accept and close other conns after this
	// goroutine last synchronised with it.
	time.Sleep(5 * time.Millisecond)
	hj, ok := s.ResponseWriter.(stream.Hijacker)
	if !ok {
		return fmt.Errorf("response writer %T cannot hijack", s.ResponseWriter)
	}
	c, err := hj.Hijack(s)
	if err != nil {
		return err
	}
	h.hijacked.Add(1)
	_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nhj")
	return c.Close()
}
func (hijackChurnHandler) RouteAsync(_, path string) bool { return path == "/hj" }
func (hijackChurnHandler) HasAsyncRoutes() bool           { return true }

// TestAsyncHijackUnderAcceptChurnLeavesTheLoopsSuspendable is the issue's
// counter-level witness: hijack from dispatch goroutines while the loops
// accept and close other conns, then pause accepting. Every loop must reach
// SUSPENDED with connCount 0 and an empty live set.
func TestAsyncHijackUnderAcceptChurnLeavesTheLoopsSuspendable(t *testing.T) {
	var hijacked atomic.Int64
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	e, err := New(resource.Config{
		Addr:          addr,
		Protocol:      engine.HTTP1,
		Resources:     resource.Resources{Workers: 2},
		AsyncHandlers: true,
	}, hijackChurnHandler{hijacked: &hijacked})
	if err != nil {
		t.Fatalf("epoll engine: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- e.Listen(ctx) }()
	defer func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
		}
	}()
	for dl := time.Now().Add(10 * time.Second); e.Addr() == nil && time.Now().Before(dl); {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Fatal("engine did not bind")
	}

	get := func(path string) error {
		c, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: x\r\n\r\n", path); err != nil {
			return err
		}
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			return err
		}
		_, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return err
	}

	const hijackers, perHijacker, churners = 4, 16, 4
	stop := make(chan struct{})
	var churned, failures atomic.Int64
	var churnWG, hjWG sync.WaitGroup
	for range churners {
		churnWG.Add(1)
		go func() {
			defer churnWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if get("/churn") == nil {
					churned.Add(1)
				} else {
					failures.Add(1)
				}
			}
		}()
	}
	for range hijackers {
		hjWG.Add(1)
		go func() {
			defer hjWG.Done()
			for range perHijacker {
				if get("/hj") != nil {
					failures.Add(1)
				}
			}
		}()
	}
	hjWG.Wait()
	close(stop)
	churnWG.Wait()

	for dl := time.Now().Add(5 * time.Second); e.Metrics().ActiveConnections != 0 && time.Now().Before(dl); {
		time.Sleep(10 * time.Millisecond)
	}
	if err := e.PauseAccept(); err != nil {
		t.Fatalf("PauseAccept: %v", err)
	}
	suspended := 0
	for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl); {
		suspended = 0
		for _, l := range e.loops {
			if l.suspended.Load() {
				suspended++
			}
		}
		if suspended == len(e.loops) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Logf("celeris668 HIJACKCHURN hijacked=%d churned=%d failures=%d active=%d suspended=%d/%d",
		hijacked.Load(), churned.Load(), failures.Load(), e.Metrics().ActiveConnections, suspended, len(e.loops))

	if got := hijacked.Load(); got != hijackers*perHijacker {
		t.Errorf("hijacked %d conns, want %d", got, hijackers*perHijacker)
	}
	if suspended != len(e.loops) {
		t.Fatalf("celeris#668: %d/%d loops reached SUSPENDED after the pause; a loop whose connCount "+
			"lost an update to an off-thread hijack never passes the connCount == 0 gate", suspended, len(e.loops))
	}
	// Each loop is parked; its last writes happened before it published
	// suspended, so reading them here is ordered.
	for i, l := range e.loops {
		if l.connCount != 0 || len(l.liveConns) != 0 {
			t.Errorf("loop %d suspended with connCount=%d liveConns=%d, want 0 and 0", i, l.connCount, len(l.liveConns))
		}
	}
}

// TestAcceptOfANumberAHijackReleasedIsOrderedAfterTheHijack pins why accept
// installs its slot under driverMu. An off-thread hijack clears its slot under
// that lock and then closes the descriptor; the kernel hands the lowest free
// number to the next accept4, so the loop can install a new conn at the very
// slot the dispatch goroutine just wrote, with nothing else ordering the two.
// The loop side below learns that the number is free from the kernel alone
// (F_GETFD), exactly as accept4 does, so under -race the only thing that can
// order the two slot writes is the lock.
func TestAcceptOfANumberAHijackReleasedIsOrderedAfterTheHijack(t *testing.T) {
	epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		t.Fatalf("epoll_create1: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(epfd) })
	lfd, err := createListenSocket("127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen socket: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(lfd) })
	addr := boundAddr(lfd).String()
	l := &Loop{
		epollFD:      epfd,
		listenFD:     lfd,
		async:        true,
		conns:        make([]*connState, connTableSize),
		activeConns:  &atomic.Int64{},
		errs:         &errclass.Counters{},
		reqCount:     &atomic.Uint64{},
		acceptCount:  &atomic.Uint64{},
		closeCount:   &atomic.Uint64{},
		bytesRead:    &atomic.Uint64{},
		bytesWritten: &atomic.Uint64{},
		timerFD:      -1,
		resolved:     resource.ResolvedResources{BufferSize: 4096},
	}
	dial := func() net.Conn {
		c, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	acceptOne := func() {
		for dl := time.Now().Add(3 * time.Second); time.Now().Before(dl); {
			before := len(l.liveConns)
			l.acceptAll(context.Background(), time.Now().UnixNano())
			if len(l.liveConns) > before {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("accept timed out")
	}

	dial()
	acceptOne()
	cs := l.liveConns[0]
	fd := cs.fd
	cs.protocol = engine.HTTP1
	cs.detected = true
	cs.h1State = conn.NewH1State()
	cs.asyncInMu.Lock()
	cs.asyncRun = true // the conn's dispatch goroutine is alive: the hijack is off-thread
	cs.asyncInMu.Unlock()
	dial() // queued before the hijack frees fd, so the next accept4 takes fd

	hijacked := make(chan net.Conn, 1)
	go func() { // the dispatch goroutine, inside the handler
		cs.detachMu.Lock()
		nc, herr := l.hijackConn(fd)
		cs.detachMu.Unlock()
		if herr != nil {
			nc = nil
		}
		hijacked <- nc
	}()
	accepted := make(chan struct{})
	go func() { // the loop thread
		defer close(accepted)
		for {
			if _, ferr := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); ferr != nil {
				break
			}
			time.Sleep(50 * time.Microsecond)
		}
		for range 3000 {
			if l.acceptAll(context.Background(), time.Now().UnixNano()) != acceptDrained || len(l.liveConns) > 1 {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	<-accepted
	nc := <-hijacked
	if nc == nil {
		t.Fatal("hijackConn failed")
	}
	t.Cleanup(func() { _ = nc.Close() })

	if len(l.liveConns) != 2 {
		t.Fatalf("liveConns = %d entries after the second accept, want 2 (the hijacked conn until its hand-back, "+
			"and the new one)", len(l.liveConns))
	}
	next := l.liveConns[1]
	if next.fd != fd {
		t.Skipf("the kernel gave the new conn fd %d, not the released %d; the reuse under test did not happen", next.fd, fd)
	}
	if l.conns[fd] != next {
		t.Fatalf("slot %d holds %p, want the new conn %p", fd, l.conns[fd], next)
	}
	handBack(l, cs)
	assertLiveSet(t, l, next)
	if l.connCount != 1 {
		t.Errorf("connCount = %d after the hand-back, want 1", l.connCount)
	}
}

// TestOffThreadHijackIsSettledBeforeItsGoroutineExits: a handler that hijacks
// may go on serving the hijacked conn for as long as it likes, on the
// dispatch goroutine. The loop must not wait for that goroutine to exit
// before taking the conn out of its live set and connCount — for a session
// served in-handler that would pin the DRAINING -> SUSPENDED gate for the
// whole session, and a handler that returns into a Detach-ed loop never
// exits at all. hijackConn's notice settles it at the next drain; the
// goroutine's own exit entry, later, is a no-op.
func TestOffThreadHijackIsSettledBeforeItsGoroutineExits(t *testing.T) {
	rig, bs := offThreadRig(t, 2)
	l, cs, local := rig.l, rig.cs, rig.local
	cs.detachMu.Lock()
	nc, err := l.hijackConn(local)
	cs.detachMu.Unlock()
	if err != nil {
		t.Fatalf("hijackConn: %v", err)
	}
	t.Cleanup(func() { _ = nc.Close() })

	// The goroutine is still running (asyncRun): only the notice is queued.
	l.drainDetachQueue()
	if !cs.hijackSettled || cs.liveIdx != -1 || l.connCount != len(bs) {
		t.Fatalf("the hijack was not settled while its goroutine still runs: settled=%v liveIdx=%d connCount=%d, "+
			"want true, -1, %d", cs.hijackSettled, cs.liveIdx, l.connCount, len(bs))
	}
	assertLiveSet(t, l, bs...)

	// Its exit, later, hands cs back again: nothing more may happen.
	handBack(l, cs)
	if l.connCount != len(bs) {
		t.Errorf("the goroutine's exit entry moved connCount to %d, want %d", l.connCount, len(bs))
	}
	assertLiveSet(t, l, bs...)
}

// TestDirtyPassSkipsAHijackedConn: a conn still on the dirty list — a
// pipelined response left partial — when its next request's handler hijacks.
// Once the handler returns and releases detachMu, the dirty pass can take the
// lock before the loop has drained the hijack's notice; the descriptor it
// would write to is closed, and its number already reissued. Nothing queued
// may reach the number's new owner.
func TestDirtyPassSkipsAHijackedConn(t *testing.T) {
	rig := hijackRaceConn(t)
	l, cs, local := rig.l, rig.cs, rig.local
	cs.writeBuf = append(cs.writeBuf[:0], "pending"...)
	cs.pendingBytes = len(cs.writeBuf)
	l.markDirty(cs)

	cs.detachMu.Lock()
	nc, err := l.hijackConn(local)
	if err != nil {
		cs.detachMu.Unlock()
		t.Fatalf("hijackConn: %v", err)
	}
	t.Cleanup(func() { _ = nc.Close() })

	// The number is reissued to the WRITE end of a pipe, so a stray write
	// lands where the test can read it.
	var p [2]int
	if err := unix.Pipe2(p[:], unix.O_CLOEXEC|unix.O_NONBLOCK); err != nil {
		cs.detachMu.Unlock()
		t.Fatalf("pipe2: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(p[0]) })
	if _, err := unix.FcntlInt(uintptr(local), unix.F_GETFD, 0); err == nil {
		cs.detachMu.Unlock()
		t.Skipf("fd %d in use again before the test could take it", local)
	}
	if err := unix.Dup3(p[1], local, unix.O_CLOEXEC); err != nil {
		cs.detachMu.Unlock()
		t.Fatalf("dup3: %v", err)
	}
	_ = unix.Close(p[1])
	t.Cleanup(func() { _ = unix.Close(local) })

	cs.detachMu.Unlock() // the handler returns
	l.flushDirty()       // before the loop drains the hijack's notice

	buf := make([]byte, 64)
	if n, err := unix.Read(p[0], buf); n > 0 {
		t.Fatalf("the dirty pass wrote %q to fd %d, which the hijack had released and another file now owns",
			buf[:n], local)
	} else if err != unix.EAGAIN {
		t.Fatalf("read the pipe: %v", err)
	}
	if cs.dirty {
		t.Error("the hijacked conn is still on the dirty list")
	}
}
