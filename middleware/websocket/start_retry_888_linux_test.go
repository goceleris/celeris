//go:build linux

package websocket_test

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris"
	celerisengine "github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/probe"
)

// The start helpers of the engine tests of this package share one rule for a
// Start that fails (celeris#888 item 48, as middleware/sse's startSSEServer and
// middleware/session's startLoginServer). The io_uring ring memory is charged to
// RLIMIT_MEMLOCK per user, so a start made while another test binary holds rings
// fails although nothing is wrong: either io_uring_setup answers ENOMEM, or the
// engine's own probe does and the engine reports "io_uring not available". Both
// are retried with a new server and listener for startRetryWindow when io_uring
// exists on this host (ioUringExists). After the window the test fails; it skips
// only where io_uring does not exist at all.
const startRetryWindow = 10 * time.Second

// startFailure is what startWS888 does with a Start that failed.
type startFailure int

const (
	startRetry startFailure = iota // a new server and listener, after a pause
	startFail                      // a real failure: fail the test
	startSkip                      // io_uring does not work on this host: skip
)

// classifyStart decides what a failed Start means. msg is the Start error.
// exists is consulted only for an io_uring failure that is not ENOMEM, and
// reports whether io_uring exists on this host (works, or is only short of
// memory). retryLeft says whether the retry window is still open.
func classifyStart(msg string, exists func() bool, retryLeft bool) startFailure {
	if strings.Contains(msg, "cannot allocate memory") {
		// io_uring_setup answered ENOMEM, so io_uring exists on this host; the
		// RLIMIT_MEMLOCK charge of other rings is what is failing. The probe
		// would fail with the same ENOMEM and report "no io_uring", so it is
		// not consulted: inside the window the start is retried, after it the
		// test fails (it must not skip where io_uring works).
		if retryLeft {
			return startRetry
		}
		return startFail
	}
	if strings.Contains(msg, "io_uring") || strings.Contains(msg, "not available") {
		// The engine's own probe can fail with that ENOMEM and then reports
		// "io_uring not available on this system", without the word ENOMEM.
		// So the answer is the test's own look at the host: io_uring does not
		// exist (skip), or it does and the start failed (retry inside the
		// window, fail after it).
		if !exists() {
			return startSkip
		}
		if retryLeft {
			return startRetry
		}
		return startFail
	}
	return startFail
}

// ioUringExists reports whether io_uring exists on this host: a raw
// io_uring_setup answers ENOMEM (it exists, and is short of memory), or the
// probe finds a tier of High or better with provided buffers (it works).
func ioUringExists(_ testing.TB) bool {
	if ioUringSetupErrno() == unix.ENOMEM {
		return true
	}
	p := probe.Probe()
	return p.IOUringTier >= celerisengine.High && p.ProvidedBuffers
}

// ioUringSetupErrno makes one io_uring_setup call and returns its errno, 0 when
// it worked (the ring is closed again).
func ioUringSetupErrno() unix.Errno {
	var params [120]byte // sizeof(struct io_uring_params)
	fd, _, errno := unix.Syscall(unix.SYS_IO_URING_SETUP, 4, uintptr(unsafe.Pointer(&params[0])), 0)
	if errno == 0 {
		_ = unix.Close(int(fd))
	}
	return errno
}

