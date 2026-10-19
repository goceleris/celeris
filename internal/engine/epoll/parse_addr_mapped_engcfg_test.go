//go:build linux

package epoll

import (
	"net"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// Checklist #885 item 44 (planned as 43). parseAddr read an IPv4 host with an
// octet loop beside the net.ParseIP it already calls, so an IPv4-mapped IPv6
// host ("[::ffff:1.2.3.4]:80"), which Go and the std engine accept and bind
// as the IPv4 address, was refused by the loop. A host is now what
// net.ParseIP says it is: To4 gives the IPv4 socket address, anything else
// that parses the IPv6 one, and what does not parse is an error.
func TestParseAddrReadsAnAddressAsNetParseIPDoes(t *testing.T) {
	v4 := func(port int, a, b, c, d byte) unix.Sockaddr {
		return &unix.SockaddrInet4{Port: port, Addr: [4]byte{a, b, c, d}}
	}
	v6 := func(port int, ip string) unix.Sockaddr {
		sa := &unix.SockaddrInet6{Port: port}
		copy(sa.Addr[:], net.ParseIP(ip).To16())
		return sa
	}
	for _, tc := range []struct {
		addr string
		want unix.Sockaddr // nil: an error
	}{
		{"[::ffff:1.2.3.4]:80", v4(80, 1, 2, 3, 4)},
		{"[::ffff:127.0.0.1]:0", v4(0, 127, 0, 0, 1)},
		{"[::ffff:7f00:1]:8080", v4(8080, 127, 0, 0, 1)},
		{"1.2.3.4:80", v4(80, 1, 2, 3, 4)},
		{"255.255.255.255:65535", v4(65535, 255, 255, 255, 255)},
		{":80", v4(80, 0, 0, 0, 0)},
		{"0.0.0.0:80", v4(80, 0, 0, 0, 0)},
		{"[::]:80", &unix.SockaddrInet6{Port: 80}},
		{"[::1]:80", v6(80, "::1")},
		{"[2001:db8::1]:443", v6(443, "2001:db8::1")},
		// The shorthands inet_aton reads stay an error (checklist #885 item
		// 44: PR #992 made them one on purpose), as do hosts ParseIP refuses.
		{"10:80", nil},
		{"127.1:80", nil},
		{"1.2.3:80", nil},
		{"0127.0.0.1:80", nil},
		{"300.1.1.1:80", nil},
		{"1.2.3.4.5:80", nil},
		// KNOWN GAP, not a goal: std binds a zoned address (net.ParseIP, which
		// this reads, rejects the zone); tracked as checklist #885 item 54.
		{"[fe80::1%eth0]:80", nil},
		{"localhost:80", nil},
	} {
		got, err := parseAddr(tc.addr)
		if tc.want == nil {
			if err == nil {
				t.Errorf("parseAddr(%q) = %v, want an error", tc.addr, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseAddr(%q): %v, want %v", tc.addr, err, tc.want)
			continue
		}
		if !sockaddrEqual(got, tc.want) {
			t.Errorf("parseAddr(%q) = %v, want %v", tc.addr, got, tc.want)
		}
		// std's reading of the same literal: the same IP and port.
		std, serr := net.ResolveTCPAddr("tcp", tc.addr)
		if serr != nil {
			t.Errorf("std refuses %q, which the table says is an address: %v", tc.addr, serr)
			continue
		}
		var ip net.IP
		var port int
		switch sa := got.(type) {
		case *unix.SockaddrInet4:
			ip, port = net.IP(sa.Addr[:]), sa.Port
		case *unix.SockaddrInet6:
			ip, port = net.IP(sa.Addr[:]), sa.Port
		}
		if tc.addr != ":80" && !ip.Equal(std.IP) || port != std.Port {
			t.Errorf("parseAddr(%q) = %v:%d, std reads %v:%d", tc.addr, ip, port, std.IP, std.Port)
		}
	}
}

func sockaddrEqual(a, b unix.Sockaddr) bool {
	switch x := a.(type) {
	case *unix.SockaddrInet4:
		y, ok := b.(*unix.SockaddrInet4)
		return ok && *x == *y
	case *unix.SockaddrInet6:
		y, ok := b.(*unix.SockaddrInet6)
		return ok && *x == *y
	}
	return false
}

// TestListenOnAnIPv4MappedHostServes: Listen (and the port 0 resolution before
// it, bindiag.ResolvePort0 over parseAddr) takes the mapped host through to a
// bound IPv4 socket that serves a request; the address it publishes is the
// IPv4 one, as net.Listen("tcp", "[::ffff:127.0.0.1]:0") gives.
func TestListenOnAnIPv4MappedHostServes(t *testing.T) {
	r := start895(t, "[::ffff:127.0.0.1]:0", 2, false)
	addr := r.e.Addr().String()
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Errorf("Addr() = %q, want the IPv4 address 127.0.0.1:port", addr)
	}
	get895(t, addr)
}
