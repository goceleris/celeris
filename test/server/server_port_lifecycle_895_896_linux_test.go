//go:build linux

package celeris_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris"
)

// celeris#895 and #896 as a user of Server meets them.
//
// #895: Server{Addr: "127.0.0.1:0", Engine: Epoll or IOUring, Workers: N} bound
// a port per worker, of which Addr() reported one and only that worker was
// given a connection. The docs recommend ":0" for tests.
//
// #896: Server.Shutdown returned while an io_uring worker's listen socket
// still held the port for ~2 ms, and a listener that bound the port right
// after (net.Listen, no SO_REUSEPORT) got EADDRINUSE.
//
// Neither test skips: an engine that cannot start fails the test, with a start
// that fails only on ring ENOMEM retried for 10 s (the kernel uncharges closed
// rings asynchronously, and at CI's 8 MiB a start right after another can fail
// with nothing leaked).

func listeningPorts895(t *testing.T) map[int]int {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("read /proc/self/fd: %v", err)
	}
	out := map[int]int{}
	for _, ent := range ents {
		fd, err := strconv.Atoi(ent.Name())
		if err != nil {
			continue
		}
		if link, err := os.Readlink("/proc/self/fd/" + ent.Name()); err != nil || !strings.HasPrefix(link, "socket:[") {
			continue
		}
		if v, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ACCEPTCONN); err != nil || v != 1 {
			continue
		}
		sa, _ := unix.Getsockname(fd)
		switch a := sa.(type) {
		case *unix.SockaddrInet4:
			out[a.Port]++
		case *unix.SockaddrInet6:
			out[a.Port]++
		}
	}
	return out
}

// startServer895 starts a server and returns it with its stop func once Addr()
// is published, retrying a start that fails only on ring ENOMEM.
func startServer895(t *testing.T, mk func() *celeris.Server) (*celeris.Server, func()) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s := mk()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.StartWithContext(ctx) }()
		var err error
		for end := time.Now().Add(15 * time.Second); s.Addr() == nil; {
			select {
			case err = <-done:
				if err == nil {
					err = errors.New("start returned before the server published an address")
				}
			case <-time.After(2 * time.Millisecond):
				if time.Now().After(end) {
					err = errors.New("no address within 15s")
				}
			}
			if err != nil {
				break
			}
		}
		if err == nil {
			return s, func() { _ = s.Shutdown(context.Background()); cancel(); <-done }
		}
		cancel()
		if strings.Contains(err.Error(), "cannot allocate memory") && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
			continue
		}
		t.Fatalf("the server did not start: %v", err)
	}
}

// TestServerPort0ListensOnOnePort895: the sockets a started server adds to the
// process are all on the port Addr() reports, and a request to it is served.
func TestServerPort0ListensOnOnePort895(t *testing.T) {
	for _, eng := range []celeris.EngineType{celeris.Std, celeris.Epoll, celeris.IOUring} {
		t.Run(eng.String(), func(t *testing.T) {
			before := listeningPorts895(t)
			s, stop := startServer895(t, func() *celeris.Server {
				s := celeris.New(celeris.Config{Addr: "127.0.0.1:0", Engine: eng, Workers: 4})
				s.GET("/ping", func(c *celeris.Context) error { return c.String(http.StatusOK, "ok") })
				return s
			})
			defer stop()
			want := s.Addr().(*net.TCPAddr).Port
			after := listeningPorts895(t)
			added := map[int]int{}
			for p, n := range after {
				if d := n - before[p]; d > 0 {
					added[p] = d
				}
			}
			var ports []int
			for p := range added {
				ports = append(ports, p)
			}
			sort.Ints(ports)
			t.Logf("%s: Addr() port %d; LISTEN sockets the server added, by port: %v", eng, want, added)
			if len(ports) != 1 || ports[0] != want {
				t.Fatalf("%s: the server listens on ports %v, want only the published %d", eng, ports, want)
			}
			resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + s.Addr().String() + "/ping")
			if err != nil {
				t.Fatalf("GET /ping: %v", err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET /ping = %d, want 200", resp.StatusCode)
			}
		})
	}
}

// TestServerShutdownFreesThePortAtOnce896: Server.Shutdown on an io_uring
// server returns with the port free: a plain bind made once, the instant it
// returns, succeeds, 20 times (std and epoll run as the controls).
func TestServerShutdownFreesThePortAtOnce896(t *testing.T) {
	const n = 20
	for _, eng := range []celeris.EngineType{celeris.Std, celeris.Epoll, celeris.IOUring} {
		t.Run(eng.String(), func(t *testing.T) {
			busy := 0
			for i := range n {
				probeLn, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatalf("probe listen: %v", err)
				}
				addr := probeLn.Addr().String()
				_ = probeLn.Close()
				_, stop := startServer895(t, func() *celeris.Server {
					return celeris.New(celeris.Config{Addr: addr, Engine: eng, Workers: 2})
				})
				stop()
				ln, err := net.Listen("tcp", addr)
				if err != nil {
					if !errors.Is(err, unix.EADDRINUSE) {
						t.Fatalf("iteration %d: plain bind: %v", i, err)
					}
					busy++
					continue
				}
				_ = ln.Close()
			}
			t.Logf("%s: %d Shutdowns, port still taken when Shutdown returned in %d", eng, n, busy)
			if busy != 0 {
				t.Fatalf("%s: Server.Shutdown returned with its port still taken in %d of %d runs", eng, busy, n)
			}
		})
	}
}
