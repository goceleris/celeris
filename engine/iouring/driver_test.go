//go:build linux

package iouring

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// testHandler is a minimal stream.Handler for engine bring-up.
type testHandler struct{}

func (testHandler) HandleStream(_ context.Context, _ *stream.Stream) error { return nil }

func startTestEngine(t *testing.T) (*Engine, func()) {
	t.Helper()

	cfg := resource.Config{
		Addr:      "127.0.0.1:0",
		Engine:    engine.IOUring,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Resources: resource.Resources{Workers: 2},
	}

	// Use a TCP listener to capture a free port, then hand the address to the
	// engine. Engine rebinds via SO_REUSEPORT.
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cfg.Addr = ln.Addr().String()
	_ = ln.Close()

	var h stream.Handler = testHandler{}
	e, err := New(cfg, h)
	if err != nil {
		t.Skipf("iouring engine unavailable: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = e.Listen(ctx)
		close(done)
	}()

	// Wait until the engine has at least one worker ready.
	deadline := time.Now().Add(3 * time.Second)
	for {
		e.mu.Lock()
		n := len(e.workers)
		e.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("engine did not start in time")
		}
		time.Sleep(10 * time.Millisecond)
	}

	return e, func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Log("engine shutdown timeout")
		}
	}
}

// nonblockSocketPair creates a pair of connected non-blocking AF_UNIX sockets.
// Returns the two raw FDs.
func nonblockSocketPair(t *testing.T) (int, int) {
	t.Helper()
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	return pair[0], pair[1]
}

