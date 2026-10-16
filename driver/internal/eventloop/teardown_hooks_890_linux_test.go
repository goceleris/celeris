//go:build linux

package eventloop

// celeris#890: the order of the steps of a teardown, pinned with hooks that
// hold a step open instead of racing it.
//
//   - forget removes the conn from the map and issues the EPOLL_CTL_DEL under
//     one hold of w.mu, so an UnregisterConn whose lookup misses knows the DEL
//     is done before its caller closes the fd (mutant X1: the DEL after the
//     unlock).
//   - shutdown marks every conn closed before it closes the epoll fd, so an
//     EPOLL_CTL issued under a conn's c.mu after its closed check never
//     reaches a closed or reused epoll descriptor (mutant X2: the conns
//     marked after the close).
//   - shutdown closes the wake eventfd, and no wake touches the number after
//     that.
//   - RegisterConn's closed check and EPOLL_CTL_ADD are one critical section
//     under c.mu, so a Close or UnregisterConn waits for the ADD (mutant
//     CHECKOUTSIDE: the check under c.mu, the ADD after it is released).
//
// Several hooks run with a lock held (see their docs), so a test starts the
// competing call on its own goroutine, watches that it has not returned
// within a bounded window, and then releases the hook. A competing call that
// is correctly held never returns inside the window, so the window can only
// make a mutant look correct on a machine slow enough to starve the
// goroutine for 300 ms, never make correct code fail.

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
)

// c890Window is how long a test waits for a call that must stay blocked.
const c890Window = 300 * time.Millisecond

// c890Held reports whether ch received nothing within c890Window.
func c890Held[T any](ch <-chan T) (T, bool) {
	select {
	case v := <-ch:
		return v, false
	case <-time.After(c890Window):
		var zero T
		return zero, true
	}
}

// c890InEpollSet probes epfd with an EPOLL_CTL_ADD of fd of its own: EEXIST
// says fd is in the set. A probe that landed is undone.
func c890InEpollSet(t *testing.T, epfd, fd int) bool {
	t.Helper()
	ev := unix.EpollEvent{Events: unix.EPOLLIN, Fd: int32(fd)}
	err := unix.EpollCtl(epfd, unix.EPOLL_CTL_ADD, fd, &ev)
	if err == nil {
		_ = unix.EpollCtl(epfd, unix.EPOLL_CTL_DEL, fd, nil)
		return false
	}
	if !errors.Is(err, unix.EEXIST) {
		t.Fatalf("probe EPOLL_CTL_ADD of %d: %v", fd, err)
	}
	return true
}

