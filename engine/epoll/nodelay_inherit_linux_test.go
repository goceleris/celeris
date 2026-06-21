//go:build linux

package epoll

import (
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestListenSocketHasNoDelay asserts createListenSocket enables TCP_NODELAY on
// the listen socket itself — the precondition for accepted sockets inheriting
// it (#337).
func TestListenSocketHasNoDelay(t *testing.T) {
	lfd, err := createListenSocket("127.0.0.1:0")
	if err != nil {
		t.Skipf("listen socket unavailable: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(lfd) })

	v, err := unix.GetsockoptInt(lfd, unix.IPPROTO_TCP, unix.TCP_NODELAY)
	if err != nil {
		t.Fatalf("getsockopt TCP_NODELAY: %v", err)
	}
	if v == 0 {
		t.Fatal("listen socket TCP_NODELAY not set")
	}
}

// TestAcceptedConnInheritsNoDelay verifies accepted sockets carry TCP_NODELAY
// without acceptAll re-issuing the per-accept setsockopt. The kernel copies
// TCP_NODELAY from the listen socket at SYN time; this guards that the
// per-accept NODELAY removal (#337) leaves accepted conns nagle-disabled.
func TestAcceptedConnInheritsNoDelay(t *testing.T) {
	lfd, err := createListenSocket("127.0.0.1:0")
	if err != nil {
		t.Skipf("listen socket unavailable: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(lfd) })

	la := boundAddr(lfd)
	if la == nil {
		t.Skip("could not resolve bound addr")
	}
	tcpAddr, ok := la.(*net.TCPAddr)
	if !ok {
		t.Skipf("unexpected addr type %T", la)
	}

	cfd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Skipf("client socket unavailable: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(cfd) })

	var sa unix.SockaddrInet4
	sa.Port = tcpAddr.Port
	copy(sa.Addr[:], tcpAddr.IP.To4())
	if err := unix.Connect(cfd, &sa); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// TCP_DEFER_ACCEPT is set on the listener, so the connection only becomes
	// acceptable once data arrives — send a byte before accepting.
	if _, err := unix.Write(cfd, []byte{'x'}); err != nil {
		t.Fatalf("write: %v", err)
	}

	var afd int
	for i := 0; i < 200; i++ {
		afd, _, err = unix.Accept4(lfd, unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)
		if err == unix.EAGAIN {
			time.Sleep(time.Millisecond)
			continue
		}
		break
	}
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(afd) })

	v, err := unix.GetsockoptInt(afd, unix.IPPROTO_TCP, unix.TCP_NODELAY)
	if err != nil {
		t.Fatalf("getsockopt TCP_NODELAY on accepted fd: %v", err)
	}
	if v == 0 {
		t.Fatal("accepted socket did not inherit TCP_NODELAY from listener")
	}
}
