//go:build linux

package epoll

// celeris#710. The worker drains a driver conn's socket in a loop, and reads
// again after onRecv whenever the previous read filled its 32 KiB buffer. It
// read by the caller's descriptor NUMBER and checked whether the conn had been
// unregistered only after the read. UnregisterConn runs on the caller's
// goroutine and returns at once, so a caller that unregistered while the
// worker was inside the conn's read loop (in onRecv, say), closed fd, and let
// another file take the number, had the worker read that file: its bytes were
// then dropped, because the conn was closed by then. A blocking file with
// nothing to read parked the worker, and every connection on it, until that
// file got data.
//
// These tests park the worker goroutine inside the conn's own onRecv after a
// read that filled the buffer, so every step the caller takes happens while
// the worker is inside the read loop, and the worker's next read comes after
// all of them. takeNumber (dup3) makes the reuse of the number deterministic.

import (
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
)

// takeNumber710 makes a new socket X hold fd's number, as the next socket the
// process creates does once fd is closed. It returns X's two ends; the caller
// closes both, and fd again when it is not X's first.
func takeNumber710(t *testing.T, fd int, nonblock bool) (x0, x1 int) {
	t.Helper()
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	x0, x1 = pair[0], pair[1]
	if nonblock {
		for _, s := range pair {
			if err := unix.SetNonblock(s, true); err != nil {
				t.Fatalf("setnonblock: %v", err)
			}
		}
	}
	if x0 != fd {
		if err := unix.Dup3(x0, fd, unix.O_CLOEXEC); err != nil {
			t.Fatalf("dup3: %v", err)
		}
	}
	return x0, x1
}

// newReadLoopTestLoop starts a test engine and returns its worker loop 0,
// with one idle driver conn (the keeper) registered on it for the whole
// test.
//
// These tests register conns whose peer has already written. RegisterConn
// arms the descriptor (EPOLL_CTL_ADD) before it sets hasDriverConns, so on a
// loop with no driver conn yet, a worker that takes the conn's first
// (edge-triggered) event in between skips the driver lookup and drops the
// event, and onRecv never runs. That is a separate defect (celeris#770).
// The keeper keeps hasDriverConns set, so these tests do not depend on it: a
// racing worker waits in lookupDriver until RegisterConn releases driverMu,
// with the conn in the map.
//
// stop runs from a Cleanup, not a defer: the tests release a parked worker
// from a Cleanup registered later, which runs first, so stop never waits on
// a parked worker.
func newReadLoopTestLoop(t *testing.T) engine.WorkerLoop {
	t.Helper()
	eng, stop := newTestEngine(t)
	t.Cleanup(stop)
	wl := eng.WorkerLoop(0)
	keeper, keeperPeer := socketpairNonblocking(t)
	if err := wl.RegisterConn(keeper, func([]byte) {}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn(keeper): %v", err)
	}
	t.Cleanup(func() {
		_ = wl.UnregisterConn(keeper)
		_ = unix.Close(keeper)
		_ = unix.Close(keeperPeer)
	})
	return wl
}

// closeX710 closes X's two ends, and fd too when dup3 put X there as a
// second number. When X was created on fd itself, fd is x0, and closing it
// twice could close whatever took the number in between.
func closeX710(fd, x0, x1 int) {
	_ = unix.Close(x0)
	_ = unix.Close(x1)
	if x0 != fd {
		_ = unix.Close(fd)
	}
}

// fullBufferDriver registers a driver conn A on wl whose peer already holds
// more than one read buffer, so the worker's first read fills
// driverReadBufSize and the loop will read again after onRecv. A's first
// onRecv reports its length on entered and parks the worker until release is
// closed.
type fullBufferDriver struct {
	fd, peer int
	entered  chan int
	release  chan struct{}
	closed   chan error
	released bool
}

func newFullBufferDriver(t *testing.T, wl engine.WorkerLoop) *fullBufferDriver {
	t.Helper()
	d := &fullBufferDriver{
		entered: make(chan int, 1),
		release: make(chan struct{}),
		closed:  make(chan error, 1),
	}
	d.fd, d.peer = socketpairNonblocking(t)
	// Queue more than one read buffer before the conn is registered.
	payload := make([]byte, driverReadBufSize+100)
	for off := 0; off < len(payload); {
		n, err := unix.Write(d.peer, payload[off:])
		if err != nil {
			t.Fatalf("queue %d bytes on A: wrote %d, then %v", len(payload), off, err)
		}
		off += n
	}
	first := true
	if err := wl.RegisterConn(d.fd, func(b []byte) {
		if first {
			first = false
			d.entered <- len(b)
			<-d.release
		}
	}, func(err error) { d.closed <- err }); err != nil {
		t.Fatalf("RegisterConn(A): %v", err)
	}
	// A test that fails while parked must not leave the worker parked.
	t.Cleanup(d.releaseWorker)
	return d
}

