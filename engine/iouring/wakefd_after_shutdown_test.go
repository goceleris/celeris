//go:build linux

package iouring

import (
	"context"
	"encoding/binary"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/internal/wakefd"
)

// celeris#655. Both engines let producers on OTHER goroutines write the
// loop's wakeup eventfd, and shutdown closes that eventfd without resetting
// the field. Every producer that runs after the close writes 8 bytes
// (01 00 00 00 00 00 00 00) into whatever descriptor now holds that number:
// a spurious wakeup if it is another eventfd, bytes injected into a stream
// if it is a client socket, silent corruption if it is a file. Nothing
// reports any of it.
//
// celeris#658 (PR #661) put the two ADOPT writers under the queue mutex that
// shutdown takes before the close. These tests cover the io_uring producers
// it left behind — addDriverAction (driver.go), enqueueDetach
// (transplant_source.go) and the detached WS/SSE ResumeRecv closure
// (worker.go) — all of which write after releasing their queue mutex, none
// of which consults a closed flag.
//
// Worker.shutdown closes the eventfd at worker.go:5213 but joins the
// dispatch goroutines at 5225, AFTER it: on this engine a producer is not
// even guaranteed to have stopped when the descriptor goes away.

// newWakeWorker655 is a Worker reduced to what shutdown needs: a wakeup
// eventfd it will close, and listenFD = -1 so it does not close fd 0. Empty
// queues, no live conns, nil ring, empty asyncWG — shutdown runs end to end.
//
// This helper is the ONLY place the two revisions differ: on main the raw
// descriptor lives in Worker.h2EventFD, on the fix it is owned by a
// *wakefd.WakeFD. Every assertion below is identical on both.
func newWakeWorker655(t *testing.T) (*Worker, int) {
	t.Helper()
	efd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
	if err != nil {
		t.Fatalf("eventfd: %v", err)
	}
	return &Worker{listenFD: -1, wakeFD: wakefd.New(efd)}, efd
}

// socketFor655 returns one end of a socketpair. Both ends are closed by
// cleanup; nothing under test closes a driver-registered fd.
func socketFor655(t *testing.T) int {
	t.Helper()
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(pair[0]); _ = unix.Close(pair[1]) })
	return pair[0]
}

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
		t.Errorf("%s wrote % x into descriptor %d after shutdown closed the worker's "+
			"wakeup eventfd; whatever now holds that number receives those bytes "+
			"(celeris#655)", who, b[:k], n)
	case err != unix.EAGAIN:
		t.Fatalf("read the descriptor occupying %d: n=%d err=%v", n, k, err)
	}
}

// TestRegisterConnAfterShutdownDoesNotWriteTheClosedWakeupFD covers
// addDriverAction, the deterministic case. shutdownDrivers sets
// driverConns=nil, and RegisterConn rebuilds the map from nil
// (driver.go:156) and always reaches the wakeup write — so a driver that
// registers an fd on a worker that has already gone away writes into a
// recycled descriptor every single time.
func TestRegisterConnAfterShutdownDoesNotWriteTheClosedWakeupFD(t *testing.T) {
	w, efd := newWakeWorker655(t)
	watch := newFDWatcher655(t)

	if err := w.RegisterConn(socketFor655(t), func([]byte) {}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn on a live worker: %v", err)
	}
	if got := drainWake655(t, efd); got != 1 {
		t.Fatalf("eventfd counter = %d after RegisterConn on a LIVE worker, want 1: "+
			"the producer under test never reaches the wakeup write, so the "+
			"post-shutdown assertion below would pass for the wrong reason", got)
	}

	w.shutdown()
	watch.claim(t, efd)

	_ = w.RegisterConn(socketFor655(t), func([]byte) {}, func(error) {})
	watch.assertNoLateWake(t, efd, "RegisterConn")
}

// TestEnqueueDetachAfterShutdownDoesNotWriteTheClosedWakeupFD covers
// enqueueDetach, which the dispatch goroutine calls (worker.go:3976) while an
// io_uring→epoll transplant drains — exactly the switch-vs-shutdown window
// celeris#655 describes. Those goroutines are joined only AFTER the eventfd
// is closed.
func TestEnqueueDetachAfterShutdownDoesNotWriteTheClosedWakeupFD(t *testing.T) {
	w, efd := newWakeWorker655(t)
	watch := newFDWatcher655(t)

	w.enqueueDetach(&connState{fd: -1, liveIdx: -1})
	if got := drainWake655(t, efd); got != 1 {
		t.Fatalf("eventfd counter = %d after enqueueDetach on a LIVE worker, want 1: "+
			"the producer under test never reaches the wakeup write", got)
	}

	w.shutdown()
	watch.claim(t, efd)

	w.enqueueDetach(&connState{fd: -1, liveIdx: -1})
	watch.assertNoLateWake(t, efd, "enqueueDetach")
}

// TestDetachedResumeRecvAfterShutdownDoesNotWriteTheClosedWakeupFD covers the
// WS/SSE backpressure callbacks installed by OnDetach. They run on middleware
// goroutines that asyncWG does not track, they capture the eventfd NUMBER by
// value (worker.go:2086), and unlike the guarded write closure they have no
// detachClosed check at all.
//
// The live path: shutdown fires OnDetachClose, the websocket chanReader
// delivers the chunks it still holds before it reports that close
// (middleware/websocket/engineread.go:263-320), and on the way down to
// lowWater it calls resume().
func TestDetachedResumeRecvAfterShutdownDoesNotWriteTheClosedWakeupFD(t *testing.T) {
	w, efd := newWakeWorker655(t)
	local := socketFor655(t)
	watch := newFDWatcher655(t)
	w.conns = make([]*connState, local+1)
	w.liveConns = make([]int, 0, 4)
	// The eventfd poll is already armed, so OnDetach does not try to submit
	// an SQE on this ringless Worker.
	w.h2PollArmed = true

	cs := &connState{fd: local, liveIdx: -1, ctx: context.Background(), buf: make([]byte, 4096)}
	cs.protocol.Store(int32(engine.HTTP1))
	cs.detected = true
	w.conns[local] = cs
	w.addLiveConn(cs)
	w.initProtocol(cs)
	cs.h1State.OnDetach()

	// Positive control on the same pair of callbacks: a pause on a LIVE
	// worker must reach the wakeup write.
	cs.h1State.PauseRecv()
	if got := drainWake655(t, efd); got != 1 {
		t.Fatalf("eventfd counter = %d after PauseRecv on a LIVE worker, want 1: "+
			"the detached backpressure callbacks never reach the wakeup write", got)
	}
	// The worker drains what the pause queued, so the resume below is again
	// the empty->non-empty edge that writes the eventfd.
	w.detachQMu.Lock()
	w.detachQueue = w.detachQueue[:0]
	w.detachQPending.Store(0)
	w.detachQMu.Unlock()

	w.shutdown()
	if got := w.detachQPending.Load(); got != 0 {
		t.Fatalf("precondition: detachQPending = %d, want 0 — ResumeRecv would coalesce "+
			"onto an earlier enqueue and skip the write this test is about", got)
	}
	watch.claim(t, efd)

	cs.h1State.ResumeRecv()
	watch.assertNoLateWake(t, efd, "ResumeRecv on a detached conn")
}
