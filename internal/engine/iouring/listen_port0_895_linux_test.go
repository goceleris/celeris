//go:build linux

package iouring

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#895. Every worker used to bind Config.Addr itself, so with port 0
// and more than one worker each bind(2) picked its own ephemeral port: the
// engine listened on as many ports as it had workers, Addr() reported one of
// them, and only that worker was ever given a connection. The pause/resume
// path had the same defect for any worker count: a worker that re-creates its
// listen socket on ResumeAccept bound Config.Addr again, port 0 again, and so
// moved to a new port nobody was told about.
//
// The oracle reads the kernel's table, not the engine's bookkeeping: the
// ports this process's own sockets are bound to, from /proc/self/fd.
//
// Four workers need memlock for four rings (minMemlockPerWorker each): the
// runner's 8 MiB funds one and capWorkersToMemlock caps the request, so the
// multi-worker arms skip there (skipOrFail656: CI's `iouring` job raises
// memlock and sets CELERIS_REQUIRE_IOURING_WORKERS=1, which turns the skip
// into a failure). The resume arm needs one worker and runs at 8 MiB too.

// sockets895 maps port to the number of this process's TCP sockets bound to
// it, and listening to the number of those that listen. Only sockets whose
// local address has a port are counted.
type sockets895 struct {
	bound, listening map[int]int
}

func census895(t *testing.T) sockets895 {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("read /proc/self/fd: %v", err)
	}
	s := sockets895{bound: map[int]int{}, listening: map[int]int{}}
	for _, ent := range ents {
		fd, err := strconv.Atoi(ent.Name())
		if err != nil {
			continue
		}
		link, err := os.Readlink("/proc/self/fd/" + ent.Name())
		if err != nil || !strings.HasPrefix(link, "socket:[") {
			continue
		}
		if typ, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TYPE); err != nil || typ != unix.SOCK_STREAM {
			continue
		}
		sa, err := unix.Getsockname(fd)
		if err != nil {
			continue
		}
		port := 0
		switch a := sa.(type) {
		case *unix.SockaddrInet4:
			port = a.Port
		case *unix.SockaddrInet6:
			port = a.Port
		}
		if port == 0 {
			continue
		}
		s.bound[port]++
		if v, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ACCEPTCONN); err == nil && v == 1 {
			s.listening[port]++
		}
	}
	return s
}

func ports895(m map[int]int) []int {
	var ps []int
	for p := range m {
		ps = append(ps, p)
	}
	sort.Ints(ps)
	return ps
}

func portOf895(t *testing.T, a net.Addr) int {
	t.Helper()
	if a == nil {
		t.Fatal("engine published no address")
	}
	_, ps, err := net.SplitHostPort(a.String())
	if err != nil {
		t.Fatalf("split %q: %v", a, err)
	}
	p, err := strconv.Atoi(ps)
	if err != nil {
		t.Fatalf("port %q: %v", ps, err)
	}
	return p
}

// needWorkers895 skips the test (fails it under CELERIS_REQUIRE_IOURING_WORKERS=1)
// when RLIMIT_MEMLOCK funds fewer than need io_uring workers. A test with
// subtests calls it before the first t.Run: the CI step that runs this package
// at 8 MiB allows a skipped top-level test by name and cannot allow a subtest.
func needWorkers895(t *testing.T, need int) {
	t.Helper()
	if maxW := MaxWorkersForMemlock(); maxW >= 0 && maxW < need {
		skipOrFail656(t, "RLIMIT_MEMLOCK funds %d io_uring workers, the test needs %d "+
			"(raise it: `ulimit -l unlimited`, or run as root)", maxW, need)
	}
}

type run895 struct {
	e      *Engine
	cancel context.CancelFunc
	done   chan error
}

// stop cancels Listen's context and waits for Listen to return, leaving its
// result in done for the t.Cleanup that waits again.
func (r *run895) stop(t *testing.T) error {
	t.Helper()
	r.cancel()
	select {
	case err := <-r.done:
		r.done <- err
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("Listen did not return within 30 s of its context being cancelled")
		return nil
	}
}

