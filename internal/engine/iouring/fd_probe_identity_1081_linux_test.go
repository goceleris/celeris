//go:build linux

package iouring

// celeris#1081: TestShutdownEndsOwedOpsBeforeClosing failed in CI with one
// of its eight server-side descriptors "still open after the engine
// stopped". The descriptor it checked was not the engine's. serverFDFor
// took the first descriptor whose peer was the test client's address, and a
// server-side socket an earlier test had left open names an earlier client's
// port as its peer: connect(2) hands that port out again once that client is
// gone, and the test's client can be given it. /proc/self/fd lists names in
// lexical order, so that socket was found before the engine's whenever its
// number sorted first. The test below forces the same collision
// deterministically with a stray of the other kind (a client whose peer, a
// listener's port, is then given to the test's client): what matters to
// serverFDFor is only that the stray names the client's address as its peer,
// at a number that sorts before the server side's.

import (
	"context"
	"net"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// dupAbove moves c's descriptor to the lowest free number >= lowest and closes
// c, returning the new number, which the test closes.
func dupAbove(t *testing.T, c syscall.Conn, lowest int) int {
	t.Helper()
	rc, err := c.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	nfd := -1
	var derr error
	if err := rc.Control(func(fd uintptr) { nfd, derr = unix.FcntlInt(fd, unix.F_DUPFD_CLOEXEC, lowest) }); err != nil {
		t.Fatal(err)
	}
	if derr != nil {
		t.Fatalf("F_DUPFD %d: %v", lowest, derr)
	}
	t.Cleanup(func() { _ = unix.Close(nfd) })
	if cl, ok := c.(interface{ Close() error }); ok {
		_ = cl.Close()
	}
	return nfd
}

func acceptTCP(t *testing.T, ln net.Listener) *net.TCPConn {
	t.Helper()
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	return c.(*net.TCPConn)
}

// TestServerFDForIsTheServerSideOfTheDialedConn1081: a server with two
// connections, A and B, and a stray socket whose peer is B's client address.
// The stray sorts first ("1xx"), A's server side next ("2xx"), B's last
// ("9xxx"). serverFDFor for B must return B's server side: matching the peer
// alone returns the stray, matching the server address alone returns A's (or
// the listener).
func TestServerFDForIsTheServerSideOfTheDialedConn1081(t *testing.T) {
	// The stray: a client connected to port X, then X's listener closed, so
	// that B can be given X as its own port while the stray still names it
	// as its peer.
	lnX, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	portX := lnX.Addr().(*net.TCPAddr).Port
	stray, err := net.Dial("tcp", lnX.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	strayAccepted := acceptTCP(t, lnX)
	defer func() { _ = strayAccepted.Close() }()
	_ = lnX.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	server := ln.Addr().String()
	a, err := net.Dial("tcp", server)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	sA := acceptTCP(t, ln)
	d := net.Dialer{
		LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: portX},
		Control: func(_, _ string, c syscall.RawConn) error {
			var serr error
			if err := c.Control(func(fd uintptr) {
				serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
			}); err != nil {
				return err
			}
			return serr
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	b, err := d.DialContext(ctx, "tcp", server)
	if err != nil {
		t.Skipf("apparatus: cannot dial from the stray's peer port %d: %v", portX, err)
	}
	defer func() { _ = b.Close() }()
	sB := acceptTCP(t, ln)
	local := b.LocalAddr().String()

	strayFD := dupAbove(t, stray.(*net.TCPConn), 100)
	aFD := dupAbove(t, sA, 200)
	bFD := dupAbove(t, sB, 9000)
	if !namesPeer(strayFD, local) {
		t.Fatalf("apparatus: the stray (fd %d) does not name %s as its peer", strayFD, local)
	}
	if !namesPeer(bFD, local) {
		t.Fatalf("apparatus: B's server side (fd %d) does not name %s as its peer", bFD, local)
	}

	got := serverFDFor(local, server)
	if got != bFD {
		lsa, _ := unix.Getsockname(got)
		psa, _ := unix.Getpeername(got)
		t.Fatalf("serverFDFor(%s, %s) = %d (local %s, peer %s), want %d, B's server side (stray %d, A's server side %d)",
			local, server, got, sockaddrString(got, lsa), sockaddrString(got, psa), bFD, strayFD, aFD)
	}
}