func TestDriverRegisterUnregister(t *testing.T) {
	e, stop := startTestEngine(t)
	defer stop()

	if e.NumWorkers() < 1 {
		t.Fatal("no workers")
	}
	wl := e.WorkerLoop(0)

	a, b := nonblockSocketPair(t)
	defer func() { _ = unix.Close(b) }()

	var closed atomic.Bool
	// closeErr uses atomic.Pointer[error] so a "no error" state is a nil
	// pointer rather than a nil interface stored into atomic.Value (which
	// panics with "store of nil value into Value").
	var closeErr atomic.Pointer[error]

	recvCh := make(chan []byte, 4)
	if err := wl.RegisterConn(a, func(p []byte) {
		cp := make([]byte, len(p))
		copy(cp, p)
		recvCh <- cp
	}, func(err error) {
		closed.Store(true)
		if err != nil {
			closeErr.Store(&err)
		}
	}); err != nil {
		t.Fatalf("RegisterConn: %v", err)
	}

	// Write from peer; expect onRecv.
	if _, err := unix.Write(b, []byte("hello")); err != nil {
		t.Fatalf("write peer: %v", err)
	}
	select {
	case data := <-recvCh:
		if !bytes.Equal(data, []byte("hello")) {
			t.Fatalf("onRecv got %q, want hello", data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for onRecv")
	}

	// Write via the worker loop; peer should read it.
	if err := wl.Write(a, []byte("world")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	buf := make([]byte, 64)
	var total []byte
	for time.Now().Before(deadline) && len(total) < 5 {
		n, err := unix.Read(b, buf)
		if n > 0 {
			total = append(total, buf[:n]...)
		}
		if err != nil && !errors.Is(err, unix.EAGAIN) {
			break
		}
		if n == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !bytes.Equal(total, []byte("world")) {
		t.Fatalf("peer read %q, want world", total)
	}

	// Unregister and verify onClose fires.
	if err := wl.UnregisterConn(a); err != nil {
		t.Fatalf("UnregisterConn: %v", err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for !closed.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !closed.Load() {
		t.Fatal("onClose did not fire")
	}
	_ = unix.Close(a)
}

func TestDriverHTTPZeroOverhead(t *testing.T) {
	e, stop := startTestEngine(t)
	defer stop()

	// Before any registration, the zero-cost gate must be off on every worker.
	for i := 0; i < e.NumWorkers(); i++ {
		w := e.workers[i]
		if w.hasDriverConns.Load() {
			t.Fatalf("worker %d: hasDriverConns should be false before any registration", i)
		}
	}

	wl := e.WorkerLoop(0)
	a, b := nonblockSocketPair(t)
	defer func() { _ = unix.Close(b) }()

	if err := wl.RegisterConn(a, func([]byte) {}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn: %v", err)
	}

	// After registration, the gate on worker 0 must be set.
	deadline := time.Now().Add(500 * time.Millisecond)
	for !e.workers[0].hasDriverConns.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !e.workers[0].hasDriverConns.Load() {
		t.Fatal("hasDriverConns should be set after RegisterConn")
	}

	_ = wl.UnregisterConn(a)
	_ = unix.Close(a)

	// After unregister + close, the gate clears.
	deadline = time.Now().Add(2 * time.Second)
	for e.workers[0].hasDriverConns.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if e.workers[0].hasDriverConns.Load() {
		t.Fatal("hasDriverConns should clear after UnregisterConn")
	}
}

func TestDriverWriteSerialization(t *testing.T) {
	e, stop := startTestEngine(t)
	defer stop()

	wl := e.WorkerLoop(0)
	a, b := nonblockSocketPair(t)
	defer func() { _ = unix.Close(b) }()

	if err := wl.RegisterConn(a, func([]byte) {}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn: %v", err)
	}

	const payloads = 100
	const chunkSize = 64
	expected := make([]byte, 0, payloads*chunkSize)
	for i := 0; i < payloads; i++ {
		chunk := bytes.Repeat([]byte{byte('A' + i%26)}, chunkSize)
		expected = append(expected, chunk...)
	}

	var wg sync.WaitGroup
	for i := 0; i < payloads; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			chunk := bytes.Repeat([]byte{byte('A' + i%26)}, chunkSize)
			_ = wl.Write(a, chunk)
		}()
	}
	wg.Wait()

	// Drain from peer.
	total := make([]byte, 0, len(expected))
	buf := make([]byte, 4096)
	deadline := time.Now().Add(3 * time.Second)
	for len(total) < len(expected) && time.Now().Before(deadline) {
		n, err := unix.Read(b, buf)
		if n > 0 {
			total = append(total, buf[:n]...)
			continue
		}
		if err != nil && !errors.Is(err, unix.EAGAIN) {
			t.Fatalf("peer read: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(total) != len(expected) {
		t.Fatalf("got %d bytes, want %d", len(total), len(expected))
	}
	// Byte count is what we guarantee; ordering across goroutines is not.
	// Verify no corruption: each byte must be in the alphabet range.
	for i, c := range total {
		if c < 'A' || c > 'Z' {
			t.Fatalf("byte %d = %q: corruption", i, c)
		}
	}

	_ = wl.UnregisterConn(a)
	_ = unix.Close(a)
}

func TestDriverCancelOnUnregister(t *testing.T) {
	e, stop := startTestEngine(t)
	defer stop()

	wl := e.WorkerLoop(0)
	a, b := nonblockSocketPair(t)
	defer func() { _ = unix.Close(b) }()
	defer func() { _ = unix.Close(a) }()

	var closed atomic.Bool
	if err := wl.RegisterConn(a, func([]byte) {}, func(error) {
		closed.Store(true)
	}); err != nil {
		t.Fatalf("RegisterConn: %v", err)
	}

	// Give the worker a moment to arm the RECV.
	time.Sleep(50 * time.Millisecond)

	if err := wl.UnregisterConn(a); err != nil {
		t.Fatalf("UnregisterConn: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for !closed.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !closed.Load() {
		t.Fatal("onClose did not fire after UnregisterConn")
	}
}

// TestIouringRegisterConnFDCollision verifies RegisterConn rejects an fd that
// is already bound to an HTTP connection. Without this guard, CQEs for the
// same fd would route to either the HTTP dispatcher or the driver dispatcher
// non-deterministically, corrupting both paths.
func TestIouringRegisterConnFDCollision(t *testing.T) {
	e, stop := startTestEngine(t)
	defer stop()

	w := e.workers[0]
	wl := e.WorkerLoop(0)

	// Simulate an HTTP connection squatting at fd=7 by inserting a non-nil
	// entry directly into the worker's conns slice. The RegisterConn guard
	// must reject driver registration on that fd.
	const fakeFD = 7
	if fakeFD >= len(w.conns) {
		t.Fatalf("fakeFD %d out of range (conns len %d)", fakeFD, len(w.conns))
	}
	prev := w.conns[fakeFD]
	w.conns[fakeFD] = &connState{fd: fakeFD}
	defer func() { w.conns[fakeFD] = prev }()

	err := wl.RegisterConn(fakeFD, func([]byte) {}, func(error) {})
	if err == nil {
		// Clean up the erroneous registration.
		_ = wl.UnregisterConn(fakeFD)
		t.Fatal("RegisterConn should reject fd already bound to HTTP conn")
	}
	if !strings.Contains(err.Error(), "HTTP connection") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

// TestIouringShutdownFiresDriverOnClose verifies Worker.shutdown fires
// onClose for every registered driver conn; otherwise drivers leak their
// per-conn goroutines/state when the engine stops.
func TestIouringShutdownFiresDriverOnClose(t *testing.T) {
	e, stop := startTestEngine(t)

	wl := e.WorkerLoop(0)
	a, b := nonblockSocketPair(t)
	defer func() { _ = unix.Close(a) }()
	defer func() { _ = unix.Close(b) }()

	closeErrCh := make(chan error, 1)
	if err := wl.RegisterConn(a, func([]byte) {}, func(err error) {
		select {
		case closeErrCh <- err:
		default:
		}
	}); err != nil {
		stop()
		t.Fatalf("RegisterConn: %v", err)
	}

	// Give the worker a moment to arm the RECV so shutdown has real state
	// to tear down (not just a freshly-inserted map entry).
	time.Sleep(50 * time.Millisecond)

	stop() // triggers Worker.shutdown on each worker goroutine

	select {
	case err := <-closeErrCh:
		if err == nil {
			t.Fatal("expected non-nil error from shutdown-triggered onClose")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("onClose did not fire during engine shutdown")
	}
}

func TestDriverUnknownFDReturnsErr(t *testing.T) {
	e, stop := startTestEngine(t)
	defer stop()

	wl := e.WorkerLoop(0)
	if err := wl.UnregisterConn(999999); !errors.Is(err, engine.ErrUnknownFD) {
		t.Fatalf("UnregisterConn unknown fd: got %v, want ErrUnknownFD", err)
	}
	if err := wl.Write(999999, []byte("x")); !errors.Is(err, engine.ErrUnknownFD) {
		t.Fatalf("Write unknown fd: got %v, want ErrUnknownFD", err)
	}
}