// start895 starts an io_uring engine, asking for the given worker count, on
// addr and waits for its address. The test skips (fails under
// CELERIS_REQUIRE_IOURING_WORKERS=1) when memlock funds fewer than need
// workers; a request above what is funded is capped by the engine. Retrying a start that fails only on ring ENOMEM
// (ring_budget_linux_test.go). disableDefer makes a pause close at once.
func start895(t *testing.T, addr string, workers, need int, disableDefer bool) *run895 {
	t.Helper()
	needWorkers895(t, need)
	var run *run895
	_, _, err := retryRingENOMEM(func() error {
		e, err := New(resource.Config{
			Addr:               addr,
			Engine:             engine.IOUring,
			Protocol:           engine.HTTP1,
			Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
			DisableDeferAccept: disableDefer,
			Resources:          resource.Resources{Workers: workers},
		}, respondingHandler{})
		if err != nil {
			skipOrFail656(t, "New: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- e.Listen(ctx) }()
		for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl); {
			if e.Addr() != nil {
				run = &run895{e: e, cancel: cancel, done: done}
				t.Cleanup(func() { cancel(); <-done })
				return nil
			}
			select {
			case lerr := <-done:
				cancel()
				return lerr
			case <-time.After(time.Millisecond):
			}
		}
		cancel()
		<-done
		t.Fatalf("engine on %s never published an address", addr)
		return nil
	})
	if err != nil {
		if ioUringUnavailable639(err) {
			skipUnlessIOUringUnusable(t, err)
		}
		t.Fatalf("engine start on %s: %v", addr, err)
	}
	return run
}

// get895 asks the engine for "/" and returns the status line's code.
func get895(t *testing.T, addr string) int {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	b, _ := io.ReadAll(c)
	if !strings.HasPrefix(string(b), "HTTP/1.1 200") {
		t.Fatalf("GET %s answered %q, want 200", addr, string(b))
	}
	return 200
}

// TestPort0EveryWorkerBindsTheSamePort895: with four workers and port 0, the
// engine listens on one port, the one Addr() reports, with one socket per
// worker; and a request to that port is served.
func TestPort0EveryWorkerBindsTheSamePort895(t *testing.T) {
	needWorkers895(t, 4)
	for _, tc := range []struct{ name, addr string }{
		{"loopback", "127.0.0.1:0"},
		{"wildcard-empty-host", ":0"},
		{"wildcard-v6-dual-stack", "[::]:0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if strings.HasPrefix(tc.addr, "[") {
				ln, err := net.Listen("tcp", tc.addr)
				if err != nil {
					requireDualStack925(t, err)
				}
				_ = ln.Close()
			}
			const workers = 4
			r := start895(t, tc.addr, workers, workers, false)
			addrPort := portOf895(t, r.e.Addr())
			c := census895(t)
			t.Logf("Addr() = %s; LISTEN sockets by port: %v", r.e.Addr(), c.listening)
			if got := ports895(c.listening); len(got) != 1 || got[0] != addrPort {
				t.Fatalf("the engine listens on ports %v, want only the published %d", got, addrPort)
			}
			if n := c.listening[addrPort]; n != workers {
				t.Fatalf("%d listen sockets on port %d, want one per worker (%d)", n, addrPort, workers)
			}
			if n := c.bound[addrPort]; n != workers+1 {
				t.Fatalf("%d sockets bound to port %d, want %d: one per worker and the engine's port 0 hold", n, addrPort, workers+1)
			}
			get895(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(addrPort)))
		})
	}
}

// TestPort0ResumeKeepsThePort895: a pause closes every worker's listen socket
// and a resume binds them again; they must come back on the port Addr()
// reported, not on a new ephemeral one each. Two workers are requested: one
// is funded at 8 MiB, which is enough for this defect.
func TestPort0ResumeKeepsThePort895(t *testing.T) {
	r := start895(t, "127.0.0.1:0", 2, 1, true)
	r.e.mu.Lock()
	workers := len(r.e.workers)
	r.e.mu.Unlock()
	addrPort := portOf895(t, r.e.Addr())
	if err := r.e.PauseAccept(); err != nil {
		t.Fatalf("PauseAccept: %v", err)
	}
	if n := census895(t).listening[addrPort]; n != 0 {
		t.Fatalf("%d listen sockets on port %d after PauseAccept, want 0", n, addrPort)
	}
	if err := r.e.ResumeAccept(); err != nil {
		t.Fatalf("ResumeAccept: %v", err)
	}
	var c sockets895
	for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl); time.Sleep(5 * time.Millisecond) {
		c = census895(t)
		n := 0
		for _, v := range c.listening {
			n += v
		}
		if n >= workers {
			break
		}
	}
	t.Logf("%d workers; after resume, LISTEN sockets by port: %v (Addr() port %d)", workers, c.listening, addrPort)
	if got := ports895(c.listening); len(got) != 1 || got[0] != addrPort {
		t.Fatalf("after ResumeAccept the workers listen on ports %v, want only the published %d", got, addrPort)
	}
	if n := c.listening[addrPort]; n != workers {
		t.Fatalf("%d listen sockets on port %d after ResumeAccept, want %d", n, addrPort, workers)
	}
	get895(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(addrPort)))
}

