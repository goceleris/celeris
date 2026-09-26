//go:build linux

package epoll

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/resource"
)

// celeris#654: with AsyncHandlers, a handler that calls Hijack runs
// hijackConn ON the dispatch goroutine, under cs.detachMu. A loop-thread
// closeConn for the same fd reads the still-non-nil connState, then parks on
// cs.detachMu behind the handler. When the handler returns, closeConn resumes
// and re-runs a teardown hijackConn already performed — on a connection the
// engine no longer owns.
//
// The loop-thread entry these tests use is the real one: checkTimeouts. It
// calls closeConn directly with no async guard, and nothing refreshes
// cs.lastActivity while a handler runs (it is written only at accept,
// on read, and on adopt). (When these tests were written, drainRead's EOF
// and error branches — the EPOLLRDHUP route the issue proposed — took
// detachMu BEFORE calling closeConn, so they waited behind the handler and
// then re-read a cleared slot; since celeris#669 they do not wait at all.)
//
// The interleaving is pinned by a lock and an atomic rather than by timing:
// closeConn stores cs.asyncClosed only AFTER capturing l.conns[fd] and
// BEFORE blocking on detachMu, so that flag is an exact happens-before
// barrier. The loop is not running, so the "loop thread" here is just
// another goroutine calling the sweep.
//
// Since celeris#669 closeConn no longer waits for a RUNNING dispatch
// goroutine: it leaves the close to it (see dispatchBusy). It still waits
// when the goroutine is parked, because then the holder is a bounded one (a
// guarded writeFn), and the ownership re-check guards that wait against any
// holder that detaches the conn meanwhile. The arms that pin the re-check
// therefore mark the goroutine parked (parkDispatch) to reach the wait, and
// let the hijack stand in for such a holder. The shape a running handler now
// produces — the close left to it, then a Hijack — is pinned separately by
// TestCloseLeftToAHandlerThatHijacksIsNotRedone.

// parkDispatch marks the rig's dispatch goroutine as parked in its
// asyncCond.Wait, so closeConn takes its (bounded) wait on detachMu.
func parkDispatch(cs *connState) {
	cs.asyncInMu.Lock()
	cs.asyncParked = true
	cs.asyncInMu.Unlock()
}

// hijackRaceRig is one bare Loop carrying a single async-mode HTTP/1 conn,
// plus the OnDisconnect counter every arm asserts on.
type hijackRaceRig struct {
	l           *Loop
	cs          *connState
	local       int
	peer        int
	disconnects *atomic.Int64
}

// hijackRaceConn builds an async-mode HTTP1 connection on a socketpair and
// registers it with l exactly as acceptAll would: armed in epoll, in the conn
// table, in the live set, counted. h1State is non-nil and never Detached,
// which is what selects closeConn's plainClose branch (SHUT_WR + Close) —
// the path that closes the descriptor a second time.
//
// cfg carries two things the arms depend on: ReadTimeout, which is what makes
// checkTimeouts reap this conn (lastActivity is stamped an hour into the
// past), and OnDisconnect, the public lifecycle callback whose delivery the
// celeris#654 early return changes.
func hijackRaceConn(t *testing.T) *hijackRaceRig {
	t.Helper()
	l := newReapLoop(t)

	disconnects := &atomic.Int64{}
	l.cfg = resource.Config{
		OnDisconnect: func(string) { disconnects.Add(1) },
		ReadTimeout:  time.Millisecond,
	}

	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Skipf("socketpair unavailable: %v", err)
	}
	local, peer := pair[0], pair[1]
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

	cs := &connState{fd: local, liveIdx: -1}
	cs.detachMu = &sync.Mutex{}
	cs.asyncCond.L = &cs.asyncInMu
	cs.asyncRun = true
	cs.asyncPromoted = true
	cs.h1State = conn.NewH1State()
	// Older than ReadTimeout, so the sweep reaps it. Nothing refreshes this
	// while a handler runs — that is the whole reachability argument.
	cs.lastActivity = time.Now().Add(-time.Hour).UnixNano()

	l.conns[local] = cs
	l.addLiveConn(cs)
	l.connCount++
	l.activeConns.Add(1)
	l.acceptCount.Add(1)
	return &hijackRaceRig{l: l, cs: cs, local: local, peer: peer, disconnects: disconnects}
}

// hijackRaceSweep runs the real reaper on its own goroutine, standing in for
// the loop thread between epoll_wait returns. checkTimeouts walks liveConns
// and calls closeConn itself — the trigger is part of the gate, not an
// assumption stated beside it.
func hijackRaceSweep(l *Loop) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.checkTimeouts()
	}()
	return done
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

