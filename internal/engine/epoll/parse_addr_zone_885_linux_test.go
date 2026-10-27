//go:build linux

package epoll

import (
	"net"
	"os"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// celeris#885 item 54. parseAddr read the host with net.ParseIP, which rejects
// a zone, so a zoned IPv6 bind address ("[fe80::1234%dz0]:0"), which net.Listen
// and so the std engine bind, made both native engines fail to start on the
// same Config.Addr. The host's zone is now read as net.Listen reads it: the
// index of the interface of that name, else the zone as a number, else 0 (the
// bind then fails for a link-local address, as std's does). An IPv4-mapped host
// is bound as IPv4 and its zone ignored, as std does.

// zoneID885 is std's rule for a zone (net.zoneToInt): the interface's index,
// else the number, else 0.
func zoneID885(zone string) uint32 {
	if zone == "" {
		return 0
	}
	if ifi, err := net.InterfaceByName(zone); err == nil {
		return uint32(ifi.Index)
	}
	n, err := strconv.ParseUint(zone, 10, 32)
	if err != nil {
		return 0
	}
	return uint32(n)
}

func TestParseAddrReadsAZonedIPv6Host885(t *testing.T) {
	lo, err := net.InterfaceByName("lo")
	if err != nil {
		t.Skipf("no lo interface: %v", err)
	}
	v6 := func(port int, ip string, zone uint32) unix.Sockaddr {
		sa := &unix.SockaddrInet6{Port: port, ZoneId: zone}
		copy(sa.Addr[:], net.ParseIP(ip).To16())
		return sa
	}
	for _, tc := range []struct {
		addr string
		want unix.Sockaddr // nil: an error
	}{
		{"[fe80::1%lo]:80", v6(80, "fe80::1", uint32(lo.Index))},
		{"[fe80::1234%lo]:0", v6(0, "fe80::1234", uint32(lo.Index))},
		{"[fe80::1%" + strconv.Itoa(lo.Index) + "]:8080", v6(8080, "fe80::1", uint32(lo.Index))},
		{"[fe80::1%7]:80", v6(80, "fe80::1", 7)},
		// An interface that does not exist and is not a number is scope 0, as
		// std reads it: the kernel then refuses the bind of a link-local
		// address, and a global one binds.
		{"[fe80::1%nosuchif0]:80", v6(80, "fe80::1", 0)},
		{"[2001:db8::1%lo]:443", v6(443, "2001:db8::1", uint32(lo.Index))},
		{"[::%lo]:80", v6(80, "::", uint32(lo.Index))},
		// Everything after the first % is the zone, so this one names no interface.
		{"[fe80::1%lo%lo]:80", v6(80, "fe80::1", 0)},
		// A mapped IPv4 address is IPv4 for std, and a zone means nothing to it.
		{"[::ffff:1.2.3.4%lo]:80", &unix.SockaddrInet4{Port: 80, Addr: [4]byte{1, 2, 3, 4}}},
		// Not an address (std refuses them too): no zone on IPv4, an empty zone, a
		// zone and no host.
		{"1.2.3.4%lo:80", nil},
		{"[1.2.3.4%lo]:80", nil},
		{"[fe80::1%]:80", nil},
		{"[%lo]:80", nil},
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
		// std's reading of the same literal: the same IP, port and zone.
		std, serr := net.ResolveTCPAddr("tcp", tc.addr)
		if serr != nil {
			t.Errorf("std refuses %q, which the table says is an address: %v", tc.addr, serr)
			continue
		}
		if sa6, ok := got.(*unix.SockaddrInet6); ok {
			if !net.IP(sa6.Addr[:]).Equal(std.IP) || sa6.Port != std.Port || sa6.ZoneId != zoneID885(std.Zone) {
				t.Errorf("parseAddr(%q) = %v:%d scope %d, std reads %v:%d zone %q (scope %d)",
					tc.addr, net.IP(sa6.Addr[:]), sa6.Port, sa6.ZoneId, std.IP, std.Port, std.Zone, zoneID885(std.Zone))
			}
		}
	}
}

// zonedTarget885 returns a link-local address of this host with its zone, as
// "fe80::1234%dz0": the first up, non-loopback interface that has one. Without
// one the test skips, and fails when CELERIS_REQUIRE_ZONED_PEER=1 (a lane's
// container gets a dummy link, "ip link add dz0 type dummy; ip -6 addr add
// fe80::1234/64 dev dz0 nodad", with CAP_NET_ADMIN and
// net.ipv6.conf.all.disable_ipv6=0).
func zonedTarget885(t *testing.T) string {
	t.Helper()
	ifs, err := net.Interfaces()
	if err != nil {
		t.Fatalf("net.Interfaces: %v", err)
	}
	for _, ifi := range ifs {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() == nil && ipn.IP.IsLinkLocalUnicast() {
				return ipn.IP.String() + "%" + ifi.Name
			}
		}
	}
	const env = "CELERIS_REQUIRE_ZONED_PEER"
	if os.Getenv(env) == "1" {
		t.Fatalf("no up, non-loopback interface has an fe80:: address, and %s=1 forbids skipping", env)
	}
	t.Skip("no up, non-loopback interface has an fe80:: address")
	return ""
}

// TestListenOnAZonedLinkLocalServes885: Listen on a zoned link-local address
// binds it, serves a request on it, and reports the address as net.Listen does
// ("[fe80::1234%dz0]:port").
func TestListenOnAZonedLinkLocalServes885(t *testing.T) {
	host := zonedTarget885(t)
	std, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatalf("std oracle: net.Listen(%q): %v", host, err)
	}
	_ = std.Close()
	r := start895(t, net.JoinHostPort(host, "0"), 2, false)
	addr := r.e.Addr().String()
	if !strings.HasPrefix(addr, "["+host+"]:") {
		t.Errorf("Addr() = %q, want the zoned address [%s]:port, as net.Listen reports it", addr, host)
	}
	get895(t, addr)
}