// waitParked returns once the worker is inside A's first onRecv, and checks
// the precondition: that read filled the buffer, so the worker reads again
// after onRecv returns.
func (d *fullBufferDriver) waitParked(t *testing.T) {
	t.Helper()
	select {
	case n := <-d.entered:
		if n != driverReadBufSize {
			t.Fatalf("A's first read returned %d bytes, want a full buffer (%d): the worker "+
				"would not read again after onRecv, so the window is not open", n, driverReadBufSize)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the worker never entered A's onRecv")
	}
}

func (d *fullBufferDriver) releaseWorker() {
	if !d.released {
		d.released = true
		close(d.release)
	}
}

// promptly runs f on a new goroutine and fails the test if it has not
// returned within 2 s. While the worker is parked inside a conn's onRecv, a
// call that blocks on it is a lock held across the callback.
func promptly(t *testing.T, what string, f func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- f() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("%s blocked while the worker was inside the conn's onRecv", what)
	}
}

// workerRoundTrip returns once the worker goroutine has served an event that
// became ready after this call started: a conn R whose peer has already
// written a byte is registered, and its onRecv runs on the worker. R is then
// unregistered and closed. It reports false if R's byte did not reach onRecv
// within timeout, which means the worker is stuck.
func workerRoundTrip(t *testing.T, wl engine.WorkerLoop, timeout time.Duration) bool {
	t.Helper()
	r, peer := socketpairNonblocking(t)
	defer func() { _ = unix.Close(r); _ = unix.Close(peer) }()
	if _, err := unix.Write(peer, []byte{'r'}); err != nil {
		t.Fatalf("round trip: write R's peer: %v", err)
	}
	got := make(chan struct{}, 1)
	if err := wl.RegisterConn(r, func([]byte) {
		select {
		case got <- struct{}{}:
		default:
		}
	}, func(error) {}); err != nil {
		t.Fatalf("round trip: RegisterConn(R): %v", err)
	}
	defer func() { _ = wl.UnregisterConn(r) }()
	select {
	case <-got:
		return true
	case <-time.After(timeout):
		return false
	}
}

// readAvailable reads what fd holds now, without waiting.
func readAvailable(fd int) (string, error) {
	var b [64]byte
	n, err := unix.Read(fd, b[:])
	if err != nil {
		return "", err
	}
	return string(b[:n]), nil
}

// The issue's probe. X, a non-blocking socket, takes A's number while the
// worker is inside A's read loop, and X's peer writes. Before the fix the
// worker's next read took X's bytes and dropped them.
func TestDriverUnregisterInReadLoopSparesReusedNumber(t *testing.T) {
	wl := newReadLoopTestLoop(t)

	a := newFullBufferDriver(t, wl)
	defer func() { _ = unix.Close(a.peer) }()
	a.waitParked(t)

	// The worker is inside A's onRecv. The caller unregisters (onClose fires
	// before UnregisterConn returns, on this engine), closes fd, and a new
	// socket X takes the number. X's peer writes.
	promptly(t, "UnregisterConn(A)", func() error { return wl.UnregisterConn(a.fd) })
	select {
	case err := <-a.closed:
		if err != nil {
			t.Fatalf("A's onClose: %v, want nil", err)
		}
	default:
		t.Fatal("UnregisterConn(A) returned before A's onClose fired")
	}
	_ = unix.Close(a.fd)
	x0, x1 := takeNumber710(t, a.fd, true)
	defer closeX710(a.fd, x0, x1)
	const xBytes = "XXXXXXXXXX"
	if _, err := unix.Write(x1, []byte(xBytes)); err != nil {
		t.Fatalf("write X's peer: %v", err)
	}

	a.releaseWorker()
	if !workerRoundTrip(t, wl, 5*time.Second) {
		t.Fatal("the worker did not serve another conn within 5 s of leaving A's onRecv")
	}

	// The worker has left A's read loop. X still holds its bytes.
	got, err := readAvailable(a.fd)
	if got != xBytes {
		t.Errorf("X, which took A's number after UnregisterConn(A) returned, reads %q (err %v), "+
			"want %q: the worker read X's bytes through A's old number and dropped them (celeris#710)",
			got, err, xBytes)
	}
}

