//go:build linux

package epoll

import (
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/conn"
)

// celeris#654: with AsyncHandlers, a handler that calls Hijack runs
// hijackConn ON the dispatch goroutine, under cs.detachMu. A loop-thread
// closeConn for the same fd (checkTimeouts' read/idle/header sweep, or the
// async input-backpressure close in drainRead) reads the still-non-nil
// connState, then parks on cs.detachMu behind the handler. When the handler
// returns, closeConn resumes and re-runs a teardown hijackConn already
// performed — on a connection the engine no longer owns.
//
// These tests drive that interleaving directly rather than through a socket,
// so the ordering is pinned by a lock and an atomic instead of by timing.
// They construct the same bare Loop the other in-package loop tests use; the
// loop is not running, so the "loop thread" is just another goroutine.

// hijackRaceConn builds an async-mode HTTP1 connection on a socketpair and
// registers it with l exactly as acceptAll would: armed in epoll, in the conn
// table, in the live set, counted. h1State is non-nil and never Detached,
// which is what selects closeConn's plainClose branch (SHUT_WR + Close) —
// the path that closes the descriptor a second time.
func hijackRaceConn(t *testing.T) (l *Loop, cs *connState, local, peer int) {
	t.Helper()
	l = newReapLoop(t)

	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Skipf("socketpair unavailable: %v", err)
	}
	local, peer = pair[0], pair[1]
	t.Cleanup(func() { _ = unix.Close(peer) })
	if local >= connTableSize {
		_ = unix.Close(local)
		t.Skipf("socketpair fd %d exceeds connTableSize %d", local, connTableSize)
	}
	// Reads on peer are only ever done after a completed write on the other
	// end, so non-blocking cannot lose data — it only keeps a broken
	// expectation from hanging the test instead of failing it.
	if err := unix.SetNonblock(peer, true); err != nil {
		_ = unix.Close(local)
		t.Skipf("set peer non-blocking: %v", err)
	}
	if err := unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_ADD, local, &unix.EpollEvent{
		Events: unix.EPOLLIN | unix.EPOLLET | unix.EPOLLRDHUP,
		Fd:     int32(local),
	}); err != nil {
		_ = unix.Close(local)
		t.Skipf("epoll_ctl ADD fd %d: %v", local, err)
	}

	cs = &connState{fd: local, liveIdx: -1}
	cs.detachMu = &sync.Mutex{}
	cs.asyncCond.L = &cs.asyncInMu
	cs.asyncRun = true
	cs.asyncPromoted = true
	cs.h1State = conn.NewH1State()

	l.conns[local] = cs
	l.addLiveConn(cs)
	l.connCount++
	l.activeConns.Add(1)
	l.acceptCount.Add(1)
	return l, cs, local, peer
}

// hijackRaceWaitFor spins until cond holds, yielding rather than sleeping so
// the test carries no timing assumption — only a deadline to fail on.
func hijackRaceWaitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		runtime.Gosched()
	}
}

// hijackRaceExpectRead reads exactly len(want) bytes from fd and compares.
// fd must be non-blocking; EAGAIN is retried to a deadline.
func hijackRaceExpectRead(t *testing.T, fd int, want, what string) {
	t.Helper()
	buf := make([]byte, len(want))
	deadline := time.Now().Add(5 * time.Second)
	got := 0
	for got < len(want) {
		n, err := unix.Read(fd, buf[got:])
		if n > 0 {
			got += n
			continue
		}
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
			if time.Now().After(deadline) {
				t.Errorf("%s: timed out, read %q of %q", what, buf[:got], want)
				return
			}
			runtime.Gosched()
			continue
		}
		t.Errorf("%s: read fd %d: %v (read %q of %q)", what, fd, err, buf[:got], want)
		return
	}
	if string(buf) != want {
		t.Errorf("%s: read %q, want %q", what, buf, want)
	}
}