// TestPort0PausedPortCannotBeTaken895: once every worker has paused nothing
// listens on the port, and the resume binds that port again, so a socket of
// anyone else that took it in between would leave the worker without a
// listener for good. The engine's hold keeps the port until Listen returns:
// a plain listener (net.Listen: SO_REUSEADDR, no SO_REUSEPORT) is refused
// while the engine is paused, the resume brings every worker back, and the
// port is free once Listen has returned.
func TestPort0PausedPortCannotBeTaken895(t *testing.T) {
	r := start895(t, "127.0.0.1:0", 2, 1, true)
	r.e.mu.Lock()
	workers := len(r.e.workers)
	r.e.mu.Unlock()
	addrPort := portOf895(t, r.e.Addr())
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(addrPort))
	if err := r.e.PauseAccept(); err != nil {
		t.Fatalf("PauseAccept: %v", err)
	}
	if n := census895(t).listening[addrPort]; n != 0 {
		t.Fatalf("%d listen sockets on port %d after PauseAccept, want 0", n, addrPort)
	}
	if ln, err := net.Listen("tcp", addr); err == nil {
		_ = ln.Close()
		t.Fatalf("a plain net.Listen on %s succeeded while every worker was paused: "+
			"the port was free for anyone to take before the resume re-bound it", addr)
	} else if !errors.Is(err, unix.EADDRINUSE) {
		t.Fatalf("net.Listen on %s while paused: %v, want EADDRINUSE", addr, err)
	}
	if err := r.e.ResumeAccept(); err != nil {
		t.Fatalf("ResumeAccept: %v", err)
	}
	var c sockets895
	for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl); time.Sleep(5 * time.Millisecond) {
		if c = census895(t); c.listening[addrPort] >= workers {
			break
		}
	}
	if n := c.listening[addrPort]; n != workers {
		t.Fatalf("%d listen sockets on port %d after ResumeAccept, want %d", n, addrPort, workers)
	}
	get895(t, addr)
	if err := r.stop(t); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if n := census895(t).bound[addrPort]; n != 0 {
		t.Fatalf("%d sockets still bound to port %d after Listen returned, want 0 (the hold included)", n, addrPort)
	}
	// An immediate bind, not bindWithin656: the port is to be free the instant
	// Listen has returned (celeris#896), after a pause and a resume too.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("net.Listen on %s the instant Listen returned: %v", addr, err)
	}
	_ = ln.Close()
}

// TestFixedPortIsBoundAsGivenControl895: the control for the two above. A
// concrete port is bound exactly as it was written, by every worker, and
// Addr() reports it, so nothing here depends on the port-0 handling.
func TestFixedPortIsBoundAsGivenControl895(t *testing.T) {
	addr := freeAddr639(t)
	const workers = 2
	r := start895(t, addr, workers, 1, false)
	r.e.mu.Lock()
	n := len(r.e.workers)
	r.e.mu.Unlock()
	if got := r.e.Addr().String(); got != addr {
		t.Fatalf("Addr() = %s, want %s", got, addr)
	}
	port := portOf895(t, r.e.Addr())
	c := census895(t)
	if got := ports895(c.listening); len(got) != 1 || got[0] != port || c.listening[port] != n {
		t.Fatalf("LISTEN sockets by port %v, want %d on %d", c.listening, n, port)
	}
}

