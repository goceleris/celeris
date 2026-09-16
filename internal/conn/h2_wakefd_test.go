//go:build linux

package conn

import (
	"context"
	"encoding/binary"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/wakefd"
	"github.com/goceleris/celeris/protocol/h2/stream"
)

// celeris#655, the H2 write queue. Both engines hand this queue the wakeup
// eventfd they will close at shutdown (each engine's NewH2State call sites),
// and the queue writes it from whichever goroutine enqueued a frame.
// Non-inline H2 streams run on globalH2Pool (see protocol/h2/stream's
// processor), which no engine joins, and CloseH2 → Manager.Close only
// cancels the streams — so a frame can be enqueued after the descriptor is
// gone, and the 8 bytes land on whatever reuses it.

type nopHandler655 struct{}

func (nopHandler655) HandleStream(context.Context, *stream.Stream) error { return nil }

// wiredQueue655 is the write queue as an engine wires it — the queue plus the
// close the engine performs at shutdown. This constructor is the ONLY place
// the two revisions differ: main hands NewH2State a raw descriptor, the fix
// hands it a *wakefd.WakeFD whose Close is the engine's close. Every
// assertion below is identical on both.
type wiredQueue655 struct {
	q         *h2ShardedQueue
	closeWake func()
}

func newWiredQueue655(t *testing.T, efd int) wiredQueue655 {
	t.Helper()
	wake := wakefd.New(efd)
	s := NewH2State(nopHandler655{}, H2Config{}, func([]byte) {}, wake)
	return wiredQueue655{q: &s.writeQueue, closeWake: wake.Close}
}

// drainWake655 reads the eventfd counter, returning 0 when nothing signalled
// it. The POSITIVE CONTROL: it proves Enqueue really does reach the wakeup
// write, so a clean result after the close cannot mean the producer is inert.
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
// onto that number afterwards by claim.
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
// number. F_DUPFD_CLOEXEC returns the LOWEST FREE number >= n, so anything
// other than n means n was still open and the test stops rather than
// reporting a pass it did not earn.
func (w *fdWatcher655) claim(t *testing.T, n int) {
	t.Helper()
	if _, err := unix.FcntlInt(uintptr(n), unix.F_GETFD, 0); err != unix.EBADF {
		t.Fatalf("precondition: fd %d is not closed (F_GETFD err=%v)", n, err)
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

// TestH2WriteQueueDoesNotSignalAClosedWakeupFD pins the queue's own barrier.
func TestH2WriteQueueDoesNotSignalAClosedWakeupFD(t *testing.T) {
	efd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
	if err != nil {
		t.Fatalf("eventfd: %v", err)
	}
	w := newWiredQueue655(t, efd)
	watch := newFDWatcher655(t)

	w.q.Enqueue(1, getH2FrameBuf())
	if got := drainWake655(t, efd); got != 1 {
		t.Fatalf("eventfd counter = %d after Enqueue on a LIVE queue, want 1: the "+
			"producer under test never reaches the wakeup write, so the assertion "+
			"below would pass for the wrong reason", got)
	}
	// The event loop drains, which clears the CAS coalescing flag, so the
	// enqueue after the close is again an edge that signals.
	w.q.DrainTo(func([]byte) {})
	if w.q.pending.Load() {
		t.Fatal("precondition: the queue still reports pending after DrainTo — the " +
			"enqueue below would coalesce and skip the write this test is about")
	}

	// The engine shuts down and closes the descriptor it handed the queue.
	w.closeWake()
	watch.claim(t, efd)

	w.q.Enqueue(1, getH2FrameBuf())

	var b [16]byte
	k, rerr := unix.Read(watch.rd, b[:])
	switch {
	case k > 0:
		t.Errorf("h2ShardedQueue.Enqueue wrote % x into descriptor %d after the engine "+
			"closed its wakeup eventfd; whatever now holds that number receives those "+
			"bytes (celeris#655)", b[:k], efd)
	case rerr != unix.EAGAIN:
		t.Fatalf("read the descriptor occupying %d: n=%d err=%v", efd, k, rerr)
	}
}
