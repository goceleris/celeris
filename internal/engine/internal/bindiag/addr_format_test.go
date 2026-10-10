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

// loopbackIface925 returns the index and the name of the loopback interface,
// the one real zone id the table can use on any host. The name comes from the
// net.Interfaces entry, not from InterfaceByIndex, for TestZoneLiteral925.
func loopbackIface925(t *testing.T) (uint32, string) {
	t.Helper()
	ifs, err := net.Interfaces()
	if err != nil {
		t.Fatalf("net.Interfaces: %v", err)
	}
	for _, ifi := range ifs {
		if ifi.Flags&net.FlagLoopback != 0 {
			return uint32(ifi.Index), ifi.Name
		}
	}
	t.Fatal("no loopback interface")
	return 0, ""
}

// TestSockaddrStringMatchesStd925 feeds raw sockaddr values to the formatter
// and compares each with the standard library's rendering of the same peer.
// The rows cover IPv4, an IPv4-mapped IPv6 peer (the dual-stack client),
// IPv6 with zone 0, link-local with zone 0, and link-local with a real zone
// id and with an id no interface has.
func TestSockaddrStringMatchesStd925(t *testing.T) {
	lo, _ := loopbackIface925(t)
	fd := ctlSocket925(t)
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
			got := SockaddrString(fd, tc.sa)
			t.Logf("925 probe: %s -> %q", tc.name, got)
			if got != want {
				t.Errorf("sockaddrString = %q, want %q (std)", got, want)
			}
		})
	}
}

// TestZoneLiteral925 pins the zone rule with literal expectations. The table
// above compares with stdPeerString, which calls net.InterfaceByIndex as the
// code under test does, so it cannot catch a rule both sides share. Here a
// scope id that no interface has prints as its number, and the loopback's id
// prints as the name that net.Interfaces gives it.
func TestZoneLiteral925(t *testing.T) {
	lo, loName := loopbackIface925(t)
	v6 := netip.MustParseAddr("fe80::1").As16()
	for _, tc := range []struct {
		name string
		fd   int
		zone uint32
		want string
	}{
		{"unknown-zone-prints-its-number", ctlSocket925(t), 999999, "[fe80::1%999999]:443"},
		{"loopback-zone-prints-its-name", ctlSocket925(t), lo, "[fe80::1%" + loName + "]:443"},
		// The name comes from an ioctl on fd. With no usable fd it cannot be
		// resolved, and the number is printed, as for an unknown interface.
		{"unusable-fd-prints-the-number", -1, lo, "[fe80::1%" + strconv.Itoa(int(lo)) + "]:443"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SockaddrString(tc.fd, &unix.SockaddrInet6{Addr: v6, Port: 443, ZoneId: tc.zone})
			t.Logf("925 probe: zone %d -> %q", tc.zone, got)
			if got != tc.want {
				t.Errorf("SockaddrString = %q, want %q (literal)", got, tc.want)
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

// ctlSocket925 is a socket to resolve scope ids on, as the accepted connection
// is for the accept path.
func ctlSocket925(t *testing.T) int {
	t.Helper()
	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		fd, err = unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	}
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	return fd
}

// TestZoneNameEveryInterface925 resolves the index of every interface of the
// host and expects the name net.Interfaces lists for it (a netlink dump, so
// independent of the ioctl under test), and the number for an index no
// interface has.
func TestZoneNameEveryInterface925(t *testing.T) {
	ifs, err := net.Interfaces()
	if err != nil {
		t.Fatalf("net.Interfaces: %v", err)
	}
	fd := ctlSocket925(t)
	for _, ifi := range ifs {
		if got := zoneName(fd, uint32(ifi.Index)); got != ifi.Name {
			t.Errorf("zoneName(%d) = %q, want %q (net.Interfaces)", ifi.Index, got, ifi.Name)
		}
	}
	t.Logf("925 probe: %d interfaces resolved by index", len(ifs))
	if got := zoneName(fd, 999999); got != "999999" {
		t.Errorf("zoneName(999999) = %q, want %q", got, "999999")
	}
	if got := zoneName(fd, 0); got != "" {
		t.Errorf("zoneName(0) = %q, want empty", got)
	}
}

// TestZoneCostsNoNetlinkDump925 bounds what a link-local peer costs on the
// accept path of an event loop (celeris#925 review). net.InterfaceByIndex is
// an RTM_GETLINK dump of every interface: 45 allocations and 62 KB with 12
// interfaces, 361 allocations and 1.7 MB with 212. The name from a
// SIOCGIFNAME ioctl is one allocation whatever the number of interfaces, so
// this bound holds on any host and fails when the dump comes back.
func TestZoneCostsNoNetlinkDump925(t *testing.T) {
	lo, _ := loopbackIface925(t)
	fd := ctlSocket925(t)
	sa := &unix.SockaddrInet6{Addr: netip.MustParseAddr("fe80::1").As16(), Port: 443, ZoneId: lo}
	sa4 := &unix.SockaddrInet4{Addr: [4]byte{10, 0, 12, 200}, Port: 54321}
	zoned := testing.AllocsPerRun(200, func() { _ = SockaddrString(fd, sa) })
	plain := testing.AllocsPerRun(200, func() { _ = SockaddrString(fd, sa4) })
	t.Logf("925 probe: allocs per call: zoned=%v ipv4=%v", zoned, plain)
	if zoned > 8 {
		t.Errorf("a zoned peer costs %v allocations per call, want at most 8 (a netlink dump costs 45 or more)", zoned)
	}
	if plain > 1 {
		t.Errorf("an IPv4 peer costs %v allocations per call, want at most 1", plain)
	}
}

func BenchmarkSockaddrString925(b *testing.B) {
	lo := uint32(1)
	if ifs, err := net.Interfaces(); err == nil {
		for _, ifi := range ifs {
			if ifi.Flags&net.FlagLoopback != 0 {
				lo = uint32(ifi.Index)
			}
		}
	}
	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		b.Fatalf("socket: %v", err)
	}
	defer func() { _ = unix.Close(fd) }()
	v6 := netip.MustParseAddr("fe80::1").As16()
	mapped := netip.MustParseAddr("::ffff:10.0.12.200").As16()
	for _, tc := range []struct {
		name string
		sa   unix.Sockaddr
	}{
		{"ipv4", &unix.SockaddrInet4{Addr: [4]byte{10, 0, 12, 200}, Port: 54321}},
		{"ipv4-mapped", &unix.SockaddrInet6{Addr: mapped, Port: 54321}},
		{"link-local-zoned", &unix.SockaddrInet6{Addr: v6, Port: 54321, ZoneId: lo}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = SockaddrString(fd, tc.sa)
			}
		})
	}
}
