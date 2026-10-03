//go:build linux

package eventloop

// celeris#862: callers on other goroutines use the worker's own two
// descriptors, the wakeup eventfd and the epoll fd, and Loop.Close closes
// both. Neither number may be used once shutdown has closed it: by then the
// number can name a socket, a driver conn or another loop's descriptor.
//
//   - A Write that leaves bytes pending wakes the worker (enqueueFlush, then
//     wake) after it has released c.mu, and Write checks w.closed only on
//     entry. A Write that runs alongside Loop.Close used to read the eventfd's
//     number with no lock while shutdown closed it and stored -1, and could
//     write 8 bytes to the number after the close.
//   - RegisterConn used to take the epoll fd's number under w.mu, release
//     w.mu, and issue its EPOLL_CTL_ADD after. A shutdown in between closed
//     the epoll fd, and the ADD went to a closed number, or to the epoll
//     instance that had taken it. It now issues the ADD under c.mu, after a
//     check that the conn has not been torn down, so an UnregisterConn of the
//     same fd in between leaves no ADD behind either.

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// c862FillSendBuffer writes to fd until the socket takes no more, so every
// later flush of fd stops at EAGAIN and leaves its bytes pending.
func c862FillSendBuffer(t *testing.T, fd int) int {
	t.Helper()
	chunk := make([]byte, 64<<10)
	total := 0
	for {
		n, err := unix.Write(fd, chunk)
		if n > 0 {
			total += n
		}
		if err == unix.EAGAIN {
			return total
		}
		if err != nil {
			t.Fatalf("fill send buffer: %v", err)
		}
	}
}

// TestWriteRacingCloseNeverTouchesTheClosedEventfd862: Writes that leave
// bytes pending, on four goroutines, while the loop closes. Each Write takes
// the pending path: the conn's send buffer is full and its peer never reads,
// so the flush stops at EAGAIN and the Write calls enqueueFlush, which wakes
// the worker. Under -race, a wake that reads the eventfd's number with no
// lock while shutdown closes it and stores -1 is a reported data race, and
// the test's coverage rests on that report (CI runs this package with
// -race). Its second check is opportunistic: two eventfds opened as soon as
// Close returns take the numbers it freed (the worker's eventfd and epoll
// fd), and neither may receive a write from a wake that loaded the old number
// before the close. A wake has to land in the few microseconds between the
// close and the reuse for it to fire, so it seldom does, with or without
// -race.
func TestWriteRacingCloseNeverTouchesTheClosedEventfd862(t *testing.T) {
	const rounds, writers = 64, 4
	one := []byte{'w'}
	pendingWrites, hits := 0, 0
	for r := 0; r < rounds; r++ {
		l, err := New(1)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		w := l.WorkerLoop(0).(*worker)
		a, aPeer := socketPair(t)
		if err := w.RegisterConn(a, func([]byte) {}, func(error) {}); err != nil {
			t.Fatalf("RegisterConn: %v", err)
		}
		c862FillSendBuffer(t, a)

		var started sync.WaitGroup
		var done sync.WaitGroup
		var nWrites atomic.Int64
		for range writers {
			started.Add(1)
			done.Add(1)
			go func() {
				defer done.Done()
				first := true
				for {
					err := w.Write(a, one)
					if first {
						first = false
						started.Done()
					}
					if err != nil {
						return // ErrLoopClosed, or ErrUnknownFD once shutdown has marked the conn closed
					}
					nWrites.Add(1)
				}
			}()
		}
		started.Wait()
		time.Sleep(200 * time.Microsecond)
		if err := l.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		// Take the numbers Close freed: the lowest free numbers, unless a
		// lower one was free already.
		var takers [2]int
		for i := range takers {
			takers[i], err = unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
			if err != nil {
				t.Fatalf("eventfd: %v", err)
			}
		}
		done.Wait()
		pendingWrites += int(nWrites.Load())
		for _, efd := range takers {
			var v [8]byte
			if n, err := unix.Read(efd, v[:]); n == 8 && err == nil {
				hits++
				t.Errorf("round %d: an eventfd opened after Close, on number %d, was written to: a wake wrote the worker's closed eventfd's number", r, efd)
			}
			_ = unix.Close(efd)
		}
		_ = unix.Close(a)
		_ = unix.Close(aPeer)
	}
	t.Logf("C862 wake: %d rounds x %d writers, %d Writes left bytes pending (each one wakes the worker); wakes that reached a number Close had freed: %d", rounds, writers, pendingWrites, hits)
	if pendingWrites == 0 {
		t.Fatal("no Write left bytes pending: the test never took the wake path")
	}
}

