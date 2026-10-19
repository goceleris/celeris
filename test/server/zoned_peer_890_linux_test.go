//go:build linux

package celeris_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// zonedTarget890 returns a link-local address of this host with its zone, as
// "fe80::1234%dz0": the first up, non-loopback interface that has one.
// Without one the test skips, and fails when CELERIS_REQUIRE_ZONED_PEER=1
// (GitHub runners' eth0 has an fe80 address; a lane's container gets a dummy
// link, `ip link add dz0 type dummy; ip -6 addr add fe80::1234/64 dev dz0
// nodad`, with CAP_NET_ADMIN and net.ipv6.conf.all.disable_ipv6=0).
func zonedTarget890(t *testing.T) string {
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

// TestZonedPeerAddressIsStdsOnNativeEngines890 (checklist #890 item 72,
// planned as 70): a peer on a link-local address reads, to the handler
// (Context.RemoteAddr) and to OnConnect, as std's Accept reads it,
// "[fe80::1234%dz0]:port", the zone by the interface's NAME.
//
// Both native engines name the zone with one ioctl on the accepted socket
// (bindiag.SockaddrString(fd, sa), called at the accept site of epoll's loop
// and io_uring's worker). The call site is what no other test reaches: a
// mutant that passes -1 there prints the zone as a number ("%12") for every
// real link-local peer, and the bindiag tests and the table tests of each
// engine, which call SockaddrString directly, all still pass. This test goes
// through a real accept on each engine, with sync and async handlers, and
// takes its expectation from std on the same peer, not from a literal.
func TestZonedPeerAddressIsStdsOnNativeEngines890(t *testing.T) {
	dst := zonedTarget890(t)

	// std's reading of this peer: the zone it prints is the oracle. Only the
	// port differs from the engines' (a connection of its own each).
	sl, err := net.Listen("tcp", "[::]:0")
	if err != nil {
		t.Fatalf("std oracle listen: %v", err)
	}
	t.Cleanup(func() { _ = sl.Close() })
	acc := make(chan string, 1)
	go func() {
		c, err := sl.Accept()
		if err != nil {
			acc <- "ERR " + err.Error()
			return
		}
		acc <- c.RemoteAddr().String()
		_ = c.Close()
	}()
	oc, err := net.DialTimeout("tcp", net.JoinHostPort(dst, fmt.Sprint(sl.Addr().(*net.TCPAddr).Port)), 2*time.Second)
	if err != nil {
		t.Fatalf("std oracle dial %s: %v", dst, err)
	}
	_ = oc.Close()
	stdRemote := <-acc
	stdHost, _, err := net.SplitHostPort(stdRemote)
	if err != nil || !strings.Contains(stdHost, "%") {
		t.Fatalf("std reports the peer as %q (%v): want a host with a zone", stdRemote, err)
	}
	t.Logf("dst %s, std Accept reports the peer as %s", dst, stdRemote)

	for _, arm := range keptArms(t) {
		t.Run(arm.name, func(t *testing.T) {
			var mu sync.Mutex
			var connects []string
			addr, stop := startKeptServerOn(t, "[::]:0", func() *celeris.Server {
				s := celeris.New(celeris.Config{
					Engine:        arm.engine,
					AsyncHandlers: arm.async,
					OnConnect: func(a string) {
						mu.Lock()
						connects = append(connects, a)
						mu.Unlock()
					},
				})
				s.GET("/", func(c *celeris.Context) error { return c.String(http.StatusOK, "%s", c.RemoteAddr()) })
				return s
			})
			defer stop()
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				t.Fatalf("server address %q: %v", addr, err)
			}

			conn, err := net.DialTimeout("tcp", net.JoinHostPort(dst, port), 2*time.Second)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer func() { _ = conn.Close() }()
			_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
			if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"); err != nil {
				t.Fatalf("write: %v", err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()

			want := net.JoinHostPort(stdHost, fmt.Sprint(conn.LocalAddr().(*net.TCPAddr).Port))
			if got := string(body); got != want {
				t.Errorf("Context.RemoteAddr = %q, want %q (std's zone rule)", got, want)
			}
			// The readiness probe of startKeptServerOn dials from [::1] first:
			// the connections with a zone are the one of this test.
			mu.Lock()
			defer mu.Unlock()
			var zoned []string
			for _, a := range connects {
				if strings.Contains(a, "%") {
					zoned = append(zoned, a)
				}
			}
			if len(zoned) != 1 || zoned[0] != want {
				t.Errorf("OnConnect saw %q (zoned %q), want exactly one zoned peer, %q", connects, zoned, want)
			}
		})
	}
}
