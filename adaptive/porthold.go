//go:build linux

package adaptive

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"

	"golang.org/x/sys/unix"
)

// holdPort decides the concrete host:port that both sub-engines will serve on
// and returns it together with a socket that keeps that port bound until the
// start engine has bound its own sockets on it (celeris#616).
//
// Both sub-engines must serve one concrete address (see New), and every one
// of their workers binds it independently in Listen, which runs later than
// New: an engine built in one place is often started somewhere else. The
// port used to be decided by binding it, reading its number and closing it
// again, so it belonged to nobody from New until the start engine bound it,
// and any socket that bound it in between made Listen fail with EADDRINUSE.
// The socket returned here closes that window. Engine.Listen closes it once
// the start engine has published its address, and Engine.Shutdown closes it
// for an engine that is never started.
//
// The socket is bound the way the sub-engines bind (epoll and io_uring
// createListenSocket), with two differences, and both are what make it safe:
//
//   - It never listens. Only a listening socket is in the kernel's table of
//     listeners, the one an incoming SYN is matched against, and only a
//     listening SO_REUSEPORT socket is in the port's reuseport group. So the
//     socket cannot take a connection, a dial in the window is refused as it
//     was before, and once the start engine's sockets are listening none of
//     the connections they are given can land on this one.
//   - It sets SO_REUSEPORT and, when it can, not SO_REUSEADDR. The
//     sub-engines' sockets set SO_REUSEPORT and are owned by the same user,
//     so the kernel lets them bind and listen beside it. Every other socket
//     is refused the port: two sockets may share a port through SO_REUSEADDR
//     only when both of them set it, and Go's net.Listen sets SO_REUSEADDR,
//     so a holder that set it too would let an ordinary Go listener bind the
//     port and then take it from the sub-engines exactly as before.
//
// That exclusive hold cannot be bound while the port still has sockets left
// by an earlier listener that did not set SO_REUSEPORT: the TIME_WAIT,
// FIN_WAIT or CLOSE_WAIT ends of the connections a net/http server, or the
// std engine, had when it stopped. Those sockets have SO_REUSEADDR and not
// SO_REUSEPORT, and the kernel lets a socket bind beside them only if it sets
// SO_REUSEADDR as well; net.Listen and the sub-engines do, which is why they
// bind there. holdPort then binds the hold with SO_REUSEADDR too: a shared
// hold. It still refuses the port to every socket without SO_REUSEADDR, but
// not to one with it, because two sockets that both set SO_REUSEADDR may
// share a port while neither listens. A socket that takes the port in that
// state makes the start engine's bind fail with EADDRINUSE at Listen, as
// every steal did before celeris#616; it never makes the engine serve another
// address. The third result reports whether the hold is the exclusive one.
//
// The address is the one net.Listen used to produce here, so the sub-engines
// are handed the same address as before for the same input: the host is
// resolved as net.Listen resolves it, the socket gets the address family
// net.Listen would give it (a wildcard host is dual-stack IPv6, "[::]:PORT",
// wherever Go would make it so; see listenFamily), and the string is built
// from the address the kernel reports, as net.Listener.Addr builds it.
//
// On an error the address is returned unchanged with no socket. The second
// hold sets every option net.Listen sets, SO_REUSEPORT besides, and binds the
// same address in the same family; setting either option only ever makes the
// kernel's bind check more permissive. So when neither hold can be bound,
// net.Listen could not bind the address either, and the bind-and-close
// version handed the sub-engines the unchanged address in that case too. The
// start engine then binds that literal address at Listen, and its family can
// differ from the one tried here: for a wildcard host ("" or 0.0.0.0) the
// sub-engines bind IPv4 0.0.0.0:PORT, while net.Listen and both holds try
// the dual-stack [::]:PORT. So the start engine's bind either fails with its
// own diagnostics or, beside an IPV6_V6ONLY socket on [::]:PORT that set no
// reuse option, succeeds with no hold taken, the New-to-Listen gap as open
// as before celeris#616.
func holdPort(addr string) (string, *os.File, bool, error) {
	ta, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return addr, nil, false, err
	}
	family := listenFamily(ta.IP)
	resolved, hold, err := bindHold(family, ta, false)
	if err == nil {
		return resolved, hold, true, nil
	}
	if !errors.Is(err, unix.EADDRINUSE) {
		return addr, nil, false, err
	}
	resolved, hold, sharedErr := bindHold(family, ta, true)
	if sharedErr != nil {
		return addr, nil, false, fmt.Errorf("%w; with SO_REUSEADDR as well: %w", err, sharedErr)
	}
	return resolved, hold, false, nil
}

