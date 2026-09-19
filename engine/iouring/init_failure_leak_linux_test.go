//go:build linux

package iouring

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/resource"
)

// celeris#656. Worker.run creates its SO_REUSEPORT listen socket, then sets up
// its ring and submits the first accept. When the ring setup or that first
// submit failed, run sent the error on ready and returned, and nothing closed
// what it had already made: shutdown, the only code that closes a worker's
// descriptors, runs from the event loop, which a worker that never became
// ready does not reach. Listen cancels the other workers and returns the
// error, and the failed worker's socket stays LISTENing in the port's
// SO_REUSEPORT group for the life of the process: the kernel keeps hashing a
// share of new connections into a backlog nobody accepts. The adaptive engine
// retries a failed lazy io_uring build on the port it is serving, one more
// dead listener per attempt.

var errInjected656 = errors.New("injected worker init failure (celeris#656)")

// envRequireIOUring656 turns every environment skip in this file into a
// failure. These three tests need two io_uring workers on one port, and
// RLIMIT_MEMLOCK funds only one at the GitHub-hosted default of 8 MiB
// (minMemlockPerWorker is 12 MiB), so in the `unit` job they all skipped and
// the leak they pin had no CI cover at all — a skip is not coverage. The
// dedicated `iouring` CI job raises memlock and sets this, exactly as the
// `adaptive` job does with CELERIS_REQUIRE_UPSWITCH (celeris#641), so that job
// cannot go green without running them.
//
// newTestRing and TestWorkersShareTheHandoffLossWitnesses honour it too, so the
// `unit` job's celeris#657 step, which runs those witness tests by name, cannot
// go green by skipping them either.
const envRequireIOUring656 = "CELERIS_REQUIRE_IOURING_WORKERS"

// skipOrFail656 skips with the given reason, or fails when the environment
// declares these tests mandatory.
func skipOrFail656(t *testing.T, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	if os.Getenv(envRequireIOUring656) == "1" {
		t.Fatal(msg + " -- " + envRequireIOUring656 + "=1 forbids skipping")
	}
	t.Skip(msg)
}

// listenersOnPort656 counts this process's LISTEN sockets bound to port. It
// reads /proc/self/fd, so another process holding the port cannot confuse it.
func listenersOnPort656(t *testing.T, port int) int {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("read /proc/self/fd: %v", err)
	}
	n := 0
	for _, ent := range ents {
		fd, err := strconv.Atoi(ent.Name())
		if err != nil {
			continue
		}
		link, err := os.Readlink("/proc/self/fd/" + ent.Name())
		if err != nil || !strings.HasPrefix(link, "socket:[") {
			continue
		}
		if v, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ACCEPTCONN); err != nil || v != 1 {
			continue
		}
		sa, err := unix.Getsockname(fd)
		if err != nil {
			continue
		}
		switch a := sa.(type) {
		case *unix.SockaddrInet4:
			if a.Port == port {
				n++
			}
		case *unix.SockaddrInet6:
			if a.Port == port {
				n++
			}
		}
	}
	return n
}

// anonInodes656 counts this process's descriptors on an anonymous inode of
// the given kind, "[io_uring]" or "[eventfd]".
func anonInodes656(t *testing.T, kind string) int {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("read /proc/self/fd: %v", err)
	}
	n := 0
	for _, ent := range ents {
		if link, err := os.Readlink("/proc/self/fd/" + ent.Name()); err == nil && link == "anon_inode:"+kind {
			n++
		}
	}
	return n
}

// bindWithin656 reports whether a plain listener (SO_REUSEADDR, no
// SO_REUSEPORT) can bind addr within d. A socket still LISTENing on the port
// refuses it with EADDRINUSE. It retries because a sibling worker that did
// start closes its socket in shutdown, and the kernel can hold that socket
// until the ring's asynchronous teardown lets go of its accept; a leaked
// socket never goes away, so retrying cannot hide one.
func bindWithin656(addr string, d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			_ = ln.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func port656(t *testing.T, addr string) int {
	t.Helper()
	_, ps, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	p, err := strconv.Atoi(ps)
	if err != nil {
		t.Fatalf("port %q: %v", ps, err)
	}
	return p
}

// oracleControl656 proves both oracles see the kind of socket that leaks: one
// made by this package's own createListenSocket. It needs no io_uring.
func oracleControl656(t *testing.T, addr string) {
	t.Helper()
	port := port656(t, addr)
	fd, err := createListenSocket(addr, true)
	if err != nil {
		t.Fatalf("oracle control: createListenSocket(%s): %v", addr, err)
	}
	if n := listenersOnPort656(t, port); n != 1 {
		_ = unix.Close(fd)
		t.Fatalf("oracle control: %d listeners on port %d with one live createListenSocket socket, want 1", n, port)
	}
	if ln, err := net.Listen("tcp", addr); err == nil {
		_ = ln.Close()
		_ = unix.Close(fd)
		t.Fatal("oracle control: a plain bind succeeded over a live SO_REUSEPORT listener; the bind check cannot see a leak")
	} else if !errors.Is(err, unix.EADDRINUSE) {
		_ = unix.Close(fd)
		t.Fatalf("oracle control: bind over a live listener failed with %v, want EADDRINUSE", err)
	}
	_ = unix.Close(fd)
	if n := listenersOnPort656(t, port); n != 0 {
		t.Fatalf("oracle control: %d listeners on port %d after closing the control socket, want 0", n, port)
	}
}

// listenExpectingInjectedFailure runs Listen and requires it to fail with the
// injected error, so a test cannot pass because the injection never fired.
func listenExpectingInjectedFailure(t *testing.T, e *Engine) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		cancel()
		<-done
		t.Fatal("Listen kept running for 10s: the injected worker failure did not fire")
	}
	if err == nil {
		t.Fatal("Listen returned nil: the injected worker failure did not fire")
	}
	if !errors.Is(err, errInjected656) {
		if ioUringUnavailable639(err) {
			skipOrFail656(t, "io_uring unavailable on this runner: %v", err)
		}
		t.Fatalf("Listen = %v, want the injected failure", err)
	}
	t.Logf("Listen failed as injected: %v", err)
}