// TestRegisterConnRacingCloseNeverAddsToAClosedEpoll862: Loop.Close runs to
// completion while RegisterConn is between putting the conn in the map and
// issuing its EPOLL_CTL_ADD (the hook runs there, with no lock held). Close
// closes the worker's epoll fd, and the test then opens epoll instances until
// one takes the closed epoll fd's number. The ADD must not land on that
// number: not on the instance that took it, and not on a closed descriptor.
// RegisterConn then either reports the conn registered, and Close has torn
// it down (onClose(ErrLoopClosed)), or it returns an error and onClose never
// fires.
func TestRegisterConnRacingCloseNeverAddsToAClosedEpoll862(t *testing.T) {
	a, aPeer := socketPair(t)
	t.Cleanup(func() { _ = unix.Close(a); _ = unix.Close(aPeer) })

	l, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w := l.WorkerLoop(0).(*worker)
	epfd := w.epollFD

	var ran atomic.Int32
	closed := make(chan struct{})
	closedInHook := false
	var takers []int
	t.Cleanup(func() {
		for _, fd := range takers {
			_ = unix.Close(fd)
		}
	})
	testHookBeforeAdd = func(fd int) {
		if fd != a || ran.Add(1) != 1 {
			return
		}
		go func() {
			_ = l.Close()
			close(closed)
		}()
		select {
		case <-closed:
			closedInHook = true
		case <-time.After(5 * time.Second):
			return
		}
		for range 64 {
			ep, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
			if err != nil {
				t.Errorf("epoll_create1: %v", err)
				return
			}
			takers = append(takers, ep)
			if ep == epfd {
				return
			}
		}
	}
	t.Cleanup(func() { testHookBeforeAdd = nil })

	fired := make(chan error, 1)
	rerr := w.RegisterConn(a, func([]byte) {}, func(err error) { fired <- err })
	if ran.Load() == 0 {
		_ = l.Close()
		t.Fatalf("the hook never ran (RegisterConn returned %v): the test did not reach RegisterConn's EPOLL_CTL_ADD", rerr)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return within 5 s")
	}
	tookNumber := len(takers) > 0 && takers[len(takers)-1] == epfd
	landed := false
	if tookNumber {
		err := unix.EpollCtl(epfd, unix.EPOLL_CTL_ADD, a, &unix.EpollEvent{Events: unix.EPOLLIN, Fd: int32(a)})
		landed = errors.Is(err, unix.EEXIST)
	}
	var closeErr error
	gotClose := false
	select {
	case closeErr = <-fired:
		gotClose = true
	case <-time.After(2 * time.Second):
	}
	t.Logf("C862 register: Close finished inside the hook: %v; an epoll instance took the closed epoll fd's number %d: %v; A was in that instance's set: %v; RegisterConn returned %v; onClose fired: %v (%v)",
		closedInHook, epfd, tookNumber, landed, rerr, gotClose, closeErr)
	if !closedInHook {
		t.Fatal("Close did not finish while RegisterConn was between the map and the ADD: the test did not drive the window")
	}
	if !tookNumber {
		t.Fatalf("no epoll instance took the closed epoll fd's number %d in %d tries: the test cannot check where the ADD landed", epfd, len(takers))
	}
	if landed {
		t.Errorf("RegisterConn's EPOLL_CTL_ADD landed on the epoll instance that took the closed epoll fd's number %d", epfd)
	}
	if rerr != nil && !errors.Is(rerr, ErrLoopClosed) {
		t.Errorf("RegisterConn returned %v: its EPOLL_CTL_ADD was issued on the worker's epoll fd after shutdown had closed it", rerr)
	}
	if rerr != nil && gotClose {
		t.Errorf("RegisterConn returned %v, and the conn's onClose fired too (%v): the caller is told the registration failed for a conn the loop registered and tore down", rerr, closeErr)
	}
	if rerr == nil && (!gotClose || !errors.Is(closeErr, ErrLoopClosed)) {
		t.Errorf("RegisterConn returned nil, and Close then ran, but onClose fired %v with %v, want ErrLoopClosed", gotClose, closeErr)
	}
}