// bindHold makes one hold socket for holdPort: bound to ta in family with
// SO_REUSEPORT, and with SO_REUSEADDR as well when reuseAddr is set; never
// listening. It returns the address the kernel bound, as net.Listener.Addr
// spells it.
func bindHold(family int, ta *net.TCPAddr, reuseAddr bool) (string, *os.File, error) {
	fd, err := unix.Socket(family, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return "", nil, fmt.Errorf("socket: %w", err)
	}
	fail := func(step string, err error) (string, *os.File, error) {
		_ = unix.Close(fd)
		return "", nil, fmt.Errorf("%s: %w", step, err)
	}

	var sa unix.Sockaddr
	if family == unix.AF_INET {
		sa4 := &unix.SockaddrInet4{Port: ta.Port}
		if ip4 := ta.IP.To4(); ip4 != nil {
			copy(sa4.Addr[:], ip4)
		}
		sa = sa4
	} else {
		// net.Listen clears IPV6_V6ONLY on every IPv6 TCP listener, which is
		// what makes a wildcard one dual-stack.
		if err := unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, 0); err != nil {
			return fail("setsockopt IPV6_V6ONLY", err)
		}
		sa6 := &unix.SockaddrInet6{Port: ta.Port, ZoneId: zoneIndex(ta.Zone)}
		if ip6 := ta.IP.To16(); ip6 != nil && !ta.IP.IsUnspecified() {
			copy(sa6.Addr[:], ip6)
		}
		sa = sa6
	}

	if reuseAddr {
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
			return fail("setsockopt SO_REUSEADDR", err)
		}
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
		return fail("setsockopt SO_REUSEPORT", err)
	}
	if err := unix.Bind(fd, sa); err != nil {
		return fail("bind", err)
	}
	bound, err := unix.Getsockname(fd)
	if err != nil {
		return fail("getsockname", err)
	}
	var resolved string
	switch b := bound.(type) {
	case *unix.SockaddrInet4:
		resolved = (&net.TCPAddr{IP: net.IP(b.Addr[:]), Port: b.Port}).String()
	case *unix.SockaddrInet6:
		resolved = (&net.TCPAddr{IP: net.IP(b.Addr[:]), Port: b.Port, Zone: zoneName(b.ZoneId)}).String()
	default:
		return fail("getsockname", fmt.Errorf("unexpected address type %T", bound))
	}

	// An *os.File so that closing it is idempotent and an engine that is
	// dropped without Listen or Shutdown still gives the port back when it
	// is collected. The descriptor is blocking, so os.NewFile does not
	// register it with the runtime poller.
	return resolved, os.NewFile(uintptr(fd), "celeris-adaptive-port-hold"), nil
}

// listenFamily is the address family net.Listen("tcp", ...) gives a listener
// on ip (favoriteAddrFamily in package net): a wildcard host is IPv6, and
// dual-stack, when this host can use IPv4-mapped IPv6 addresses or has no
// IPv4 at all, and otherwise has the family of the address it was written
// as; a named address has its own family.
func listenFamily(ip net.IP) int {
	if ip == nil || ip.IsUnspecified() {
		if stack := ipStack(); stack.ipv4Mapped || !stack.ipv4 {
			return unix.AF_INET6
		}
		if len(ip) == net.IPv6len && ip.To4() == nil {
			return unix.AF_INET6 // "::"
		}
		return unix.AF_INET
	}
	if ip.To4() != nil {
		return unix.AF_INET
	}
	return unix.AF_INET6
}

type ipStackCaps struct{ ipv4, ipv4Mapped bool }

// ipStack probes, once, what package net probes before it picks a wildcard
// listener's family (ipStackCapabilities.probe): whether an IPv4 socket can be
// made, and whether an IPv6 socket with IPV6_V6ONLY cleared can bind an
// IPv4-mapped address. A bind refused by a security policy (EPERM, EACCES)
// still counts as support, as it does there.
var ipStack = sync.OnceValue(func() (s ipStackCaps) {
	if fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0); err == nil {
		_ = unix.Close(fd)
		s.ipv4 = true
	}
	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return s
	}
	defer func() { _ = unix.Close(fd) }()
	_ = unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, 0)
	sa := &unix.SockaddrInet6{}
	copy(sa.Addr[:], net.IPv4(127, 0, 0, 1).To16())
	err = unix.Bind(fd, sa)
	s.ipv4Mapped = err == nil || errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES)
	return s
})

// zoneIndex maps an IPv6 zone (an interface name, or a number) to the scope
// id a sockaddr carries; 0 when there is none or it is unknown.
func zoneIndex(zone string) uint32 {
	if zone == "" {
		return 0
	}
	if ifi, err := net.InterfaceByName(zone); err == nil {
		return uint32(ifi.Index)
	}
	if n, err := strconv.ParseUint(zone, 10, 32); err == nil {
		return uint32(n)
	}
	return 0
}

// zoneName is the inverse, as net.Listener.Addr spells it: the interface
// name, or the number when no interface has it.
func zoneName(id uint32) string {
	if id == 0 {
		return ""
	}
	if ifi, err := net.InterfaceByIndex(int(id)); err == nil {
		return ifi.Name
	}
	return strconv.FormatUint(uint64(id), 10)
}
