//go:build linux

package sockoptstest

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// SO_PREFER_BUSY_POLL and SO_BUSY_POLL_BUDGET, which golang.org/x/sys/unix
// does not name in every version celeris builds with.
const (
	SOPreferBusyPoll   = 69
	SOBusyPollBudget   = 70
	acceptedFDDeadline = 3 * time.Second
)

// Baseline is what SO_BUSY_POLL reads on a socket nothing has set it on. It is
// not always 0: a new socket starts from the net.core.busy_read sysctl, and a
// connection the kernel accepts takes the listen socket's value. A test that
// asserts "busy poll is off" compares the accepted socket with this, so that
// it holds on a host whose administrator set the sysctl and still cannot pass
// on a socket the engine set the option on.
func Baseline(t testing.TB) int {
	t.Helper()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	defer func() { _ = unix.Close(fd) }()
	v, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_BUSY_POLL)
	if err != nil {
		t.Fatalf("getsockopt SO_BUSY_POLL on a new socket: %v", err)
	}
	return v
}

// AcceptedBusyPoll is SO_BUSY_POLL, in microseconds, of the server-side socket
// of client's connection. See Accepted.
func AcceptedBusyPoll(t testing.TB, client net.Conn) int {
	t.Helper()
	return Accepted(t, client, unix.SOL_SOCKET, unix.SO_BUSY_POLL)
}

// Accepted reads one integer socket option of the server-side socket of
// client's connection, wherever in this process that socket lives (a native
// engine's descriptor, which no test API hands out). The socket is found by
// scanning /proc/self/fd for the descriptor whose local address is the
// client's remote address and whose peer is the client's local address, so it
// needs no knowledge of the engine, and it cannot pick the client's own end.
//
// The caller must have completed a request and response on client first: the
// native engines apply their socket options right after the accept, before the
// first read, so a response is proof the options are on the socket.
func Accepted(t testing.TB, client net.Conn, level, opt int) int {
	t.Helper()
	local, lok := addrPort(client.LocalAddr())
	remote, rok := addrPort(client.RemoteAddr())
	if !lok || !rok {
		t.Fatalf("client addresses %v -> %v are not TCP", client.LocalAddr(), client.RemoteAddr())
	}
	var last error
	for deadline := time.Now().Add(acceptedFDDeadline); ; time.Sleep(10 * time.Millisecond) {
		fd, ok := findAccepted(local, remote)
		if ok {
			v, err := unix.GetsockoptInt(fd, level, opt)
			if err == nil {
				return v
			}
			last = err // the engine closed the descriptor under the scan: look again
		}
		if time.Now().After(deadline) {
			break
		}
	}
	if last != nil {
		t.Fatalf("getsockopt(level %d, option %d) on the accepted socket of %v -> %v: %v", level, opt, local, remote, last)
	}
	t.Fatalf("no descriptor of this process is the accepted end of %v -> %v", local, remote)
	return 0
}

func findAccepted(clientLocal, clientRemote netip.AddrPort) (int, bool) {
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0, false
	}
	for _, ent := range ents {
		fd, err := strconv.Atoi(ent.Name())
		if err != nil {
			continue
		}
		peer, err := unix.Getpeername(fd)
		if err != nil {
			continue // not a connected socket
		}
		self, err := unix.Getsockname(fd)
		if err != nil {
			continue
		}
		p, pok := sockaddrPort(peer)
		s, sok := sockaddrPort(self)
		if pok && sok && p == clientLocal && s == clientRemote {
			return fd, true
		}
	}
	return 0, false
}

func addrPort(a net.Addr) (netip.AddrPort, bool) {
	ta, ok := a.(*net.TCPAddr)
	if !ok {
		return netip.AddrPort{}, false
	}
	ap := ta.AddrPort()
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()), true
}

func sockaddrPort(sa unix.Sockaddr) (netip.AddrPort, bool) {
	switch a := sa.(type) {
	case *unix.SockaddrInet4:
		return netip.AddrPortFrom(netip.AddrFrom4(a.Addr), uint16(a.Port)), true
	case *unix.SockaddrInet6:
		return netip.AddrPortFrom(netip.AddrFrom16(a.Addr).Unmap(), uint16(a.Port)), true
	}
	return netip.AddrPort{}, false
}

// RoundTrip sends one HTTP/1.1 request on c and reads the first bytes of the
// response, so that the server has accepted c and applied its socket options
// to it (see Accepted).
func RoundTrip(t testing.TB, c net.Conn) {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	defer func() { _ = c.SetDeadline(time.Time{}) }()
	if _, err := fmt.Fprintf(c, "GET /ping HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatalf("write the request: %v", err)
	}
	buf := make([]byte, 12)
	n := 0
	for n < len(buf) {
		m, err := c.Read(buf[n:])
		n += m
		if err != nil {
			t.Fatalf("read the response: %v (%q)", err, buf[:n])
		}
	}
	if string(buf[:9]) != "HTTP/1.1 " {
		t.Fatalf("response %q is not HTTP/1.1", buf[:n])
	}
}
