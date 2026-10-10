//go:build linux

package iouring

import (
	"net"
	"net/netip"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

// celeris#925: the native engines must format a peer and a bound address the
// way the standard library does. stdPeerString925 is the oracle for a peer:
// net.TCPAddr.String of it, which unmaps an IPv4-mapped address and names a
// zone by its interface, or by its number when no interface has the index.

func stdPeerString925(ip net.IP, port int, zoneID uint32) string {
	zone := ""
	if zoneID != 0 {
		zone = strconv.FormatUint(uint64(zoneID), 10)
		if ifi, err := net.InterfaceByIndex(int(zoneID)); err == nil {
			zone = ifi.Name
		}
	}
	return (&net.TCPAddr{IP: ip, Port: port, Zone: zone}).String()
}

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

// TestSockaddrStringMatchesStd925 feeds raw sockaddr values to the peer
// formatter and compares each with the standard library's rendering.
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
			want := stdPeerString925(tc.ip, tc.port, tc.zone)
			if got := sockaddrString(tc.sa); got != want {
				t.Errorf("sockaddrString = %q, want %q (std)", got, want)
			}
		})
	}
}

// TestBoundAddrMatchesStdListener925 takes the address of a listener the
// standard library bound, and compares it with what boundAddr reports for the
// same socket. A dual-stack or unscoped IPv6 listener must not report a zone
// "0", which std never prints.
func TestBoundAddrMatchesStdListener925(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:0", "[::1]:0", "[::]:0"} {
		t.Run(addr, func(t *testing.T) {
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				t.Skipf("listen %s: %v", addr, err)
			}
			defer func() { _ = ln.Close() }()
			f, err := ln.(*net.TCPListener).File()
			if err != nil {
				t.Fatalf("File: %v", err)
			}
			defer func() { _ = f.Close() }()
			want := ln.Addr().String()
			if got := boundAddr(int(f.Fd())).String(); got != want {
				t.Errorf("boundAddr = %q, want %q (std listener)", got, want)
			}
		})
	}
}