// hijackRaceRecycle takes over the descriptor number the hijack just freed,
// standing in for whatever really claims it in a server: another loop's
// accept4, a file the handler opens, a Go netpoll fd, or a driver conn
// registered off-thread. Returns the pipe's write end; the read end IS fd.
//
// The returned fd is deliberately never closed by the test. Once a teardown
// bug has closed it, the number may belong to something else, and closing it
// again would be the very mistake under test.
func hijackRaceRecycle(t *testing.T, fd int) (writeEnd int) {
	t.Helper()
	var p [2]int
	if err := unix.Pipe2(p[:], unix.O_CLOEXEC|unix.O_NONBLOCK); err != nil {
		t.Fatalf("pipe2: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(p[1]) })
	if p[0] == fd {
		// The kernel's lowest-free-fd rule handed the just-freed number
		// straight to the pipe, with no help from the test. That is the
		// recycling this test is about, so there is nothing left to set up.
		return p[1]
	}
	if err := unix.Dup3(p[0], fd, unix.O_CLOEXEC); err != nil {
		_ = unix.Close(p[0])
		t.Fatalf("dup3 pipe onto the freed fd %d: %v", fd, err)
	}
	_ = unix.Close(p[0])
	return p[1]
}

// TestCloseConnDoesNotRecloseConnHijackedWhileWaitingOnDetachMu is the
// celeris#654 regression. It pins the exact interleaving:
//
//	dispatch goroutine   loop thread
//	------------------   -----------
//	detachMu.Lock
//	(handler running)    closeConn: reads l.conns[fd] -> cs (non-nil)
//	                     closeConn: asyncClosed.Store(true)
//	                     closeConn: detachMu.Lock ... blocked
//	Hijack -> hijackConn
//	  EPOLL_CTL_DEL, l.conns[fd]=nil, counters--, close(fd)
//	(fd number reused by something else)
//	detachMu.Unlock
//	                     closeConn: resumes and tears the conn down AGAIN
//
// Before the fix, the resumed closeConn decrements the counters a second
// time and SHUT_WR + close()es a descriptor number that now belongs to an
// unrelated file. After the fix it re-reads the slot, sees it no longer owns
// cs, and returns.
func TestCloseConnDoesNotRecloseConnHijackedWhileWaitingOnDetachMu(t *testing.T) {
	l, cs, local, peer := hijackRaceConn(t)

	// (a) The dispatch goroutine is inside ProcessH1: runAsyncHandler holds
	// cs.detachMu for the whole user handler call.
	cs.detachMu.Lock()

	// (b) The loop thread reaps the conn. checkTimeouts calls closeConn
	// directly on a read/idle/write/header deadline, and nothing refreshes
	// cs.lastActivity while a handler runs.
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.closeConn(local)
	}()

	// (c) Barrier, not a sleep. closeConn stores asyncClosed only AFTER it
	// has read l.conns[fd] into its own cs, and before it blocks on
	// detachMu. Once the flag flips, closeConn is committed to this cs and
	// can only be parked on the lock we hold — the interleaving is pinned,
	// and the atomic supplies the happens-before edge the race detector
	// needs for the unsynchronized slot read.
	hijackRaceWaitFor(t, cs.asyncClosed.Load, "closeConn to capture the conn and reach its detachMu wait")

	// (d) The handler calls Hijack, still inside ProcessH1, still holding
	// detachMu. hijackConn performs the full engine-side teardown here.
	nc, err := l.hijackConn(local)
	if err != nil {
		t.Fatalf("hijackConn: %v", err)
	}
	t.Cleanup(func() { _ = nc.Close() })
	if l.conns[local] != nil {
		t.Fatalf("hijackConn left l.conns[%d] set; the race under test cannot arise", local)
	}

	// (e) The freed descriptor number is taken by something else, which is
	// what turns a double decrement into a closed stranger's fd.
	pipeWrite := hijackRaceRecycle(t, local)

	// (f) The handler returns and runAsyncHandler releases detachMu.
	cs.detachMu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("closeConn never returned after the handler released detachMu")
	}

	// One connection, one close. hijackConn already counted it.
	if got := l.closeCount.Load(); got != 1 {
		t.Errorf("closeCount = %d, want 1: closeConn counted a close hijackConn had already counted", got)
	}
	if got := l.activeConns.Load(); got != 0 {
		t.Errorf("activeConns = %d, want 0: a double decrement drives the live gauge negative", got)
	}
	if got := l.connCount; got != 0 {
		t.Errorf("connCount = %d, want 0: a negative count never satisfies the DRAINING->SUSPENDED gate", got)
	}

	// The recycled descriptor must survive. This is the damage that reaches
	// outside the connection: an unrelated fd shut down and closed.
	if _, err := unix.FcntlInt(uintptr(local), unix.F_GETFD, 0); err != nil {
		t.Errorf("fd %d was closed by the second teardown: %v", local, err)
	} else {
		if _, err := unix.Write(pipeWrite, []byte("x")); err != nil {
			t.Errorf("write to the recycled pipe: %v", err)
		} else {
			hijackRaceExpectRead(t, local, "x", "recycled pipe still wired to its writer")
		}
	}

	// drainDetachQueue tests detachClosed BEFORE the hijacked branch, so a
	// stray detachClosed strands the connState outside the pool.
	if cs.detachClosed {
		t.Error("closeConn marked a hijacked conn detachClosed; the pool release is then skipped")
	}

	// The hijacked conn belongs to its new owner and must keep working.
	if _, err := nc.Write([]byte("ping")); err != nil {
		t.Errorf("write on the hijacked conn: %v", err)
	} else {
		hijackRaceExpectRead(t, peer, "ping", "hijacked conn still carries traffic")
	}

	// The dispatch goroutine exits and hands cs back via the detachQueue
	// (runAsyncHandler's ErrHijacked path). The pool release must happen.
	cs.asyncInMu.Lock()
	cs.asyncRun = false
	cs.asyncInMu.Unlock()
	l.detachQMu.Lock()
	l.detachQueue = append(l.detachQueue, cs)
	l.detachQPending.Store(1)
	l.detachQMu.Unlock()
	l.drainDetachQueue()
	if cs.hijacked {
		t.Error("drainDetachQueue skipped the hijacked pool release (detachClosed short-circuit)")
	}
	if cs.fd != 0 {
		t.Errorf("cs.fd = %d after the hand-off, want 0 (connState not released)", cs.fd)
	}
}