func newEngine656(t *testing.T, addr string, workers int) *Engine {
	t.Helper()
	if maxW := MaxWorkersForMemlock(); maxW >= 0 && maxW < workers {
		skipOrFail656(t, "RLIMIT_MEMLOCK funds %d io_uring workers, the test needs %d "+
			"(raise it: `ulimit -l unlimited`, or run as root)", maxW, workers)
	}
	e, err := New(resource.Config{
		Addr:      addr,
		Protocol:  engine.HTTP1,
		Resources: resource.Resources{Workers: workers},
	}, respondingHandler{})
	if err != nil {
		skipOrFail656(t, "New: %v", err)
	}
	return e
}

// failRingSetup656 makes worker ring setup fail on the calls selected by fail
// (1-based call number) and returns how many calls were made.
func failRingSetup656(t *testing.T, fail func(call int32) bool) *atomic.Int32 {
	t.Helper()
	orig := newWorkerRing
	var calls atomic.Int32
	newWorkerRing = func(entries, flags, sqPollIdle uint32, cpuID int) (*Ring, error) {
		if fail(calls.Add(1)) {
			return nil, errInjected656
		}
		return orig(entries, flags, sqPollIdle, cpuID)
	}
	// Restored only after Listen has returned, and Listen joins every worker
	// before it returns, so no worker reads the var concurrently.
	t.Cleanup(func() { newWorkerRing = orig })
	return &calls
}

func assertNoListenerSurvives656(t *testing.T, addr string) {
	t.Helper()
	port := port656(t, addr)
	if n := listenersOnPort656(t, port); n != 0 {
		t.Errorf("%d listen socket(s) on port %d survived Listen's init failure, want 0", n, port)
	}
	if err := bindWithin656(addr, 3*time.Second); err != nil {
		t.Errorf("port %d still refuses a plain bind 3s after Listen failed: %v", port, err)
	}
}

// TestListenClosesListenSocketsWhenEveryWorkerRingSetupFails: both workers
// fail ring setup, so each one leaked its socket.
func TestListenClosesListenSocketsWhenEveryWorkerRingSetupFails(t *testing.T) {
	addr := freeAddr639(t)
	oracleControl656(t, addr)
	e := newEngine656(t, addr, 2)
	calls := failRingSetup656(t, func(int32) bool { return true })

	listenExpectingInjectedFailure(t, e)
	t.Logf("worker ring setup calls: %d", calls.Load())
	assertNoListenerSurvives656(t, addr)
}

// TestListenClosesListenSocketWhenOneWorkerRingSetupFails: one worker fails
// ring setup and its sibling starts, then is cancelled by Listen.
func TestListenClosesListenSocketWhenOneWorkerRingSetupFails(t *testing.T) {
	addr := freeAddr639(t)
	oracleControl656(t, addr)
	e := newEngine656(t, addr, 2)
	calls := failRingSetup656(t, func(call int32) bool { return call == 1 })

	listenExpectingInjectedFailure(t, e)
	if got := calls.Load(); got != 2 {
		t.Fatalf("worker ring setup ran %d times, want 2 (one failed, one started)", got)
	}
	assertNoListenerSurvives656(t, addr)
}

// TestListenClosesListenSocketRingAndEventfdWhenInitialSubmitFails: the
// initial submit fails after the ring and the H2 eventfd exist too.
func TestListenClosesListenSocketRingAndEventfdWhenInitialSubmitFails(t *testing.T) {
	addr := freeAddr639(t)
	oracleControl656(t, addr)
	e := newEngine656(t, addr, 2)

	orig := submitInitialAccept
	var calls atomic.Int32
	submitInitialAccept = func(*Ring) (int, error) {
		calls.Add(1)
		return 0, errInjected656
	}
	t.Cleanup(func() { submitInitialAccept = orig })

	ringsBefore := anonInodes656(t, "[io_uring]")
	eventfdsBefore := anonInodes656(t, "[eventfd]")

	listenExpectingInjectedFailure(t, e)
	t.Logf("initial submit calls: %d", calls.Load())
	assertNoListenerSurvives656(t, addr)
	if got := anonInodes656(t, "[io_uring]"); got != ringsBefore {
		t.Errorf("io_uring descriptors: %d before Listen, %d after its init failure (want equal)", ringsBefore, got)
	}
	if got := anonInodes656(t, "[eventfd]"); got != eventfdsBefore {
		t.Errorf("eventfd descriptors: %d before Listen, %d after its init failure (want equal)", eventfdsBefore, got)
	}
}