// startWS888 starts the server that build makes on a fresh loopback listener and
// returns it with its address and a stop function (cancel, then wait up to 3 s
// for Start to return). A Start that fails is classified (classifyStart): it is
// retried with a new server and listener inside startRetryWindow, skips the test
// where io_uring does not exist, and fails it otherwise. build is called once per
// try, so no server or listener is reused after a failed Start.
func startWS888(tb testing.TB, build func() *celeris.Server) (srv *celeris.Server, addr string, stop func()) {
	tb.Helper()
	began := time.Now()
	deadline := began.Add(startRetryWindow)
	for retries := 0; ; retries++ {
		srv, addr, stop, err := tryStartWS888(tb, build)
		if err == nil {
			if retries > 0 {
				tb.Logf("startWS888: started after %d retries in %v", retries, time.Since(began).Round(time.Millisecond))
			}
			return srv, addr, stop
		}
		switch classifyStart(err.Error(), func() bool { return ioUringExists(tb) }, time.Now().Before(deadline)) {
		case startRetry:
			if retries == 0 {
				tb.Logf("startWS888: retrying a Start that failed: %v", err)
			}
			time.Sleep(2 * time.Millisecond)
		case startSkip:
			tb.Skipf("engine unavailable on this runner: %v", err)
		default:
			tb.Fatalf("server start (after %d retries): %v", retries, err)
		}
	}
}

// tryStartWS888 is one attempt of startWS888. It returns Start's error.
func tryStartWS888(tb testing.TB, build func() *celeris.Server) (*celeris.Server, string, func(), error) {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	addr := ln.Addr().String()
	srv := build()
	ctx, cancel := context.WithCancel(context.Background())
	var startErr atomic.Pointer[error]
	done := make(chan struct{})
	go func() {
		defer close(done)
		if e := srv.StartWithListenerAndContext(ctx, ln); e != nil {
			startErr.Store(&e)
		}
	}()
	// iouring and epoll rebind via SO_REUSEPORT: give them time to settle.
	time.Sleep(500 * time.Millisecond)
	// A Start that fails after the settle sleep is not missed (celeris#706).
	waitUpOrStopped(tb, srv, done, 30*time.Second)
	if p := startErr.Load(); p != nil {
		cancel()
		_ = ln.Close()
		return nil, "", nil, *p
	}
	return srv, addr, func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			tb.Log("server goroutine did not exit within 3s")
		}
	}, nil
}

// TestClassifyStartTable pins classifyStart's decision for each kind of Start
// failure. exists is consulted only for a failure that names io_uring and is
// not ENOMEM; ENOMEM is retried inside the window and fails after it.
func TestClassifyStartTable(t *testing.T) {
	yes := func() bool { return true }
	no := func() bool { return false }
	never := func() bool {
		t.Fatal("exists consulted for an ENOMEM or for a failure that does not name io_uring")
		return false
	}
	cases := []struct {
		name      string
		msg       string
		exists    func() bool
		retryLeft bool
		want      startFailure
	}{
		{"ENOMEM inside the window is retried", "worker 0 ring setup: io_uring_setup: cannot allocate memory (likely RLIMIT_MEMLOCK)", never, true, startRetry},
		{"ENOMEM after the window fails, probe not consulted", "io_uring_setup: cannot allocate memory", never, false, startFail},
		{"ENOMEM after the window fails even if the probe says no", "worker 0 ring setup: io_uring_setup: cannot allocate memory", no, false, startFail},
		{"io_uring unavailable where it does not exist skips", "engine unavailable: io_uring not available", no, true, startSkip},
		{"io_uring unavailable where it does not exist skips after the window too", "engine unavailable: io_uring not available", no, false, startSkip},
		{"io_uring unavailable where it exists is retried", "create engine: io_uring not available on this system", yes, true, startRetry},
		{"io_uring unavailable where it exists fails after the window", "create engine: io_uring not available on this system", yes, false, startFail},
		{"not available without io_uring skips where io_uring does not exist", "feature not available", no, true, startSkip},
		{"a start error that is not about io_uring fails", "listen tcp: address already in use", never, true, startFail},
		{"a memory error without io_uring is retried in the window", "mmap: cannot allocate memory", never, true, startRetry},
		{"a memory error without io_uring fails after the window", "mmap: cannot allocate memory", never, false, startFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyStart(tc.msg, tc.exists, tc.retryLeft); got != tc.want {
				t.Fatalf("classifyStart(%q, retryLeft=%t) = %d, want %d", tc.msg, tc.retryLeft, got, tc.want)
			}
		})
	}
}
