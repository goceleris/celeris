//go:build linux

package epoll

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/resource"
)

// regLive registers a connState at fd in l.conns and adds it to liveConns,
// mirroring the bookkeeping acceptAll performs.
func regLive(l *Loop, fd int) *connState {
	cs := &connState{fd: fd, liveIdx: -1}
	l.conns[fd] = cs
	l.addLiveConn(cs)
	return cs
}

// TestLiveConnsAddRemoveO1 verifies the O(1) index-based add/remove keeps the
// dense liveConns slice and every conn's liveIdx self-consistent across the
// swap-with-last path (#3.6). This is the epoll port of the iouring
// live_conns test, but using the index-based removeLiveConn (NOT the O(N)
// scan).
func TestLiveConnsAddRemoveO1(t *testing.T) {
	l := &Loop{conns: make([]*connState, 1024), liveConns: make([]int, 0, 16)}

	cs5 := regLive(l, 5)
	cs10 := regLive(l, 10)
	cs15 := regLive(l, 15)
	if got := len(l.liveConns); got != 3 {
		t.Fatalf("after 3 adds: len(liveConns) = %d, want 3", got)
	}
	if cs5.liveIdx != 0 || cs10.liveIdx != 1 || cs15.liveIdx != 2 {
		t.Fatalf("liveIdx stamps wrong: %d %d %d, want 0 1 2", cs5.liveIdx, cs10.liveIdx, cs15.liveIdx)
	}

	// Remove the middle — swap-with-last moves 15 into slot 1: [5, 15].
	l.removeLiveConn(cs10)
	if l.liveConns[0] != 5 || l.liveConns[1] != 15 || len(l.liveConns) != 2 {
		t.Fatalf("liveConns = %v, want [5 15]", l.liveConns)
	}
	if cs15.liveIdx != 1 {
		t.Errorf("swapped-in cs15.liveIdx = %d, want 1", cs15.liveIdx)
	}
	if cs10.liveIdx != -1 {
		t.Errorf("removed cs10.liveIdx = %d, want -1", cs10.liveIdx)
	}

	l.removeLiveConn(cs15)
	l.removeLiveConn(cs5)
	if got := len(l.liveConns); got != 0 {
		t.Errorf("after all removes: len = %d, want 0", got)
	}

	// Double-remove (stale liveIdx) and nil are no-ops.
	l.removeLiveConn(cs5)
	l.removeLiveConn(nil)
}