// The blocking face. X takes A's number as a BLOCKING socket with nothing to
// read. Before the fix the worker's next read blocked in X, and the worker
// served nothing else until X got data.
func TestDriverUnregisterInReadLoopNeverBlocksOnReusedNumber(t *testing.T) {
	wl := newReadLoopTestLoop(t)

	a := newFullBufferDriver(t, wl)
	defer func() { _ = unix.Close(a.peer) }()
	a.waitParked(t)

	promptly(t, "UnregisterConn(A)", func() error { return wl.UnregisterConn(a.fd) })
	_ = unix.Close(a.fd)
	x0, x1 := takeNumber710(t, a.fd, false)
	defer closeX710(a.fd, x0, x1)

	a.releaseWorker()
	served := workerRoundTrip(t, wl, 3*time.Second)
	if !served {
		// Unpark the worker so the engine can stop: X's byte ends its read.
		_, _ = unix.Write(x1, []byte{'u'})
		t.Fatal("the worker served no other conn for 3 s after leaving A's onRecv: it is blocked " +
			"reading X, the blocking socket that took A's number after UnregisterConn(A) returned (celeris#710)")
	}
	// Nothing read X: its peer's first byte is still X's to read.
	if _, err := unix.Write(x1, []byte{'y'}); err != nil {
		t.Fatalf("write X's peer: %v", err)
	}
	if err := unix.SetNonblock(a.fd, true); err != nil {
		t.Fatalf("setnonblock X: %v", err)
	}
	if got, err := readAvailable(a.fd); got != "y" {
		t.Errorf("X reads %q (err %v), want \"y\"", got, err)
	}
}

// The control: the same park and the same X, but the caller unregisters and
// closes only after the worker has left A's read loop. It passes with or
// without the fix, so a failure of the two tests above is about the worker
// reading inside that window, not about the apparatus (the park, dup3, the
// round trip, X's reader).
func TestDriverUnregisterAfterReadLoopControl(t *testing.T) {
	wl := newReadLoopTestLoop(t)

	a := newFullBufferDriver(t, wl)
	defer func() { _ = unix.Close(a.peer) }()
	a.waitParked(t)
	a.releaseWorker()
	if !workerRoundTrip(t, wl, 5*time.Second) {
		t.Fatal("the worker did not serve another conn within 5 s of leaving A's onRecv")
	}

	promptly(t, "UnregisterConn(A)", func() error { return wl.UnregisterConn(a.fd) })
	_ = unix.Close(a.fd)
	x0, x1 := takeNumber710(t, a.fd, true)
	defer closeX710(a.fd, x0, x1)
	const xBytes = "XXXXXXXXXX"
	if _, err := unix.Write(x1, []byte(xBytes)); err != nil {
		t.Fatalf("write X's peer: %v", err)
	}
	if !workerRoundTrip(t, wl, 5*time.Second) {
		t.Fatal("the worker did not serve another conn within 5 s")
	}
	if got, err := readAvailable(a.fd); got != xBytes {
		t.Errorf("control: X reads %q (err %v), want %q", got, err, xBytes)
	}
}

// Write and UnregisterConn from the driver's goroutine while the worker is
// inside the conn's onRecv must return at once: onRecv runs outside dc.mu.
// The fix takes dc.mu across each read of the loop; had it held the lock
// across the callback as well, both calls would wait for the callback, and
// this test would fail on its 2 s bound instead of hanging.
func TestDriverWriteAndUnregisterDuringOnRecvDoNotWait(t *testing.T) {
	wl := newReadLoopTestLoop(t)

	a := newFullBufferDriver(t, wl)
	defer func() { _ = unix.Close(a.peer) }()
	a.waitParked(t)

	promptly(t, "Write(A)", func() error { return wl.Write(a.fd, []byte("w")) })
	promptly(t, "UnregisterConn(A)", func() error { return wl.UnregisterConn(a.fd) })
	if err := wl.Write(a.fd, []byte("w")); !errors.Is(err, engine.ErrUnknownFD) {
		t.Errorf("Write(A) after UnregisterConn: %v, want ErrUnknownFD", err)
	}
	a.releaseWorker()
	if !workerRoundTrip(t, wl, 5*time.Second) {
		t.Fatal("the worker did not serve another conn within 5 s of leaving A's onRecv")
	}
	_ = unix.Close(a.fd)
	// A's peer got the one byte written before the unregister.
	if got, err := readAvailable(a.peer); got != "w" {
		t.Errorf("A's peer reads %q (err %v), want \"w\"", got, err)
	}
}
