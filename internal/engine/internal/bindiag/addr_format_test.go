//go:build linux

package bindiag

import (
	"net"
	"net/netip"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

// celeris#925: the address formatting of the native engines and of Format
// must print what the standard library prints for the same peer. stdPeerString
// is that oracle: net.TCPAddr.String of the peer, which unmaps an IPv4-mapped
// address and names a zone by its interface, or by its number when no
// interface has the index (net.Conn's rule, as net/ipsock_posix.go applies it).
func stdPeerString(ip net.IP, port int, zoneID uint32) string {
	zone := ""
	if zoneID != 0 {
		zone = strconv.FormatUint(uint64(zoneID), 10)
		if ifi, err := net.InterfaceByIndex(int(zoneID)); err == nil {
			zone = ifi.Name
		}
	}
	return (&net.TCPAddr{IP: ip, Port: port, Zone: zone}).String()
}

// loopbackIndex925 returns the interface index of the loopback interface,
// the one real zone id the table can use on any host.
func loopbackIndex925(t *testing.T) uint32 {
	t.Helper()
	ifs, err := net.Interfaces()
	if err != nil {
		t.Fatalf("net.Interfaces: %v", err)
	}
	for _, ifi := range ifs {
		if ifi.Flags&net.FlagLoopback != 0 {
			return uint32(ifi.Index)
		}
	}
	t.Fatal("no loopback interface")
	return 0
}

// TestSockaddrStringMatchesStd925 feeds raw sockaddr values to the formatter
// and compares each with the standard library's rendering of the same peer.
// The rows cover IPv4, an IPv4-mapped IPv6 peer (the dual-stack client),
// IPv6 with zone 0, link-local with zone 0, and link-local with a real zone
// id and with an id no interface has.
func TestSockaddrStringMatchesStd925(t *testing.T) {
	lo := loopbackIndex925(t)
	v6 := func(a string) [16]byte { return netip.MustParseAddr(a).As16() }
	for _, tc := range []struct {
		name string
		sa   unix.Sockaddr
		ip   net.IP
		zone uint32
		port int
	}{
		{"v4", &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}, Port: 41728}, net.IPv4(127, 0, 0, 1), 0, 41728},
		{"v4-mapped-loopback", &unix.SockaddrInet6{Addr: v6("::ffff:127.0.0.1"), Port: 41728}, net.ParseIP("::ffff:127.0.0.1"), 0, 41728},
		{"v4-mapped-private", &unix.SockaddrInet6{Addr: v6("::ffff:10.1.2.3"), Port: 443}, net.ParseIP("::ffff:10.1.2.3"), 0, 443},
		{"v6-loopback", &unix.SockaddrInet6{Addr: v6("::1"), Port: 80}, net.ParseIP("::1"), 0, 80},
		{"v6-unspecified", &unix.SockaddrInet6{Addr: v6("::"), Port: 8080}, net.ParseIP("::"), 0, 8080},
		{"v6-global", &unix.SockaddrInet6{Addr: v6("2001:db8::1"), Port: 9}, net.ParseIP("2001:db8::1"), 0, 9},
		{"link-local-zone0", &unix.SockaddrInet6{Addr: v6("fe80::1"), Port: 443}, net.ParseIP("fe80::1"), 0, 443},
		{"link-local-loopback-zone", &unix.SockaddrInet6{Addr: v6("fe80::1"), Port: 443, ZoneId: lo}, net.ParseIP("fe80::1"), lo, 443},
		{"link-local-unknown-zone", &unix.SockaddrInet6{Addr: v6("fe80::1"), Port: 443, ZoneId: 999999}, net.ParseIP("fe80::1"), 999999, 443},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := stdPeerString(tc.ip, tc.port, tc.zone)
			if got := sockaddrString(tc.sa); got != want {
				t.Errorf("sockaddrString = %q, want %q (std)", got, want)
			}
		})
	}
}

// TestFormatPrintsIPv6Address925 pins the bind diagnostic: the address of an
// IPv6 bind is printed as an address, not as the [16]byte array it printed
// before celeris#925.
func TestFormatPrintsIPv6Address925(t *testing.T) {
	sa := &unix.SockaddrInet6{Addr: netip.MustParseAddr("::1").As16(), Port: 8080}
	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	defer func() { _ = unix.Close(fd) }()
	got := Format(fd, sa)
	want := "addr=[::1]:8080 "
	if len(got) < len(want) || got[:len(want)] != want {
		t.Errorf("Format() = %q, want it to start with %q", got, want)
	}
}
