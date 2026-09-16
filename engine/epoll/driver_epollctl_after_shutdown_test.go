//go:build linux

package epoll

import (
	"sync/atomic"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/wakefd"
	"github.com/goceleris/celeris/resource"
)

// celeris#655, the epoll driver path — the same defect on the other
// descriptor a driver goroutine can reach.
//
// Loop.shutdown closes l.epollFD and, before this change, left the number in
// the field. A driver's RegisterConn / UnregisterConn / Write runs on the
// driver's own goroutine, which nothing joins, so an epoll_ctl could be
// issued against that number after it was free to be recycled. It is the
// deterministic case, not a narrow race: shutdownDrivers sets
// l.driverConns = nil and RegisterConn rebuilds the map from nil, so a
// registration after shutdown always reaches the epoll_ctl.
//
// The witness here is stronger than "an error was returned": a second epoll
// instance is parked on the closed descriptor's number, and the test asserts
// that instance did not silently gain the connection.

// parkFD655 asserts n is closed and moves victim onto exactly that number.
// F_DUPFD_CLOEXEC returns the LOWEST FREE number >= n, so anything other
// than n means n was still open and the test stops rather than reporting a
// pass it did not earn.
func parkFD655(t *testing.T, victim, n int) {
	t.Helper()
	if _, err := unix.FcntlInt(uintptr(n), unix.F_GETFD, 0); err != unix.EBADF {
		t.Fatalf("precondition: fd %d is not closed after shutdown (F_GETFD err=%v)", n, err)
	}
	got, err := unix.FcntlInt(uintptr(victim), unix.F_DUPFD_CLOEXEC, n)
	if err != nil || got != n {
		if err == nil {
			_ = unix.Close(got)
		}
		t.Fatalf("could not park a descriptor on %d (got %d, err=%v)", n, got, err)
	}
	t.Cleanup(func() { _ = unix.Close(n) })
}

// inInterestSet655 reports whether fd is already in the epoll instance epfd:
// EPOLL_CTL_ADD returns EEXIST exactly when it is. A probe that adds is
// removed again so the check leaves no trace.
func inInterestSet655(t *testing.T, epfd, fd int) bool {
	t.Helper()
	err := unix.EpollCtl(epfd, unix.EPOLL_CTL_ADD, fd, &unix.EpollEvent{
		Events: unix.EPOLLIN,
		Fd:     int32(fd),
	})
	switch err {
	case unix.EEXIST:
		return true
	case nil:
		_ = unix.EpollCtl(epfd, unix.EPOLL_CTL_DEL, fd, nil)
		return false
	default:
		t.Fatalf("probing the interest set of %d for fd %d: %v", epfd, fd, err)
		return false
	}
}

func socketFor655Ctl(t *testing.T) int {
	t.Helper()
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(pair[0]); _ = unix.Close(pair[1]) })
	return pair[0]
}

func TestRegisterConnAfterShutdownDoesNotTouchTheClosedEpollFD(t *testing.T) {
	epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		t.Fatalf("epoll_create1: %v", err)
	}
	// The stand-in for whatever the process opens next. Created BEFORE the
	// shutdown, or epoll_create1 would take the very number under test.
	victim, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		t.Fatalf("epoll_create1 (victim): %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(victim) })

	shutdownRan := false
	t.Cleanup(func() {
		if !shutdownRan {
			_ = unix.Close(epfd)
		}
	})

	l := &Loop{
		epollFD:      epfd,
		listenFD:     -1,
		timerFD:      -1,
		wakeFD:       wakefd.New(-1),
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

	// Positive control: on a LIVE loop the same call really does reach
	// epoll_ctl, so a clean result after shutdown cannot mean RegisterConn
	// is simply inert.
	live := socketFor655Ctl(t)
	if err := l.RegisterConn(live, func([]byte) {}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn on a live loop: %v", err)
	}
	if !inInterestSet655(t, epfd, live) {
		t.Fatal("RegisterConn on a LIVE loop did not add the fd to the loop's epoll " +
			"set: the producer under test never reaches epoll_ctl, so the " +
			"post-shutdown assertion below would pass for the wrong reason")
	}

	l.shutdown()
	shutdownRan = true
	parkFD655(t, victim, epfd)

	late := socketFor655Ctl(t)
	regErr := l.RegisterConn(late, func([]byte) {}, func(error) {})

	// The real witness: whatever inherited the number must not have been
	// operated on. On main the epoll_ctl succeeds against the recycled
	// descriptor and the victim instance silently starts watching `late`.
	if inInterestSet655(t, epfd, late) {
		t.Errorf("RegisterConn added fd %d to the epoll instance that now holds "+
			"descriptor %d after shutdown closed the loop's epoll fd; that "+
			"instance is now watching a connection nobody registered with it "+
			"(celeris#655)", late, epfd)
	}
	if regErr == nil {
		t.Errorf("RegisterConn returned nil after shutdown: the driver believes fd %d "+
			"is registered on a loop that is gone, and will never get its onClose", late)
	}
}
