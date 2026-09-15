//go:build linux

package epoll

import (
	"context"
	"encoding/binary"
	"sync/atomic"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/internal/wakefd"
	"github.com/goceleris/celeris/resource"
)

// celeris#655, the epoll half. Loop.shutdown closes the wakeup eventfd
// (loop.go:2887) and leaves the number in l.eventFD, so any producer that
// runs afterwards writes 8 bytes into whatever descriptor now holds it.
//
// The dispatch goroutines are joined before that close (loop.go:2862), so
// runAsyncHandler and drainDetachQueue are NOT the hazard here. What is left
// are the producers asyncWG never tracked: the detached WS/SSE callbacks
// installed by OnDetach, which run on middleware goroutines, and the H2
// write queue, which the engine hands the same descriptor number
// (loop.go:1801, 1829) and which is drained by pool goroutines the engine
// never joins.
//
// PauseRecv and ResumeRecv (loop.go:1672-1703) are the sharpest of them:
// unlike the guarded write closure they carry no detachClosed check at all.

// drainWake655 reads the eventfd counter, returning 0 when nothing signalled
// it. Used as the POSITIVE CONTROL: it proves the producer under test really
// does reach the wakeup write, so a clean result after shutdown cannot mean
// the producer is simply inert (a -1 descriptor, or a coalescing flag left
// set).
func drainWake655(t *testing.T, efd int) uint64 {
	t.Helper()
	var b [8]byte
	n, err := unix.Read(efd, b[:])
	if err == unix.EAGAIN {
		return 0
	}
	if err != nil || n != 8 {
		t.Fatalf("read eventfd %d: n=%d err=%v", efd, n, err)
	}
	return binary.NativeEndian.Uint64(b[:])
}

// fdWatcher655 stands in for whatever the process opens next and inherits the
// eventfd's descriptor number. Its pipe is created BEFORE the eventfd is
// closed — otherwise pipe2, which allocates the lowest free numbers, would
// take the very number the test wants to watch — and its write end is moved
// onto that number afterwards by claim. Anything a late producer writes to
// the number then arrives on rd.
type fdWatcher655 struct{ rd, wr int }

func newFDWatcher655(t *testing.T) *fdWatcher655 {
	t.Helper()
	var p [2]int
	if err := unix.Pipe2(p[:], unix.O_NONBLOCK|unix.O_CLOEXEC); err != nil {
		t.Fatalf("pipe2: %v", err)
	}
	w := &fdWatcher655{rd: p[0], wr: p[1]}
	t.Cleanup(func() { _ = unix.Close(w.rd); _ = unix.Close(w.wr) })
	return w
}

// claim asserts n is closed and parks the watcher's write end on exactly that
// number. F_DUPFD_CLOEXEC cannot clobber a live descriptor: it returns the
// LOWEST FREE number >= n, so anything other than n means the precondition
// failed and the test stops rather than reporting a pass it did not earn.
func (w *fdWatcher655) claim(t *testing.T, n int) {
	t.Helper()
	if _, err := unix.FcntlInt(uintptr(n), unix.F_GETFD, 0); err != unix.EBADF {
		t.Fatalf("precondition: fd %d is not closed after shutdown (F_GETFD err=%v)", n, err)
	}
	got, err := unix.FcntlInt(uintptr(w.wr), unix.F_DUPFD_CLOEXEC, n)
	if err != nil || got != n {
		if err == nil {
			_ = unix.Close(got)
		}
		t.Fatalf("could not park a descriptor on %d (got %d, err=%v)", n, got, err)
	}
	t.Cleanup(func() { _ = unix.Close(n) })
}