// TestCloseConnAfterHijackIsNoOpWithoutTheRace is negative control 1: the
// same conn, the same hijack, the same fd recycling — but no interleaving.
// closeConn runs after the hijack has finished, reads a nil slot and returns.
// It passes before and after the fix, showing the counters and the
// recycled-fd witness are sound and that only the interleaving breaks them.
func TestCloseConnAfterHijackIsNoOpWithoutTheRace(t *testing.T) {
	l, cs, local, _ := hijackRaceConn(t)

	nc, err := l.hijackConn(local)
	if err != nil {
		t.Fatalf("hijackConn: %v", err)
	}
	t.Cleanup(func() { _ = nc.Close() })

	pipeWrite := hijackRaceRecycle(t, local)

	// No goroutine, no contended lock: closeConn simply finds the slot clear.
	l.closeConn(local)

	if got := l.closeCount.Load(); got != 1 {
		t.Errorf("closeCount = %d, want 1", got)
	}
	if got := l.activeConns.Load(); got != 0 {
		t.Errorf("activeConns = %d, want 0", got)
	}
	if got := l.connCount; got != 0 {
		t.Errorf("connCount = %d, want 0", got)
	}
	if _, err := unix.FcntlInt(uintptr(local), unix.F_GETFD, 0); err != nil {
		t.Errorf("fd %d was closed although closeConn saw a clear slot: %v", local, err)
	} else {
		if _, err := unix.Write(pipeWrite, []byte("y")); err != nil {
			t.Errorf("write to the recycled pipe: %v", err)
		} else {
			hijackRaceExpectRead(t, local, "y", "recycled pipe survives an uncontended closeConn")
		}
	}
	if cs.detachClosed {
		t.Error("closeConn touched a connState it no longer owns")
	}
}

// TestCloseConnClosesOnceWhenHandlerDoesNotHijack is negative control 2: the
// identical interleaving — loop thread parked on detachMu behind a running
// async handler — with no Hijack. closeConn still owns the conn when it
// wakes, so it must close it exactly once. It passes before and after the
// fix, showing the harness does not by itself produce a double count, and
// that the fix's ownership re-check does not suppress a legitimate close.
func TestCloseConnClosesOnceWhenHandlerDoesNotHijack(t *testing.T) {
	l, cs, local, _ := hijackRaceConn(t)

	cs.detachMu.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.closeConn(local)
	}()
	hijackRaceWaitFor(t, cs.asyncClosed.Load, "closeConn to reach its detachMu wait")

	// The handler returns without hijacking anything.
	cs.detachMu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("closeConn never returned after the handler released detachMu")
	}

	if got := l.closeCount.Load(); got != 1 {
		t.Errorf("closeCount = %d, want 1", got)
	}
	if got := l.activeConns.Load(); got != 0 {
		t.Errorf("activeConns = %d, want 0", got)
	}
	if got := l.connCount; got != 0 {
		t.Errorf("connCount = %d, want 0", got)
	}
	if l.conns[local] != nil {
		t.Errorf("closeConn left l.conns[%d] set", local)
	}
	if !cs.detachClosed {
		t.Error("closeConn did not mark the conn detachClosed; it still owned it")
	}
	// The conn really was closed here — this fd is legitimately gone.
	if _, err := unix.FcntlInt(uintptr(local), unix.F_GETFD, 0); err == nil {
		t.Errorf("fd %d still open; closeConn owned the conn and must have closed it", local)
	}
}
