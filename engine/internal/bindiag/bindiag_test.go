//go:build linux

package bindiag

import (
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestBindWithRetrySucceedsImmediately pins the happy path: the very
// first Bind succeeds and returns nil with no sleep.
func TestBindWithRetrySucceedsImmediately(t *testing.T) {
	fd, sa := newReusePortSocket(t, "127.0.0.1:0")
	defer unix.Close(fd)

	start := time.Now()
	if err := BindWithRetry(fd, sa); err != nil {
		t.Fatalf("BindWithRetry on a fresh port: %v", err)
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Errorf("happy path took %v — should be near-zero (no retry sleeps)", d)
	}
}

// TestBindWithRetryReturnsImmediatelyOnNonEADDRINUSE pins that a
// non-EADDRINUSE error short-circuits without consuming the retry
// budget. Use EBADF (closed fd) as the trigger — it's the canonical
// "definitely not transient" Bind failure.
func TestBindWithRetryReturnsImmediatelyOnNonEADDRINUSE(t *testing.T) {
	fd, sa := newReusePortSocket(t, "127.0.0.1:0")
	_ = unix.Close(fd) // close BEFORE Bind so the syscall fails with EBADF

	start := time.Now()
	err := BindWithRetry(fd, sa)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("BindWithRetry on a closed fd: want error, got nil")
	}
	if errors.Is(err, unix.EADDRINUSE) {
		t.Fatalf("non-EADDRINUSE expected, got EADDRINUSE: %v", err)
	}
	// Non-EADDRINUSE errors must short-circuit. Even one retry sleep
	// would push elapsed past ~250 µs; budget 10 ms for scheduler
	// noise on a loaded CI runner.
	if elapsed > 10*time.Millisecond {
		t.Errorf("non-EADDRINUSE took %v — must short-circuit, no retries", elapsed)
	}
}

// TestBindWithRetryEventuallyGivesUpOnPersistentEADDRINUSE pins that
// a real EADDRINUSE (not transient) escapes the retry budget. We hold
// the port open with a non-SO_REUSEPORT bind so retries can never
// succeed; BindWithRetry must return EADDRINUSE after consuming the
// retry budget.
func TestBindWithRetryEventuallyGivesUpOnPersistentEADDRINUSE(t *testing.T) {
	// Hold a port open WITHOUT SO_REUSEPORT so a SO_REUSEPORT bind
	// to the same port cannot succeed.
	holder, port := holdPortNoReusePort(t)
	defer holder.Close()

	fd, sa := newReusePortSocket(t, "127.0.0.1:"+strconv.Itoa(port))
	defer unix.Close(fd)

	start := time.Now()
	err := BindWithRetry(fd, sa)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("BindWithRetry against held port: want EADDRINUSE, got nil")
	}
	if !errors.Is(err, unix.EADDRINUSE) {
		t.Fatalf("want EADDRINUSE, got %v", err)
	}
	// Must have actually retried. With BindRetries=9 and the schedule
	// in Run(), the minimum total sleep across attempts 1..8 is
	// 250 µs · (1 + 2 + 4 + 8 + 16 + 32 + 64 + 128) = 63.75 ms.
	if elapsed < 50*time.Millisecond {
		t.Errorf("elapsed %v too short — retry budget should produce ≥50 ms even on a fast host",
			elapsed)
	}
}

// TestBindWithRetryUnderConcurrentSOReusePort exercises the actual
// race the helper exists to mitigate: many concurrent Binds into the
// same SO_REUSEPORT group on the same port. Every attempt must either
// succeed or return a definite error — no goroutine blocks forever.
func TestBindWithRetryUnderConcurrentSOReusePort(t *testing.T) {
	const N = 12 // mirrors the epoll loop count on a 12-core host

	// Pre-pick a port by binding-then-closing a reuseport socket; the
	// real test then races N goroutines onto that port.
	probe, sa := newReusePortSocket(t, "127.0.0.1:0")
	if err := unix.Bind(probe, sa); err != nil {
		_ = unix.Close(probe)
		t.Fatalf("probe bind: %v", err)
	}
	probeAddr, _ := unix.Getsockname(probe)
	_ = unix.Close(probe)

	port := 0
	if v, ok := probeAddr.(*unix.SockaddrInet4); ok {
		port = v.Port
	}

	var (
		wg        sync.WaitGroup
		successes atomic.Int32
		failures  atomic.Int32
	)
	for range N {
		wg.Go(func() {
			fd, addr := newReusePortSocketRaw(t, port)
			defer unix.Close(fd)
			if err := BindWithRetry(fd, addr); err == nil {
				successes.Add(1)
			} else {
				failures.Add(1)
			}
		})
	}
	wg.Wait()

	// Every goroutine must have completed (no blocked-forever).
	total := successes.Load() + failures.Load()
	if total != N {
		t.Fatalf("expected %d completions, got %d (success=%d fail=%d)",
			N, total, successes.Load(), failures.Load())
	}
	// The whole point of this test: under SO_REUSEPORT, every bind
	// SHOULD succeed. If we see failures, the retry budget wasn't
	// enough — the helper failed its job.
	if failures.Load() > 0 {
		t.Errorf("under SO_REUSEPORT contention every bind should succeed; got %d failures",
			failures.Load())
	}
}

// TestFormatProducesStructuredString pins the diagnostic format so a
// regression that breaks the kv=v shape (which gets attached to error
// messages) is caught.
func TestFormatProducesStructuredString(t *testing.T) {
	fd, sa := newReusePortSocket(t, "127.0.0.1:0")
	defer unix.Close(fd)

	got := Format(fd, sa)

	for _, want := range []string{"addr=127.0.0.1:", "SO_REUSEADDR=", "SO_REUSEPORT=", "our_open_fds="} {
		if !strings.Contains(got, want) {
			t.Errorf("Format() = %q; missing %q", got, want)
		}
	}
}

// --- helpers -------------------------------------------------------------

func newReusePortSocket(t *testing.T, addr string) (int, unix.Sockaddr) {
	t.Helper()
	host, port := splitHostPort(t, addr)
	return newReusePortSocketRawAt(t, host, port)
}

func newReusePortSocketRaw(t *testing.T, port int) (int, unix.Sockaddr) {
	t.Helper()
	return newReusePortSocketRawAt(t, "127.0.0.1", port)
}

func newReusePortSocketRawAt(t *testing.T, host string, port int) (int, unix.Sockaddr) {
	t.Helper()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
		_ = unix.Close(fd)
		t.Fatalf("SO_REUSEADDR: %v", err)
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
		_ = unix.Close(fd)
		t.Fatalf("SO_REUSEPORT: %v", err)
	}
	sa4 := &unix.SockaddrInet4{Port: port}
	parsed := net.ParseIP(host).To4()
	if parsed == nil {
		_ = unix.Close(fd)
		t.Fatalf("bad host: %q", host)
	}
	copy(sa4.Addr[:], parsed)
	return fd, sa4
}

func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort %q: %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("Atoi %q: %v", portStr, err)
	}
	return host, port
}

// holdPortNoReusePort opens a stdlib net.Listener on a random port
// WITHOUT SO_REUSEPORT, so a subsequent SO_REUSEPORT bind to the same
// port returns EADDRINUSE persistently. Returns the listener (caller
// must Close) and the held port.
func holdPortNoReusePort(t *testing.T) (net.Listener, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	return ln, addr.Port
}