// TestPort0PauseResumeWithManyWorkers895: the pause/resume arm of the hold with
// four workers, twice over (CI ran the arms above with the one worker an 8 MiB
// memlock funds; this one needs four and fails, not skips, under
// CELERIS_REQUIRE_IOURING_WORKERS=1). Each cycle closes four listen sockets and
// binds four again on the one port: a plain listener is refused in between, every
// worker is listening on the published port after the resume, a request is
// served, and the port is free the instant Listen returns.
func TestPort0PauseResumeWithManyWorkers895(t *testing.T) {
	const workers = 4
	r := start895(t, "127.0.0.1:0", workers, workers, true)
	r.e.mu.Lock()
	got := len(r.e.workers)
	r.e.mu.Unlock()
	if got != workers {
		t.Fatalf("the engine runs %d workers, want %d", got, workers)
	}
	addrPort := portOf895(t, r.e.Addr())
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(addrPort))
	for cycle := 1; cycle <= 2; cycle++ {
		if err := r.e.PauseAccept(); err != nil {
			t.Fatalf("cycle %d: PauseAccept: %v", cycle, err)
		}
		if n := census895(t).listening[addrPort]; n != 0 {
			t.Fatalf("cycle %d: %d listen sockets on port %d after PauseAccept, want 0", cycle, n, addrPort)
		}
		if ln, err := net.Listen("tcp", addr); err == nil {
			_ = ln.Close()
			t.Fatalf("cycle %d: a plain net.Listen on %s succeeded while all %d workers were paused", cycle, addr, workers)
		} else if !errors.Is(err, unix.EADDRINUSE) {
			t.Fatalf("cycle %d: net.Listen on %s while paused: %v, want EADDRINUSE", cycle, addr, err)
		}
		if err := r.e.ResumeAccept(); err != nil {
			t.Fatalf("cycle %d: ResumeAccept: %v", cycle, err)
		}
		var c sockets895
		for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl); time.Sleep(5 * time.Millisecond) {
			if c = census895(t); c.listening[addrPort] >= workers {
				break
			}
		}
		t.Logf("cycle %d: after resume, LISTEN sockets by port: %v (Addr() port %d)", cycle, c.listening, addrPort)
		if gotPorts := ports895(c.listening); len(gotPorts) != 1 || gotPorts[0] != addrPort || c.listening[addrPort] != workers {
			t.Fatalf("cycle %d: LISTEN sockets by port %v, want %d on the published %d", cycle, c.listening, workers, addrPort)
		}
		get895(t, addr)
	}
	if err := r.stop(t); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if n := census895(t).bound[addrPort]; n != 0 {
		t.Fatalf("%d sockets still bound to port %d after Listen returned, want 0", n, addrPort)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("net.Listen on %s the instant Listen returned: %v", addr, err)
	}
	_ = ln.Close()
}

// TestResumeAfterThePortWasTaken895 pins what a resume does when the port it
// re-binds was taken while accepting was paused, which only a fixed port allows
// (a port 0 engine's hold refuses every plain listener, the test above). Each
// worker's re-create fails; the worker counts it (ErrorListenerRecreate), logs
// it and ends itself, and ResumeAccept, which only wakes the workers and cannot
// wait for their sockets, has returned nil by then. The engine is left running
// with no listener until its context is cancelled, which is the behaviour
// ErrorListenerRecreate's doc describes ("an engine that has lost accept
// capacity"); Server.ResumeAccept's doc says so too (celeris#895). The assertion
// that Listen does not return on its own pins today's behaviour: a change that
// makes a lost listener end the engine is expected to flip that line, not to
// be read as a regression.
func TestResumeAfterThePortWasTaken895(t *testing.T) {
	addr := freeAddr639(t)
	r := start895(t, addr, 2, 1, true)
	r.e.mu.Lock()
	workers := len(r.e.workers)
	r.e.mu.Unlock()
	if err := r.e.PauseAccept(); err != nil {
		t.Fatalf("PauseAccept: %v", err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("a fixed port has no hold, so a plain listener should take it while paused: %v", err)
	}
	defer func() { _ = ln.Close() }()
	if err := r.e.ResumeAccept(); err != nil {
		t.Fatalf("ResumeAccept: %v", err)
	}
	var lost uint64
	for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl); time.Sleep(5 * time.Millisecond) {
		if lost = r.e.Metrics().ErrorListenerRecreate; lost >= uint64(workers) {
			break
		}
	}
	t.Logf("%d workers; after a resume onto a taken port ErrorListenerRecreate = %d", workers, lost)
	if lost != uint64(workers) {
		t.Fatalf("ErrorListenerRecreate = %d, want %d (one per worker): the lost listener is not reported", lost, workers)
	}
	select {
	case err := <-r.done:
		r.done <- err
		t.Fatalf("Listen returned (%v) on its own; the engine is documented to keep running without a listener", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := r.stop(t); err != nil {
		t.Fatalf("Listen: %v", err)
	}
}