// TestUnregisterConnWaitsForTheDelOfTheTeardownInFlight890: A's teardown is
// held inside forget, after it took A out of the map and before the
// EPOLL_CTL_DEL. A second UnregisterConn of A's number, the call a caller makes
// when it is unsure, must not return while the teardown is held: a miss
// promises that the DEL is done and that the number may be closed. (X1 moves
// the DEL after the unlock: the second call then misses the map and returns
// ErrUnknownFD at once, with A still in the epoll set.)
func TestUnregisterConnWaitsForTheDelOfTheTeardownInFlight890(t *testing.T) {
	t.Cleanup(func() { testHookBeforeDel = nil }) // runs last: after the loop is closed
	l, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	w := l.WorkerLoop(0).(*worker)
	a, aPeer := socketPair(t)
	t.Cleanup(func() { _ = unix.Close(a); _ = unix.Close(aPeer) })
	if err := w.RegisterConn(a, func([]byte) {}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn(A): %v", err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	letGo := func() { once.Do(func() { close(release) }) }
	t.Cleanup(letGo) // runs before Close
	var first atomic.Bool
	testHookBeforeDel = func(fd int) {
		if fd == a && first.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
	}

	firstErr := make(chan error, 1)
	go func() { firstErr <- w.UnregisterConn(a) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the teardown never reached forget's EPOLL_CTL_DEL")
	}
	secondErr := make(chan error, 1)
	go func() { secondErr <- w.UnregisterConn(a) }()

	if err, held := c890Held(secondErr); !held {
		t.Errorf("a second UnregisterConn(A) returned %v while the first teardown was held before its EPOLL_CTL_DEL: its caller may close the number with A still in the epoll set", err)
		secondErr <- err // keep the result for the wait below
	}
	if err, held := c890Held(firstErr); !held {
		t.Errorf("the held UnregisterConn(A) returned %v", err)
		firstErr <- err
	}
	inSetHeld := c890InEpollSet(t, w.epollFD, a)
	letGo()
	for _, ch := range []chan error{firstErr, secondErr} {
		select {
		case err := <-ch:
			if err != nil && !errors.Is(err, engine.ErrUnknownFD) {
				t.Errorf("UnregisterConn(A): %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("UnregisterConn(A) did not return within 5 s of the release")
		}
	}
	inSetAfter := c890InEpollSet(t, w.epollFD, a)
	t.Logf("C890 forget DEL: A in the epoll set while the teardown was held: %v; after both calls returned: %v", inSetHeld, inSetAfter)
	if !inSetHeld {
		t.Error("A was already out of the epoll set while the teardown was held before its DEL: the hook is not immediately before the DEL")
	}
	if inSetAfter {
		t.Error("A is in the epoll set after both UnregisterConn calls returned")
	}
}

// TestShutdownMarksEveryConnClosedBeforeItClosesTheEpollFd890: a hook between
// the marking of the conns and the close of the epoll fd checks every conn's
// closed flag. A conn not yet marked when the fd closes could still issue an
// EPOLL_CTL (ADD, MOD) under c.mu on a closed number, or on another epoll
// instance that took it (celeris#862). (X2 marks the conns after the close.)
func TestShutdownMarksEveryConnClosedBeforeItClosesTheEpollFd890(t *testing.T) {
	t.Cleanup(func() { testHookBeforeEpollClose = nil }) // runs last
	l, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	w := l.WorkerLoop(0).(*worker)

	const n = 3
	var conns []*driverConn
	for range n {
		fd, peer := socketPair(t)
		t.Cleanup(func() { _ = unix.Close(fd); _ = unix.Close(peer) })
		if err := w.RegisterConn(fd, func([]byte) {}, func(error) {}); err != nil {
			t.Fatalf("RegisterConn: %v", err)
		}
		conns = append(conns, c784Lookup(w, fd))
	}
	epfd := w.epollFD
	ran := false
	notClosed, epollOpen := 0, false
	testHookBeforeEpollClose = func() {
		ran = true
		// w.mu is held by shutdown. The number must still be an open
		// descriptor (F_GETFD succeeds): a shutdown that closes the fd and
		// only then clears w.epollFD would pass a comparison of the field.
		_, ferr := unix.FcntlInt(uintptr(epfd), unix.F_GETFD, 0)
		epollOpen = w.epollFD == epfd && ferr == nil
		for _, c := range conns {
			c.mu.Lock()
			if !c.closed {
				notClosed++
			}
			c.mu.Unlock()
		}
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	t.Logf("C890 shutdown order: hook ran %v with the epoll fd still open: %v; conns not yet marked closed at the hook: %d of %d", ran, epollOpen, notClosed, n)
	if !ran {
		t.Fatal("the hook never ran: shutdown did not reach the close of the epoll fd")
	}
	if !epollOpen {
		t.Error("the epoll fd was already closed at the hook: the hook is not before the close")
	}
	if notClosed != 0 {
		t.Errorf("%d of %d conns were not marked closed when shutdown reached the close of the epoll fd", notClosed, n)
	}
}

// TestAWakeAfterShutdownClosedTheEventfdWritesNothing890: right after shutdown
// closes the wake eventfd the hook puts an eventfd of its own on exactly the
// number the worker's had, and then wakes the worker, as a producer that was
// already past Write's closed check does (Write, enqueueFlush, wake). The new
// eventfd must not receive the write. This pins wakes that name the old number
// (one cached when the worker was made, or a Signal that ignores the close).
// It does not pin a wake that loads w.wakeFD.FD() before the close and writes
// the loaded number after it (mutant WAKEATOMIC): FD() already returns -1 once
// the close has run, so no hook placed after the close can order that shape,
// and the race detector reports nothing (the number is an atomic, the write is
// a syscall). Only the probabilistic oracle of
// TestWriteRacingCloseNeverTouchesTheClosedEventfd862 covers it (celeris#890).
func TestAWakeAfterShutdownClosedTheEventfdWritesNothing890(t *testing.T) {
	t.Cleanup(func() { testHookAfterWakeClose = nil }) // runs last
	l, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	w := l.WorkerLoop(0).(*worker)
	old := w.wakeFD.FD()
	if old < 0 {
		t.Fatal("the worker has no wake eventfd")
	}

	taker := -1
	var hookErr error
	testHookAfterWakeClose = func() {
		efd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
		if err != nil {
			hookErr = err
			return
		}
		if efd != old {
			// old was just closed, so it is the lowest number F_DUPFD_CLOEXEC
			// can return that is >= old; unlike dup3 it never replaces a
			// descriptor another goroutine opened on the number meanwhile.
			dup, err := unix.FcntlInt(uintptr(efd), unix.F_DUPFD_CLOEXEC, old)
			_ = unix.Close(efd)
			if err != nil {
				hookErr = err
				return
			}
			efd = dup
		}
		taker = efd
		w.wake()
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if taker >= 0 {
		t.Cleanup(func() { _ = unix.Close(taker) })
	}
	if hookErr != nil {
		t.Fatalf("hook: %v", hookErr)
	}
	if taker != old {
		t.Fatalf("the hook did not put an eventfd on the worker's old eventfd number %d (got %d): the hook did not run after the close", old, taker)
	}
	var v [8]byte
	n, rerr := unix.Read(taker, v[:])
	t.Logf("C890 wake: eventfd on the closed wake number %d reads %d bytes (err %v) after a wake that followed the close", old, n, rerr)
	if n == 8 && rerr == nil {
		t.Errorf("a wake issued after shutdown closed the wake eventfd wrote to number %d, now another eventfd's", old)
	}
}

// TestCloseAndUnregisterConnWaitForRegisterConnsAdd890: RegisterConn is held
// inside its c.mu section, after the closed check and before the
// EPOLL_CTL_ADD. Close and UnregisterConn both take c.mu to mark the conn
// closed, so neither may return while the section is held; if one did, it
// would be done with the conn (and Close with the epoll fd) before the ADD
// ran. (CHECKOUTSIDE checks c.closed under c.mu, releases it and issues the
// ADD after: the hook, which stays between the check and the ADD, is then
// outside the lock and the call returns at once.) Without a teardown, the ADD
// must land on the worker's own epoll instance: the worker delivers A's bytes.
func TestCloseAndUnregisterConnWaitForRegisterConnsAdd890(t *testing.T) {
	for _, teardown := range []string{"Close", "UnregisterConn", "None"} {
		t.Run(teardown, func(t *testing.T) {
			t.Cleanup(func() { testHookInAdd = nil }) // runs last
			l, err := New(1)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { _ = l.Close() })
			w := l.WorkerLoop(0).(*worker)
			a, aPeer := socketPair(t)
			t.Cleanup(func() { _ = unix.Close(a); _ = unix.Close(aPeer) })

			entered := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			letGo := func() { once.Do(func() { close(release) }) }
			t.Cleanup(letGo) // runs before Close
			var first atomic.Bool
			testHookInAdd = func(fd int) {
				if fd == a && first.CompareAndSwap(false, true) {
					close(entered)
					<-release
				}
			}

			got := make(chan struct{}, 4)
			fired := make(chan error, 2)
			regErr := make(chan error, 1)
			go func() {
				regErr <- w.RegisterConn(a, func([]byte) {
					select {
					case got <- struct{}{}:
					default:
					}
				}, func(err error) { fired <- err })
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("RegisterConn never reached its c.mu section")
			}

			tearErr := make(chan error, 1)
			switch teardown {
			case "Close":
				go func() { tearErr <- l.Close() }()
			case "UnregisterConn":
				go func() { tearErr <- w.UnregisterConn(a) }()
			}
			if teardown != "None" {
				if err, held := c890Held(tearErr); !held {
					t.Errorf("%s returned %v while RegisterConn was held between its closed check and its EPOLL_CTL_ADD", teardown, err)
				}
			}
			letGo()
			select {
			case err := <-regErr:
				if err != nil {
					t.Errorf("RegisterConn: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("RegisterConn did not return within 5 s of the release")
			}
			if teardown != "None" {
				select {
				case err := <-tearErr:
					if err != nil {
						t.Errorf("%s: %v", teardown, err)
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("%s did not return within 5 s of the release", teardown)
				}
			}
			switch teardown {
			case "Close":
				select {
				case err := <-fired:
					if !errors.Is(err, ErrLoopClosed) {
						t.Errorf("onClose got %v, want ErrLoopClosed", err)
					}
				case <-time.After(2 * time.Second):
					t.Error("onClose did not fire after Close")
				}
			case "UnregisterConn":
				select {
				case err := <-fired:
					if err != nil {
						t.Errorf("onClose got %v, want nil", err)
					}
				case <-time.After(2 * time.Second):
					t.Error("onClose did not fire after UnregisterConn")
				}
				if c890InEpollSet(t, w.epollFD, a) {
					t.Error("A is in the epoll set after UnregisterConn returned")
				}
			case "None":
				c784Queue(t, aPeer, []byte("hi"))
				select {
				case <-got:
				case <-time.After(2 * time.Second):
					t.Error("A's onRecv never ran: the EPOLL_CTL_ADD did not land on the worker's epoll instance")
				}
			}
		})
	}
}
