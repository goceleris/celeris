//go:build linux

package bindiag

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

// parse895 stands in for the engines' parseAddr: ":P" and "0.0.0.0:P" are
// IPv4 wildcard, "[::]:P" IPv6 wildcard, "a.b.c.d:P" a named IPv4 address.
func parse895(addr string) (unix.Sockaddr, error) {
	host, ps, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(ps)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host)
	switch {
	case host == "" || host == "0.0.0.0":
		return &unix.SockaddrInet4{Port: port}, nil
	case ip == nil:
		return nil, fmt.Errorf("invalid addr: %s", addr)
	case ip.To4() == nil:
		sa := &unix.SockaddrInet6{Port: port}
		copy(sa.Addr[:], ip.To16())
		return sa, nil
	}
	sa := &unix.SockaddrInet4{Port: port}
	copy(sa.Addr[:], ip.To4())
	return sa, nil
}

func TestWithPort895(t *testing.T) {
	for _, tc := range []struct {
		addr string
		port int
		want string
	}{
		{"127.0.0.1:0", 4242, "127.0.0.1:4242"},
		{":0", 4242, ":4242"},
		{"0.0.0.0:0", 4242, "0.0.0.0:4242"},
		{"[::]:0", 4242, "[::]:4242"},
		{"[::1]:0", 4242, "[::1]:4242"},
		{"0", 4242, ":4242"},
		{"127.0.0.1:", 4242, "127.0.0.1:4242"},
	} {
		if got := WithPort(tc.addr, tc.port); got != tc.want {
			t.Errorf("WithPort(%q, %d) = %q, want %q", tc.addr, tc.port, got, tc.want)
		}
	}
}

// TestResolvePort0LeavesOtherAddressesAlone895: a concrete port, or an
// address parse rejects, comes back as it was with no hold.
func TestResolvePort0LeavesOtherAddressesAlone895(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8080", ":9", "not an address", "999.1.1.1:0x"} {
		got, hold, err := ResolvePort0(addr, parse895)
		if got != addr || hold != nil || err != nil {
			t.Errorf("ResolvePort0(%q) = %q, %v, %v; want the address unchanged, no hold, no error", addr, got, hold, err)
		}
	}
}

// TestResolvePort0HoldsTheChosenPort895: the hold pins the port it chose. A
// socket that binds it the way net.Listen does is refused; one that binds it
// the way the engines do (SO_REUSEPORT) gets it and can listen, and the hold
// is not in the way of any of them; and closing the hold gives the port back.
func TestResolvePort0HoldsTheChosenPort895(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:0", ":0", "0.0.0.0:0"} {
		t.Run(addr, func(t *testing.T) {
			resolved, hold, err := ResolvePort0(addr, parse895)
			if err != nil || hold == nil {
				t.Fatalf("ResolvePort0(%q) = %q, %v, %v", addr, resolved, hold, err)
			}
			defer func() { _ = hold.Close() }()
			_, ps, err := net.SplitHostPort(resolved)
			if err != nil || ps == "0" {
				t.Fatalf("resolved address %q has no concrete port: %v", resolved, err)
			}
			if ln, err := net.Listen("tcp", resolved); err == nil {
				_ = ln.Close()
				t.Fatalf("a plain net.Listen on %s succeeded while the hold was held", resolved)
			} else if !errors.Is(err, unix.EADDRINUSE) {
				t.Fatalf("a plain net.Listen on %s: %v, want EADDRINUSE", resolved, err)
			}
			var lfds []int
			for range 3 {
				sa, _ := parse895(resolved)
				fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
				if err != nil {
					t.Fatal(err)
				}
				lfds = append(lfds, fd)
				_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
				_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
				if err := unix.Bind(fd, sa); err != nil {
					t.Fatalf("an engine-style bind beside the hold: %v", err)
				}
				if err := unix.Listen(fd, 16); err != nil {
					t.Fatalf("listen beside the hold: %v", err)
				}
			}
			for _, fd := range lfds {
				_ = unix.Close(fd)
			}
			_ = hold.Close()
			if err := hold.Close(); err == nil {
				t.Log("a second Close of the hold reported no error")
			}
			ln, err := net.Listen("tcp", resolved)
			if err != nil {
				t.Fatalf("net.Listen on %s after the hold was closed: %v", resolved, err)
			}
			_ = ln.Close()
		})
	}
}

// TestResolvePort0ErrorsWhenItCannotHold895: an address this host cannot
// bind is an error, not a silent fall back to per-loop binds.
func TestResolvePort0ErrorsWhenItCannotHold895(t *testing.T) {
	got, hold, err := ResolvePort0("203.0.113.77:0", parse895)
	if err == nil || hold != nil {
		if hold != nil {
			_ = hold.Close()
		}
		t.Fatalf("ResolvePort0 of an address this host does not own = %q, %v, %v; want an error", got, hold, err)
	}
	if !errors.Is(err, unix.EADDRNOTAVAIL) {
		t.Fatalf("error = %v, want EADDRNOTAVAIL", err)
	}
	t.Logf("error: %v", err)
}
