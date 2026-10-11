//go:build linux

package adaptive

import (
	"errors"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
)

// celeris#895 and #896 through the adaptive engine.
//
// #895: the standalone engines decided a port 0 per worker; adaptive decides
// it once in New (celeris#616), so it never had the defect. The test pins that
// it stays so after the standalone engines learned to resolve the port
// themselves: the concrete address New hands them is left alone.
//
// #896: an io_uring worker used to leave its listen socket bound until the
// kernel tore its ring down, ~2 ms after Listen returned, and an adaptive
// engine started on io_uring has those workers. The port must be free the
// moment Listen has returned and the engine is shut down.

// listeningPorts895 is the set of ports this process's LISTEN sockets are
// bound to, from /proc/self/fd, with how many sockets each has.
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
		switch a := mustGetsockname895(fd).(type) {
		case *unix.SockaddrInet4:
			out[a.Port]++
		case *unix.SockaddrInet6:
			out[a.Port]++
		}
	}
	return out
}

func mustGetsockname895(fd int) unix.Sockaddr {
	sa, _ := unix.Getsockname(fd)
	return sa
}

// TestAdaptivePort0ListensOnOnePort895: adaptive on 127.0.0.1:0, whose start
// engine runs several workers, listens on the one port it published. Epoll is
// the start engine, so the test never depends on io_uring being usable.
func TestAdaptivePort0ListensOnOnePort895(t *testing.T) {
	e, _, _ := newGapEngine(t, "epoll", engine.Epoll)
	stop, err := startAndServe(t, e)
	if err != nil {
		_ = e.Shutdown(t.Context())
		t.Fatalf("adaptive Listen: %v", err)
	}
	defer stop()
	want := e.Addr().(*net.TCPAddr).Port
	got := listeningPorts895(t)
	var ports []int
	for p := range got {
		ports = append(ports, p)
	}
	sort.Ints(ports)
	t.Logf("Addr() port %d; LISTEN sockets by port: %v", want, got)
	if len(ports) != 1 || ports[0] != want {
		t.Fatalf("adaptive listens on ports %v, want only the published %d", ports, want)
	}
	if got[want] < 2 {
		t.Fatalf("%d listen sockets on port %d, want one per worker (at least 2)", got[want], want)
	}
}

// TestAdaptiveStopReleasesThePortAtOnce896: an adaptive engine started on
// io_uring is stopped; a plain bind (SO_REUSEADDR, no SO_REUSEPORT) made once
// the instant its Listen has returned and it is shut down must succeed, 50
// times. Before the fix it was refused for ~2 ms (the 616 tests wait up to 2 s
// for it, free_after_stop_in=).
func TestAdaptiveStopReleasesThePortAtOnce896(t *testing.T) {
	const n = 50
	busy := 0
	var slowest time.Duration
	for i := range n {
		e, addr, _ := newGapEngine(t, "iouring", engine.IOUring)
		var stop func()
		var lerr error
		for dl := time.Now().Add(10 * time.Second); time.Now().Before(dl); {
			stop, lerr = startAndServe(t, e)
			if lerr == nil || !strings.Contains(lerr.Error(), "cannot allocate memory") {
				break
			}
			// The previous iteration's rings are uncharged asynchronously.
			_ = e.Shutdown(t.Context())
			time.Sleep(5 * time.Millisecond)
			e, addr, _ = newGapEngine(t, "iouring", engine.IOUring)
		}
		if lerr != nil {
			_ = e.Shutdown(t.Context())
			t.Fatalf("iteration %d: adaptive Listen: %v", i, lerr)
		}
		stop()
		returned := time.Now()
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			_ = ln.Close()
			continue
		}
		if !errors.Is(err, unix.EADDRINUSE) {
			t.Fatalf("iteration %d: plain bind: %v", i, err)
		}
		busy++
		for time.Since(returned) < time.Second {
			if ln, err := net.Listen("tcp", addr); err == nil {
				_ = ln.Close()
				break
			}
			time.Sleep(50 * time.Microsecond)
		}
		slowest = max(slowest, time.Since(returned))
	}
	t.Logf("adaptive on io_uring: %d stops, port still taken at return in %d (%.0f%%), slowest to come free %v",
		n, busy, 100*float64(busy)/float64(n), slowest)
	if busy != 0 {
		t.Fatalf("an adaptive engine on io_uring returned with its port still taken in %d of %d stops", busy, n)
	}
}