// TestRegisterConnRacingUnregisterConnLeavesNoEpollEntry862: an
// UnregisterConn of the same fd runs to completion while RegisterConn is
// between putting the conn in the map and issuing its EPOLL_CTL_ADD (the hook
// runs there, with no lock held). Once both have returned, fd must not be in
// the worker's epoll set: an ADD issued after the unregister's DEL leaves a
// registration that no map entry owns, and the owner may already have closed
// the number. The test probes the set with an EPOLL_CTL_ADD of its own, which
// fails with EEXIST if fd is still in it.
func TestRegisterConnRacingUnregisterConnLeavesNoEpollEntry862(t *testing.T) {
	l, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	w := l.WorkerLoop(0).(*worker)
	a, aPeer := socketPair(t)
	t.Cleanup(func() { _ = unix.Close(a); _ = unix.Close(aPeer) })

	ran := false
	uerr := errors.New("not called")
	testHookBeforeAdd = func(fd int) {
		if fd != a || ran {
			return
		}
		ran = true
		uerr = w.UnregisterConn(a)
	}
	t.Cleanup(func() { testHookBeforeAdd = nil })
	var closes atomic.Int32
	rerr := w.RegisterConn(a, func([]byte) {}, func(error) { closes.Add(1) })
	testHookBeforeAdd = nil
	if !ran {
		t.Fatalf("the hook never ran (RegisterConn returned %v): the test did not reach RegisterConn's EPOLL_CTL_ADD", rerr)
	}
	if uerr != nil {
		t.Fatalf("UnregisterConn in the window returned %v: the conn was not in the map, so the test did not drive the window", uerr)
	}
	ev := unix.EpollEvent{Events: unix.EPOLLIN, Fd: int32(a)}
	perr := unix.EpollCtl(w.epollFD, unix.EPOLL_CTL_ADD, a, &ev)
	inSet := errors.Is(perr, unix.EEXIST)
	if perr == nil {
		_ = unix.EpollCtl(w.epollFD, unix.EPOLL_CTL_DEL, a, nil)
	}
	t.Logf("C862 register/unregister: UnregisterConn in the window returned %v; RegisterConn returned %v; onClose fired %d times; A in the epoll set after both returned: %v (probe ADD: %v)",
		uerr, rerr, closes.Load(), inSet, perr)
	if perr != nil && !inSet {
		t.Fatalf("the probe's EPOLL_CTL_ADD failed with %v: the test cannot tell whether A is in the epoll set", perr)
	}
	if inSet {
		t.Errorf("A is in the worker's epoll set after UnregisterConn(A) returned: RegisterConn's EPOLL_CTL_ADD was issued after the unregister's DEL")
	}
	if rerr != nil {
		t.Errorf("RegisterConn returned %v for a conn UnregisterConn had already torn down, want nil (its onClose has fired)", rerr)
	}
	if n := closes.Load(); n != 1 {
		t.Errorf("onClose fired %d times, want once (UnregisterConn's)", n)
	}
}