// assertNoLateWake fails if anything reached the descriptor now occupying the
// closed eventfd's number.
func (w *fdWatcher655) assertNoLateWake(t *testing.T, n int, who string) {
	t.Helper()
	var b [16]byte
	k, err := unix.Read(w.rd, b[:])
	switch {
	case k > 0:
		t.Errorf("%s wrote % x into descriptor %d after shutdown closed the loop's "+
			"wakeup eventfd; whatever now holds that number receives those bytes "+
			"(celeris#655)", who, b[:k], n)
	case err != unix.EAGAIN:
		t.Fatalf("read the descriptor occupying %d: n=%d err=%v", n, k, err)
	}
}

// TestDetachedResumeRecvAfterShutdownDoesNotWriteTheClosedWakeupFD models a
// WebSocket connection paused by backpressure whose handler drains its buffer
// after the engine is gone: shutdown fires OnDetachClose, the chanReader
// delivers the chunks it still holds before it reports that close
// (middleware/websocket/engineread.go:263-320), and on the way down to
// lowWater it calls resume().
//
// The Loop is built as a literal in the shape review_v150_test.go uses, with
// a real epollfd and a real socket, so shutdown runs its three phases
// unmodified. The eventFD field is the ONLY line that differs between main
// and the fix, where the descriptor is owned by a *wakefd.WakeFD.
func TestDetachedResumeRecvAfterShutdownDoesNotWriteTheClosedWakeupFD(t *testing.T) {
	efd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
	if err != nil {
		t.Fatalf("eventfd: %v", err)
	}
	epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		t.Fatalf("epoll_create1: %v", err)
	}
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	local, peer := pair[0], pair[1]
	t.Cleanup(func() { _ = unix.Close(peer) })
	if local >= connTableSize {
		t.Fatalf("fd %d is outside the conn table", local)
	}
	watch := newFDWatcher655(t)
	// shutdown owns epfd and local (phase 3 closes both); close them here
	// only if the test stops before it runs.
	shutdownRan := false
	t.Cleanup(func() {
		if !shutdownRan {
			_ = unix.Close(local)
			_ = unix.Close(epfd)
		}
	})

	l := &Loop{
		epollFD:      epfd,
		listenFD:     -1,
		timerFD:      -1,
		wakeFD:       wakefd.New(efd),
		conns:        make([]*connState, connTableSize),
		liveConns:    make([]int, 0, 4),
		activeConns:  &atomic.Int64{},
		closeCount:   &atomic.Uint64{},
		acceptCount:  &atomic.Uint64{},
		bytesRead:    &atomic.Uint64{},
		bytesWritten: &atomic.Uint64{},
		handler:      okHandler658{},
		cfg:          resource.Config{},
	}

	cs := acquireConnState(context.Background(), local, 4096, false)
	cs.protocol = engine.HTTP1
	cs.detected = true
	cs.writeFn = l.makeWriteFn(cs)
	l.conns[local] = cs
	l.addLiveConn(cs)
	l.connCount++
	l.initProtocol(cs)
	cs.h1State.OnDetach()

	// Positive control on the same pair of callbacks: a pause on a LIVE loop
	// must reach the wakeup write.
	cs.h1State.PauseRecv()
	if got := drainWake655(t, efd); got != 1 {
		t.Fatalf("eventfd counter = %d after PauseRecv on a LIVE loop, want 1: the "+
			"detached backpressure callbacks never reach the wakeup write, so the "+
			"post-shutdown assertion below would pass for the wrong reason", got)
	}
	// The loop drains what the pause queued, so the resume below is again the
	// empty->non-empty edge that writes the eventfd.
	l.detachQMu.Lock()
	l.detachQueue = l.detachQueue[:0]
	l.detachQPending.Store(0)
	l.detachQMu.Unlock()

	l.shutdown()
	shutdownRan = true
	if got := l.detachQPending.Load(); got != 0 {
		t.Fatalf("precondition: detachQPending = %d, want 0 — ResumeRecv would coalesce "+
			"onto an earlier enqueue and skip the write this test is about", got)
	}
	watch.claim(t, efd)

	cs.h1State.ResumeRecv()
	watch.assertNoLateWake(t, efd, "ResumeRecv on a detached conn")
}