// hijackRaceAwait waits for the sweep goroutine to return.
func hijackRaceAwait(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("checkTimeouts never returned after the handler released detachMu")
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
// again would be the very mistake under test. The leak is one descriptor per
// arm that reaches this point, bounded and process-local.
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
	// dup3 silently closes whatever newfd currently designates, so prove the
	// number is free before taking it. If the runtime claimed it between
	// hijackConn's close and the pipe2 above (netpoll, a profiler fd, the
	// race runtime), clobbering it would surface as a failure far from here.
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
		_ = unix.Close(p[0])
		t.Skipf("fd %d is in use again (F_GETFD: %v); refusing to dup3 over a live descriptor", fd, err)
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
//	(handler running)    checkTimeouts: read deadline expired
//	                     closeConn: reads l.conns[fd] -> cs (non-nil)
//	                     closeConn: asyncClosed.Store(true)
//	                     closeConn: detachMu.Lock ... blocked
//	Hijack -> hijackConn
//	  EPOLL_CTL_DEL, l.conns[fd]=nil, counters--, close(fd)
//	(fd number reused by something else)
//	detachMu.Unlock
//	                     closeConn: resumes and tears the conn down AGAIN
//
// Before the fix, the resumed closeConn decrements the counters a second
// time, SHUT_WR + close()es a descriptor number that now belongs to an
// unrelated file, and fires OnDisconnect for a connection that was handed
// to the application. After the fix it re-reads the slot, sees it no longer
// owns cs, and returns.
func TestCloseConnDoesNotRecloseConnHijackedWhileWaitingOnDetachMu(t *testing.T) {
	rig := hijackRaceConn(t)
	l, cs, local, peer := rig.l, rig.cs, rig.local, rig.peer
	parkDispatch(cs)

	// (a) The dispatch goroutine is inside ProcessH1: runAsyncHandler holds
	// cs.detachMu for the whole user handler call.
	cs.detachMu.Lock()

	// (b) The loop thread runs its timeout sweep. checkTimeouts finds the
	// stale lastActivity and calls closeConn itself.
	done := hijackRaceSweep(l)

	// (c) Barrier, not a sleep. closeConn stores asyncClosed only AFTER it
	// has read l.conns[fd] into its own cs, and before it blocks on
	// detachMu. Once the flag flips, closeConn is committed to this cs and
	// can only be parked on the lock we hold.
	hijackRaceWaitFor(t, cs.asyncClosed.Load, "checkTimeouts -> closeConn to capture the conn and reach its detachMu wait")

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
	hijackRaceAwait(t, done)

	// One connection, one close. hijackConn already counted it.
	if got := l.closeCount.Load(); got != 1 {
		t.Errorf("closeCount = %d, want 1: closeConn counted a close hijackConn had already counted", got)
	}
	if got := l.activeConns.Load(); got != 0 {
		t.Errorf("activeConns = %d, want 0: a double decrement drives the live gauge negative", got)
	}
	// connCount is the loop's: an off-thread hijack leaves it to the
	// hand-back (celeris#668), so it still counts the conn here and must
	// reach exactly 0 after, below.
	if got := l.connCount; got != 1 {
		t.Errorf("connCount = %d before the hand-back, want 1", got)
	}

	// OnDisconnect is a public lifecycle callback. The connection did not
	// disconnect — it was handed to the application — and the sync hijack
	// path fires nothing, so the async path must not either.
	if got := rig.disconnects.Load(); got != 0 {
		t.Errorf("OnDisconnect fired %d times for a hijacked conn, want 0", got)
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
	// stray detachClosed would skip the hijack's settling.
	if cs.detachClosed {
		t.Error("closeConn marked a hijacked conn detachClosed; the hijack is then never settled")
	}

	// The hijacked conn belongs to its new owner and must keep working.
	if _, err := nc.Write([]byte("ping")); err != nil {
		t.Errorf("write on the hijacked conn: %v", err)
	} else {
		hijackRaceExpectRead(t, peer, "ping", "hijacked conn still carries traffic")
	}

	// The dispatch goroutine exits and hands cs back via the detachQueue
	// (runAsyncHandler's ErrHijacked path); the loop drains that and the
	// notice hijackConn enqueued, and settles the hijack once.
	cs.asyncInMu.Lock()
	cs.asyncRun = false
	cs.asyncInMu.Unlock()
	l.detachQMu.Lock()
	l.detachQueue = append(l.detachQueue, cs)
	l.detachQPending.Store(1)
	l.detachQMu.Unlock()
	l.drainDetachQueue()
	if !cs.hijackSettled || cs.liveIdx != -1 {
		t.Errorf("drainDetachQueue did not settle the hijack (settled=%v liveIdx=%d)", cs.hijackSettled, cs.liveIdx)
	}
	if got := l.connCount; got != 0 {
		t.Errorf("connCount = %d, want 0: a negative count never satisfies the DRAINING->SUSPENDED gate", got)
	}
}

// TestCloseConnLeavesAReissuedSlotAloneAfterWaitingOnDetachMu is the arm that
// actually exercises the new ownership branch. The hijack frees the slot and
// a DIFFERENT connState is installed on the same fd number before closeConn
// wakes, so the re-check lands on `l.conns[fd] != cs` rather than on the
// pre-existing `cs == nil` guard at the top of closeConn.
//
// Today only the parked loop thread fills this loop's conn table, so the
// reachable production shape is the nil slot — this arm is defence in depth
// for the `== cs` half of the predicate, and it is the only arm that proves
// the early return does not tear down whoever owns the slot now. Without the
// fix, closeConn nils the new owner's slot, closes its descriptor and fires
// its OnDisconnect.
func TestCloseConnLeavesAReissuedSlotAloneAfterWaitingOnDetachMu(t *testing.T) {
	rig := hijackRaceConn(t)
	l, cs, local := rig.l, rig.cs, rig.local
	parkDispatch(cs)

	cs.detachMu.Lock()
	done := hijackRaceSweep(l)
	hijackRaceWaitFor(t, cs.asyncClosed.Load, "checkTimeouts -> closeConn to reach its detachMu wait")

	nc, err := l.hijackConn(local)
	if err != nil {
		t.Fatalf("hijackConn: %v", err)
	}
	t.Cleanup(func() { _ = nc.Close() })

	// The number is reissued, and the slot is refilled with the new owner's
	// connState — what an adopt or a future off-thread registration would do.
	pipeWrite := hijackRaceRecycle(t, local)
	newCS := &connState{fd: local, liveIdx: -1}
	newCS.h1State = conn.NewH1State()
	newCS.lastActivity = time.Now().UnixNano()
	l.driverMu.Lock()
	l.conns[local] = newCS
	l.driverMu.Unlock()
	l.addLiveConn(newCS)
	l.connCount++
	l.activeConns.Add(1)

	cs.detachMu.Unlock()
	hijackRaceAwait(t, done)

	if l.conns[local] != newCS {
		t.Errorf("l.conns[%d] = %p, want the new owner %p: closeConn cleared a slot it no longer owned",
			local, l.conns[local], newCS)
	}
	if got := l.closeCount.Load(); got != 1 {
		t.Errorf("closeCount = %d, want 1 (only the hijack)", got)
	}
	if got := l.activeConns.Load(); got != 1 {
		t.Errorf("activeConns = %d, want 1 (the new owner)", got)
	}
	// The hijacked conn's live-set entry and count are the loop's until its
	// dispatch goroutine hands it back (celeris#668); then only the new
	// owner is left, found by its own entry.
	cs.asyncInMu.Lock()
	cs.asyncRun = false
	cs.asyncInMu.Unlock()
	l.detachQMu.Lock()
	l.detachQueue = append(l.detachQueue, cs)
	l.detachQPending.Store(1)
	l.detachQMu.Unlock()
	l.drainDetachQueue()
	if newCS.liveIdx != 0 || len(l.liveConns) != 1 || l.liveConns[0] != newCS {
		t.Errorf("new owner liveIdx = %d, len(liveConns) = %d, want it alone in the live set",
			newCS.liveIdx, len(l.liveConns))
	}
	if got := l.connCount; got != 1 {
		t.Errorf("connCount = %d, want 1 (the new owner)", got)
	}
	if got := rig.disconnects.Load(); got != 0 {
		t.Errorf("OnDisconnect fired %d times, want 0: neither conn disconnected", got)
	}
	if _, err := unix.FcntlInt(uintptr(local), unix.F_GETFD, 0); err != nil {
		t.Errorf("fd %d was closed although the slot held another conn: %v", local, err)
	} else {
		if _, err := unix.Write(pipeWrite, []byte("z")); err != nil {
			t.Errorf("write to the recycled pipe: %v", err)
		} else {
			hijackRaceExpectRead(t, local, "z", "the new owner's descriptor survives")
		}
	}
	if cs.detachClosed {
		t.Error("closeConn marked the hijacked conn detachClosed")
	}
}

// TestCloseConnAfterHijackIsNoOpWithoutTheRace is negative control 1: the
// same conn, the same hijack, the same fd recycling — but no interleaving.
//
// It does NOT exercise the fix: with the hijack complete, l.conns[fd] is nil
// and closeConn returns at the pre-existing `cs == nil` guard, above the new
// ownership re-check. Its job is to show that the witnesses this file relies
// on — the three counters, OnDisconnect, and the recycled-descriptor probe —
// read correctly when nothing goes wrong, on main as well as on the fix. The
// arm that reaches the new branch is the reissued-slot test above.
func TestCloseConnAfterHijackIsNoOpWithoutTheRace(t *testing.T) {
	rig := hijackRaceConn(t)
	l, cs, local := rig.l, rig.cs, rig.local

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
	if cs.detachClosed {
		t.Error("closeConn touched a connState it no longer owns")
	}
	// The dispatch goroutine exits and hands cs back; only then is the
	// loop's connCount decremented (celeris#668).
	cs.asyncInMu.Lock()
	cs.asyncRun = false
	cs.asyncInMu.Unlock()
	l.detachQMu.Lock()
	l.detachQueue = append(l.detachQueue, cs)
	l.detachQPending.Store(1)
	l.detachQMu.Unlock()
	l.drainDetachQueue()
	if got := l.connCount; got != 0 {
		t.Errorf("connCount = %d, want 0", got)
	}
	if got := rig.disconnects.Load(); got != 0 {
		t.Errorf("OnDisconnect fired %d times for a hijacked conn, want 0", got)
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
	if !cs.hijackSettled {
		t.Error("the hand-back did not settle the hijack")
	}
}

// TestCloseConnClosesOnceWhenHandlerDoesNotHijack is negative control 2: the
// identical interleaving — the timeout sweep waiting on detachMu behind a
// holder (with the dispatch goroutine parked, see parkDispatch) — with no
// Hijack. closeConn still owns the conn when
// it wakes, so it must close it exactly once AND still deliver OnDisconnect.
// It passes before and after the fix, showing the harness does not by itself
// produce a double count, and that the ownership re-check suppresses neither
// the close nor the public callback for a conn the engine really owns.
func TestCloseConnClosesOnceWhenHandlerDoesNotHijack(t *testing.T) {
	rig := hijackRaceConn(t)
	l, cs, local := rig.l, rig.cs, rig.local
	parkDispatch(cs)

	cs.detachMu.Lock()
	done := hijackRaceSweep(l)
	hijackRaceWaitFor(t, cs.asyncClosed.Load, "checkTimeouts -> closeConn to reach its detachMu wait")

	// The handler returns without hijacking anything.
	cs.detachMu.Unlock()
	hijackRaceAwait(t, done)

	if got := l.closeCount.Load(); got != 1 {
		t.Errorf("closeCount = %d, want 1", got)
	}
	if got := l.activeConns.Load(); got != 0 {
		t.Errorf("activeConns = %d, want 0", got)
	}
	if got := l.connCount; got != 0 {
		t.Errorf("connCount = %d, want 0", got)
	}
	if got := rig.disconnects.Load(); got != 1 {
		t.Errorf("OnDisconnect fired %d times, want 1: the engine owned this conn and closed it", got)
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

// TestHijackReleaseUnlinksTheConnFromTheDirtyList covers the other half of
// the same defect, on the COMMON hijack paths rather than the raced one.
//
// releaseConnState clears cs.dirtyNext/dirtyPrev and cs.dirty but never
// repairs l.dirtyHead or a predecessor's dirtyNext, and hijackConn does not
// unlink either. So both ordinary hijack releases — the synchronous one in
// hijackConn and the deferred one in drainDetachQueue's hijacked branch —
// used to hand a still-linked connState back to connStatePool, leaving
// l.dirtyHead pointing at pooled, reissued memory that the loop's dirty pass
// would then flushWrites against.
func TestHijackReleaseUnlinksTheConnFromTheDirtyList(t *testing.T) {
	t.Run("deferred_release_via_drainDetachQueue", func(t *testing.T) {
		rig := hijackRaceConn(t)
		l, cs, local := rig.l, rig.cs, rig.local

		// A partial write from a prior pipelined response left the conn on
		// the dirty list.
		l.markDirty(cs)
		if l.dirtyHead != cs {
			t.Fatalf("setup: dirtyHead = %p, want %p", l.dirtyHead, cs)
		}

		nc, err := l.hijackConn(local)
		if err != nil {
			t.Fatalf("hijackConn: %v", err)
		}
		t.Cleanup(func() { _ = nc.Close() })
		if !cs.hijacked.Load() {
			t.Fatalf("setup: async hijack did not defer the release")
		}

		cs.asyncInMu.Lock()
		cs.asyncRun = false
		cs.asyncInMu.Unlock()
		l.detachQMu.Lock()
		l.detachQueue = append(l.detachQueue, cs)
		l.detachQPending.Store(1)
		l.detachQMu.Unlock()
		l.drainDetachQueue()

		if l.dirtyHead != nil {
			t.Errorf("dirtyHead = %p after the hijacked conn was settled, want nil: "+
				"the loop's dirty pass would flush its bytes to a reissued fd", l.dirtyHead)
		}
	})

	t.Run("synchronous_release_in_hijackConn", func(t *testing.T) {
		rig := hijackRaceConn(t)
		l, cs, local := rig.l, rig.cs, rig.local

		// Sync mode (or an async conn not yet promoted): no dispatch
		// goroutine, so hijackConn releases the connState inline.
		cs.detachMu = nil
		cs.asyncRun = false

		l.markDirty(cs)
		if l.dirtyHead != cs {
			t.Fatalf("setup: dirtyHead = %p, want %p", l.dirtyHead, cs)
		}

		nc, err := l.hijackConn(local)
		if err != nil {
			t.Fatalf("hijackConn: %v", err)
		}
		t.Cleanup(func() { _ = nc.Close() })
		if cs.hijacked.Load() {
			t.Fatalf("setup: sync hijack deferred the release instead of doing it inline")
		}

		if l.dirtyHead != nil {
			t.Errorf("dirtyHead = %p after the inline release, want nil", l.dirtyHead)
		}
	})
}

// TestCloseLeftToAHandlerThatHijacksIsNotRedone is the shape the celeris#654
// race takes since celeris#669. The timeout reap finds the handler running and
// leaves the close to its dispatch goroutine instead of waiting; the handler
// then hijacks. The goroutine's exit (runAsyncHandler's ErrHijacked path)
// hands the conn back, and drainDetachQueue must take the hijack's branch —
// pool release, live set, connCount — and not run the owed close: no second
// close count, no OnDisconnect, and the reissued descriptor survives.
func TestCloseLeftToAHandlerThatHijacksIsNotRedone(t *testing.T) {
	rig := hijackRaceConn(t)
	l, cs, local := rig.l, rig.cs, rig.local
	release := holdAsHandler(t, cs, true)

	if !returnsWhileHeld(t, release, l.checkTimeouts) {
		t.Fatal("the reap waited on a running handler (celeris#669)")
	}
	cs.asyncInMu.Lock()
	owed := cs.closeOwed
	cs.asyncInMu.Unlock()
	if !owed {
		t.Fatal("setup: the reap did not leave the close to the dispatch goroutine")
	}

	nc, err := l.hijackConn(local)
	if err != nil {
		t.Fatalf("hijackConn: %v", err)
	}
	t.Cleanup(func() { _ = nc.Close() })
	pipeWrite := hijackRaceRecycle(t, local)
	release()

	// runAsyncHandler's ErrHijacked exit: asyncClosed, the goroutine gone,
	// cs enqueued — the enqueue is the hand-back.
	cs.asyncClosed.Store(true)
	cs.asyncInMu.Lock()
	cs.endDispatch()
	cs.asyncInMu.Unlock()
	l.enqueueDetach(cs)
	l.drainDetachQueue()

	if got := l.closeCount.Load(); got != 1 {
		t.Errorf("closeCount = %d, want 1 (the hijack only)", got)
	}
	if got := l.activeConns.Load(); got != 0 {
		t.Errorf("activeConns = %d, want 0", got)
	}
	if got := l.connCount; got != 0 {
		t.Errorf("connCount = %d, want 0", got)
	}
	if got := rig.disconnects.Load(); got != 0 {
		t.Errorf("OnDisconnect fired %d times for a hijacked conn, want 0", got)
	}
	if len(l.liveConns) != 0 {
		t.Errorf("len(liveConns) = %d after the hand-back, want 0", len(l.liveConns))
	}
	if !cs.hijackSettled {
		t.Error("the hand-back did not settle the hijack")
	}
	if !fdOpen(local) {
		t.Fatalf("fd %d, reissued after the hijack, was closed by the owed close", local)
	}
	if _, err := unix.Write(pipeWrite, []byte("w")); err != nil {
		t.Errorf("write to the recycled pipe: %v", err)
	} else {
		hijackRaceExpectRead(t, local, "w", "the reissued descriptor survives")
	}
}
