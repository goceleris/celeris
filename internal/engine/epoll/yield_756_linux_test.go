//go:build linux

package epoll

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestEpollWaitYielding756 pins the gate of the idle-wait yield (celeris#756):
// no yield unless a goroutine was readied and the wait would block; a ready
// event or a zero timeout never yields; the flag is consumed by every wait.
func TestEpollWaitYielding756(t *testing.T) {
	epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(epfd) }()
	efd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(efd) }()
	if err := unix.EpollCtl(epfd, unix.EPOLL_CTL_ADD, efd, &unix.EpollEvent{Events: unix.EPOLLIN, Fd: int32(efd)}); err != nil {
		t.Fatal(err)
	}
	l := &Loop{epollFD: epfd, events: make([]unix.EpollEvent, 8)}

	// Nothing readied: a plain wait, no yield, the timeout is honoured.
	start := time.Now()
	n, err := l.epollWaitYielding(5)
	if err != nil || n != 0 || l.yieldLastNs != 0 {
		t.Fatalf("plain wait: n=%d err=%v yieldLastNs=%d", n, err, l.yieldLastNs)
	}
	if time.Since(start) < 4*time.Millisecond {
		t.Fatalf("plain wait returned early")
	}

	// Readied and idle: yields once, consumes the flag, still waits out the timeout.
	l.yieldReady = true
	n, err = l.epollWaitYielding(5)
	if err != nil || n != 0 || l.yieldReady || l.yieldLastNs == 0 {
		t.Fatalf("idle wait: n=%d err=%v ready=%v yieldLastNs=%d", n, err, l.yieldReady, l.yieldLastNs)
	}

	// Readied again inside the gap: no second yield.
	last := l.yieldLastNs
	l.yieldReady = true
	if _, err = l.epollWaitYielding(1); err != nil || l.yieldReady {
		t.Fatalf("gap wait: err=%v ready=%v", err, l.yieldReady)
	}
	if l.yieldLastNs != last && time.Duration(l.yieldLastNs-last) < yieldMinGap {
		t.Fatalf("yielded again inside the gap: %d -> %d", last, l.yieldLastNs)
	}

	// An event is ready: it is returned without a yield.
	time.Sleep(yieldMinGap + time.Millisecond)
	if _, err := unix.Write(efd, []byte{1, 0, 0, 0, 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	last = l.yieldLastNs
	l.yieldReady = true
	n, err = l.epollWaitYielding(1000)
	if err != nil || n != 1 || l.yieldLastNs != last {
		t.Fatalf("busy wait: n=%d err=%v yieldLastNs moved %d -> %d", n, err, last, l.yieldLastNs)
	}

	// A zero timeout never yields.
	l.yieldReady = true
	if _, err = l.epollWaitYielding(0); err != nil || l.yieldReady || l.yieldLastNs != last {
		t.Fatalf("zero timeout: err=%v ready=%v", err, l.yieldReady)
	}
}
