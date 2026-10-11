//go:build linux

package bindiag

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// ResolvePort0 decides the port of an address written with port 0 once, for
// every socket the engine is about to bind on it (celeris#895).
//
// The standalone epoll and io_uring engines bind Config.Addr once per
// loop/worker, each in its own goroutine, and a bind of port 0 picks an
// ephemeral port: with several of them, one port each, of which Addr()
// reports one and only that loop or worker is ever given a connection; and a
// loop that re-creates its listen socket on ResumeAccept bound port 0 again
// and moved to yet another port. So the port is decided here, before any of
// them binds, and the engine hands every one of them the concrete address.
//
// An address whose port is not 0, or that parse rejects (the loops report
// that themselves, with their own diagnostics), comes back unchanged with no
// hold. Otherwise ResolvePort0 binds a socket the way the engines bind theirs
// (the family and address parse gives; SO_REUSEPORT), reads the port the
// kernel chose, and returns addr with that port and the socket. The socket
// never listens, which is what makes it a hold and not a participant: only a
// listening SO_REUSEPORT socket is in the port's reuseport group, so it never
// takes a connection, and the loops' sockets (same user, SO_REUSEPORT) bind
// and listen beside it. It keeps every other socket off the port, one that
// binds with SO_REUSEADDR as net.Listen does included, until the caller
// closes it, which the engine does as Listen returns: the same hold the
// adaptive engine takes (celeris#616, internal/adaptive/porthold.go), which
// cannot be shared, as the adaptive package imports the engines. The engine
// keeps it that long, not only until every loop is listening, because a pause
// closes every listen socket and the resume binds the port again: with the
// hold released at startup a connect(2) of any process on the host, which
// takes its source port from the same ephemeral range, could take the port
// in between, and the resume would fail for good. A socket that binds and
// never listens is not given a connection, and while it is bound the kernel
// leaves the port out of that choice of a source port.
//
// Unlike adaptive's it sets no SO_REUSEADDR in any case: a port 0 bind is
// given a port nothing is bound to, so it never meets the leftovers of an
// earlier listener (TIME_WAIT and the like) that make that one fall back.
//
// The caller closes the hold on every path; closing a nil *os.File is
// harmless. A hold that cannot be made is an error and not a fallback to
// per-loop binds, which is the defect.
func ResolvePort0(addr string, parse func(string) (unix.Sockaddr, error)) (string, *os.File, error) {
	sa, err := parse(addr)
	if err != nil || sockaddrPort(sa) != 0 {
		return addr, nil, nil
	}
	family := unix.AF_INET
	if _, ok := sa.(*unix.SockaddrInet6); ok {
		family = unix.AF_INET6
	}
	fd, err := unix.Socket(family, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return addr, nil, fmt.Errorf("port 0 hold for %s: socket: %w", addr, err)
	}
	fail := func(step string, err error) (string, *os.File, error) {
		diag := Format(fd, sa)
		_ = unix.Close(fd)
		return addr, nil, fmt.Errorf("port 0 hold for %s: %s: %w [%s]", addr, step, err, diag)
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
		return fail("SO_REUSEPORT", err)
	}
	if err := unix.Bind(fd, sa); err != nil {
		return fail("bind", err)
	}
	bound, err := unix.Getsockname(fd)
	if err != nil {
		return fail("getsockname", err)
	}
	port := sockaddrPort(bound)
	if port == 0 {
		return fail("getsockname", fmt.Errorf("the kernel bound no port"))
	}
	// An *os.File so that closing it is idempotent. The descriptor is
	// blocking, so os.NewFile does not register it with the runtime poller.
	return WithPort(addr, port), os.NewFile(uintptr(fd), "celeris-port-hold"), nil
}

// WithPort returns addr, an address in the forms the engines' parseAddr
// accepts ("host:port", "[v6]:port", or a bare port), with its port replaced:
// the host is kept as it was written.
func WithPort(addr string, port int) string {
	p := strconv.Itoa(port)
	if strings.HasPrefix(addr, "[") {
		if i := strings.Index(addr, "]:"); i >= 0 {
			return addr[:i+2] + p
		}
		return addr
	}
	if i := strings.LastIndexByte(addr, ':'); i >= 0 {
		return addr[:i+1] + p
	}
	return ":" + p
}