// TestCheckTimeoutsClosesAllExpired is the #3.6 regression: register conns at
// fds {0,5,10,15}, expire every deadline, and assert checkTimeouts closes ALL
// four. The pre-fix forward `for fd:=0; fd<=maxFD` scan still worked here, but
// the new reverse-index liveConns iteration must also reach every conn even
// though closeConn swap-removes from liveConns mid-scan (a forward range over
// liveConns would skip the swapped-in tail entries).
func TestCheckTimeoutsClosesAllExpired(t *testing.T) {
	epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		t.Skipf("epoll_create1 unavailable: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(epfd) })

	l := &Loop{
		epollFD:      epfd,
		conns:        make([]*connState, 1024),
		liveConns:    make([]int, 0, 8),
		activeConns:  &atomic.Int64{},
		closeCount:   &atomic.Uint64{},
		acceptCount:  &atomic.Uint64{},
		bytesRead:    &atomic.Uint64{},
		bytesWritten: &atomic.Uint64{},
		cfg:          resource.Config{IdleTimeout: time.Second},
	}
	past := time.Now().Add(-time.Hour).UnixNano()

	var peers []int
	t.Cleanup(func() {
		for _, p := range peers {
			_ = unix.Close(p)
		}
	})

	// One socketpair per conn so closeConn's Shutdown/Close are harmless.
	// We register at the natural local fds (clobbering fd 0/stdin in a test
	// binary is unsafe); the regression is about every expired conn being
	// reached by the reverse-index liveConns scan + swap-remove mid-loop,
	// which is exercised regardless of the exact fd numbers.
	want := map[int]*connState{}
	for i := 0; i < 4; i++ {
		pair, perr := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
		if perr != nil {
			t.Skipf("socketpair unavailable: %v", perr)
		}
		local, peer := pair[0], pair[1]
		peers = append(peers, peer)
		fd := local
		if fd >= len(l.conns) {
			_ = unix.Close(local)
			continue
		}
		cs := &connState{fd: fd, liveIdx: -1, lastActivity: past}
		// Give it an H1State so closeConn takes the plain-close path and
		// CloseH1 runs (exercises the real teardown).
		cs.h1State = conn.NewH1State()
		l.conns[fd] = cs
		l.addLiveConn(cs)
		l.connCount++
		l.activeConns.Add(1)
		if fd > l.maxFD {
			l.maxFD = fd
		}
		want[fd] = cs
	}
	if len(want) == 0 {
		t.Skip("no fds registered")
	}

	l.checkTimeouts()

	if got := len(l.liveConns); got != 0 {
		t.Fatalf("checkTimeouts left %d live conns, want 0 (reverse-scan swap-remove regression)", got)
	}
	for fd := range want {
		if l.conns[fd] != nil {
			t.Errorf("conn at fd %d not closed (slot still non-nil)", fd)
		}
	}
	if l.connCount != 0 {
		t.Errorf("connCount = %d, want 0", l.connCount)
	}
}

// TestAcceptAllDrainsBacklogNoStrand is the #3.3 regression: 200 simultaneous
// connections against a single loop must ALL be accepted in one acceptAll
// pass with none stranded (the pre-fix code capped at 64 per call and
// returned on any non-EAGAIN error, stranding the backlog until the next SYN
// edge on an edge-triggered listen socket).
func TestAcceptAllDrainsBacklogNoStrand(t *testing.T) {
	epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		t.Skipf("epoll_create1 unavailable: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(epfd) })

	lfd, err := createListenSocket("127.0.0.1:0")
	if err != nil {
		t.Skipf("listen socket unavailable: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(lfd) })
	la := boundAddr(lfd)
	if la == nil {
		t.Skip("could not resolve bound addr")
	}

	l := &Loop{
		epollFD:      epfd,
		listenFD:     lfd,
		conns:        make([]*connState, connTableSize),
		liveConns:    make([]int, 0, 256),
		activeConns:  &atomic.Int64{},
		errCount:     &atomic.Uint64{},
		reqCount:     &atomic.Uint64{},
		acceptCount:  &atomic.Uint64{},
		closeCount:   &atomic.Uint64{},
		bytesRead:    &atomic.Uint64{},
		bytesWritten: &atomic.Uint64{},
		eventFD:      -1,
		timerFD:      -1,
		resolved:     resource.ResolvedResources{BufferSize: 8192},
		cfg:          resource.Config{},
	}

	const want = 200
	var dialWG sync.WaitGroup
	clients := make([]int, 0, want)
	var clientsMu sync.Mutex
	for i := 0; i < want; i++ {
		dialWG.Add(1)
		go func() {
			defer dialWG.Done()
			cfd, derr := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
			if derr != nil {
				return
			}
			var sa unix.SockaddrInet4
			sa.Port = la.(*net.TCPAddr).Port
			copy(sa.Addr[:], la.(*net.TCPAddr).IP.To4())
			if cerr := unix.Connect(cfd, &sa); cerr != nil {
				_ = unix.Close(cfd)
				return
			}
			clientsMu.Lock()
			clients = append(clients, cfd)
			clientsMu.Unlock()
		}()
	}
	dialWG.Wait()
	t.Cleanup(func() {
		for _, c := range clients {
			_ = unix.Close(c)
		}
	})
	if len(clients) < want {
		t.Logf("only %d/%d clients connected (kernel backlog); proceeding", len(clients), want)
	}

	// Give the kernel a moment to enqueue all the SYNs into the accept queue.
	deadline := time.Now().Add(2 * time.Second)
	for len(l.liveConns) < len(clients) && time.Now().Before(deadline) {
		l.acceptAll(context.Background(), time.Now().UnixNano())
		if l.listenHot {
			continue
		}
		// Drained to EAGAIN this round but more clients may still be mid-
		// handshake; brief yield then retry until we've drained them all.
		time.Sleep(2 * time.Millisecond)
	}

	if got := len(l.liveConns); got < len(clients) {
		t.Fatalf("acceptAll stranded backlog: accepted %d, dialed %d", got, len(clients))
	}
	if l.connCount != len(l.liveConns) {
		t.Errorf("connCount=%d out of sync with liveConns=%d", l.connCount, len(l.liveConns))
	}
}

// TestHijackDefersReleaseWhileAsyncGoroutineActive is the #3.1 regression: a
// conn hijacked from inside an async dispatch goroutine MUST NOT have its
// connState recycled by hijackConn while that goroutine still references cs.
// We simulate the dispatch goroutine (asyncRun=true, detachMu non-nil),
// hijack the conn, and assert (a) the pool release was deferred (hijacked
// flag set, NOT recycled — fd still readable on cs) and (b) once we run the
// worker-thread drainDetachQueue handoff the cs is released exactly once.
func TestHijackDefersReleaseWhileAsyncGoroutineActive(t *testing.T) {
	epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		t.Skipf("epoll_create1 unavailable: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(epfd) })

	pair, perr := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if perr != nil {
		t.Skipf("socketpair unavailable: %v", perr)
	}
	local, peer := pair[0], pair[1]
	t.Cleanup(func() { _ = unix.Close(peer) })
	if local >= connTableSize {
		_ = unix.Close(local)
		t.Skip("fd out of table range")
	}

	l := &Loop{
		epollFD:      epfd,
		conns:        make([]*connState, connTableSize),
		liveConns:    make([]int, 0, 8),
		activeConns:  &atomic.Int64{},
		closeCount:   &atomic.Uint64{},
		acceptCount:  &atomic.Uint64{},
		bytesRead:    &atomic.Uint64{},
		bytesWritten: &atomic.Uint64{},
		eventFD:      -1,
		cfg:          resource.Config{},
	}

	// Async-mode conn: detachMu set, dispatch goroutine "alive" (asyncRun).
	cs := &connState{fd: local, liveIdx: -1}
	cs.detachMu = &sync.Mutex{}
	cs.asyncCond.L = &cs.asyncInMu
	cs.asyncRun = true
	l.conns[local] = cs
	l.addLiveConn(cs)
	l.connCount++
	l.activeConns.Add(1)

	// Hijack runs on the (simulated) dispatch goroutine path.
	c, herr := l.hijackConn(local)
	if herr != nil {
		t.Fatalf("hijackConn: %v", herr)
	}
	if c != nil {
		t.Cleanup(func() { _ = c.Close() })
	}

	// Detached synchronously: slot nil'd, removed from live set.
	if l.conns[local] != nil {
		t.Errorf("hijackConn did not nil l.conns[%d]", local)
	}
	if cs.liveIdx != -1 || len(l.liveConns) != 0 {
		t.Errorf("hijackConn did not remove from liveConns (liveIdx=%d, len=%d)", cs.liveIdx, len(l.liveConns))
	}
	// Pool release MUST be deferred while the async goroutine is alive.
	if !cs.hijacked {
		t.Fatal("hijackConn released cs synchronously while async goroutine active (UAR risk)")
	}
	// releaseConnState zeroes fd; deferred means fd is still intact.
	if cs.fd != local {
		t.Fatalf("cs was recycled despite active async goroutine: fd=%d", cs.fd)
	}

	// Now the dispatch goroutine "exits" and hands cs to the worker via the
	// detachQueue with hijacked still set. drainDetachQueue must release it.
	cs.asyncRun = false
	l.detachQMu.Lock()
	l.detachQueue = append(l.detachQueue, cs)
	l.detachQPending.Store(1)
	l.detachQMu.Unlock()

	l.drainDetachQueue()

	// releaseConnState clears hijacked + zeroes fd.
	if cs.hijacked {
		t.Error("drainDetachQueue did not release hijacked cs (hijacked still set)")
	}
	if cs.fd != 0 {
		t.Errorf("cs.fd = %d after release, want 0", cs.fd)
	}
}

// TestHijackSyncReleasesImmediately verifies the inline (non-async) hijack
// path releases the connState synchronously — there is no dispatch goroutine
// holding a reference, so deferring would leak the pooled state.
func TestHijackSyncReleasesImmediately(t *testing.T) {
	epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		t.Skipf("epoll_create1 unavailable: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(epfd) })

	pair, perr := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if perr != nil {
		t.Skipf("socketpair unavailable: %v", perr)
	}
	local, peer := pair[0], pair[1]
	t.Cleanup(func() { _ = unix.Close(peer) })
	if local >= connTableSize {
		_ = unix.Close(local)
		t.Skip("fd out of table range")
	}

	l := &Loop{
		epollFD:      epfd,
		conns:        make([]*connState, connTableSize),
		liveConns:    make([]int, 0, 8),
		activeConns:  &atomic.Int64{},
		closeCount:   &atomic.Uint64{},
		acceptCount:  &atomic.Uint64{},
		bytesRead:    &atomic.Uint64{},
		bytesWritten: &atomic.Uint64{},
		eventFD:      -1,
		cfg:          resource.Config{},
	}
	// Sync mode: detachMu nil, no dispatch goroutine.
	cs := &connState{fd: local, liveIdx: -1}
	l.conns[local] = cs
	l.addLiveConn(cs)
	l.connCount++
	l.activeConns.Add(1)

	c, herr := l.hijackConn(local)
	if herr != nil {
		t.Fatalf("hijackConn: %v", herr)
	}
	if c != nil {
		t.Cleanup(func() { _ = c.Close() })
	}
	if cs.hijacked {
		t.Error("sync hijack deferred release (hijacked set) — should release inline")
	}
	if cs.fd != 0 {
		t.Errorf("sync hijack did not release cs (fd=%d, want 0)", cs.fd)
	}
}
