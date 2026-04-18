//go:build linux

package eventloop

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
)

// socketPair returns a non-blocking AF_UNIX socketpair.
func socketPair(t *testing.T) (int, int) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	if err := unix.SetNonblock(fds[0], true); err != nil {
		t.Fatalf("nonblock[0]: %v", err)
	}
	if err := unix.SetNonblock(fds[1], true); err != nil {
		t.Fatalf("nonblock[1]: %v", err)
	}
	return fds[0], fds[1]
}

func TestLinuxSocketpairRoundtrip(t *testing.T) {
	l, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	a, b := socketPair(t)
	t.Cleanup(func() {
		_ = unix.Close(a)
		_ = unix.Close(b)
	})

	recvCh := make(chan []byte, 4)
	closeCh := make(chan error, 1)
	w := l.WorkerLoop(0)
	if err := w.RegisterConn(a, func(buf []byte) {
		cp := append([]byte(nil), buf...)
		recvCh <- cp
	}, func(err error) {
		closeCh <- err
	}); err != nil {
		t.Fatalf("RegisterConn: %v", err)
	}

	if _, err := unix.Write(b, []byte("ping")); err != nil {
		t.Fatalf("peer write: %v", err)
	}
	select {
	case got := <-recvCh:
		if string(got) != "ping" {
			t.Fatalf("recv %q want ping", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("recv timeout")
	}
	if err := w.Write(a, []byte("pong")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	buf := make([]byte, 16)
	// Peer is non-blocking; spin briefly.
	deadline := time.Now().Add(2 * time.Second)
	var n int
	for time.Now().Before(deadline) {
		n, err = unix.Read(b, buf)
		if n > 0 {
			break
		}
		if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
			time.Sleep(time.Millisecond)
			continue
		}
		if err != nil {
			t.Fatalf("peer read: %v", err)
		}
	}
	if string(buf[:n]) != "pong" {
		t.Fatalf("peer got %q want pong", buf[:n])
	}

	if err := w.UnregisterConn(a); err != nil {
		t.Fatalf("UnregisterConn: %v", err)
	}
	select {
	case err := <-closeCh:
		if err != nil {
			t.Fatalf("onClose err: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close timeout")
	}
}

func TestLinuxWriteBackpressure(t *testing.T) {
	l, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	a, b := socketPair(t)
	t.Cleanup(func() {
		_ = unix.Close(a)
		_ = unix.Close(b)
	})

	// Shrink the peer's receive buffer so we overflow quickly without
	// writing megabytes.
	_ = unix.SetsockoptInt(b, unix.SOL_SOCKET, unix.SO_RCVBUF, 4096)
	_ = unix.SetsockoptInt(a, unix.SOL_SOCKET, unix.SO_SNDBUF, 4096)

	w := l.WorkerLoop(0)
	var closedCount atomic.Int32
	if err := w.RegisterConn(a, func([]byte) {}, func(err error) {
		closedCount.Add(1)
	}); err != nil {
		t.Fatalf("RegisterConn: %v", err)
	}

	// Fill far beyond maxPendingBytes without reading on the peer. We
	// expect ErrQueueFull eventually.
	payload := make([]byte, 64<<10)
	for i := 0; i < (maxPendingBytes/len(payload))+64; i++ {
		err := w.Write(a, payload)
		if err == nil {
			continue
		}
		if errors.Is(err, engine.ErrQueueFull) {
			// Success — backpressure signalled.
			_ = w.UnregisterConn(a)
			return
		}
		t.Fatalf("unexpected Write error: %v", err)
	}
	t.Fatal("never observed ErrQueueFull despite > maxPendingBytes pending")
}

// TestStandaloneEAGAINWake verifies that a Write that hits EAGAIN (peer
// send buffer full) eventually completes once the peer drains the buffer.
// Regression for a bug where flushLocked returned nil on EAGAIN without
// arming EPOLLOUT, leaving the worker goroutine unable to resume the write
// when the socket became writable again.
func TestStandaloneEAGAINWake(t *testing.T) {
	l, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	a, b := socketPair(t)
	t.Cleanup(func() {
		_ = unix.Close(a)
		_ = unix.Close(b)
	})

	// Shrink buffers so we hit EAGAIN quickly.
	_ = unix.SetsockoptInt(b, unix.SOL_SOCKET, unix.SO_RCVBUF, 4096)
	_ = unix.SetsockoptInt(a, unix.SOL_SOCKET, unix.SO_SNDBUF, 4096)

	w := l.WorkerLoop(0)
	if err := w.RegisterConn(a, func([]byte) {}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn: %v", err)
	}
	t.Cleanup(func() { _ = w.UnregisterConn(a) })

	// Write a payload that is guaranteed to exceed both socket buffers,
	// forcing at least one EAGAIN while the peer hasn't read yet.
	const total = 256 << 10
	payload := make([]byte, total)
	for i := range payload {
		payload[i] = byte(i)
	}
	if err := w.Write(a, payload); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Drain from the peer. Before the fix, the worker never got an
	// EPOLLOUT notification, so the writeBuf stays partially flushed
	// forever and this test times out.
	buf := make([]byte, 4096)
	var got int
	deadline := time.Now().Add(5 * time.Second)
	for got < total && time.Now().Before(deadline) {
		n, rerr := unix.Read(b, buf)
		if n > 0 {
			got += n
			continue
		}
		if rerr == unix.EAGAIN || rerr == unix.EWOULDBLOCK {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		if rerr != nil {
			t.Fatalf("peer read: %v", rerr)
		}
	}
	if got != total {
		t.Fatalf("drained %d/%d bytes — EPOLLOUT rearm likely missing", got, total)
	}
}

func TestLinuxAlreadyRegistered(t *testing.T) {
	l, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	a, b := socketPair(t)
	t.Cleanup(func() {
		_ = unix.Close(a)
		_ = unix.Close(b)
	})

	var closeCh = make(chan error, 1)
	var fired sync.Once
	w := l.WorkerLoop(0)
	cb := func(err error) { fired.Do(func() { closeCh <- err }) }
	if err := w.RegisterConn(a, func([]byte) {}, cb); err != nil {
		t.Fatalf("RegisterConn: %v", err)
	}
	if err := w.RegisterConn(a, func([]byte) {}, cb); !errors.Is(err, ErrAlreadyRegistered) {
		t.Fatalf("duplicate: got %v want ErrAlreadyRegistered", err)
	}
	_ = w.UnregisterConn(a)
	select {
	case <-closeCh:
	case <-time.After(time.Second):
		t.Fatal("close timeout")
	}
}

func TestWriteAndPollSyncPath(t *testing.T) {
	l, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	a, b := socketPair(t)
	t.Cleanup(func() {
		_ = unix.Close(a)
		_ = unix.Close(b)
	})

	w := l.WorkerLoop(0)
	if err := w.RegisterConn(a, func([]byte) {}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn: %v", err)
	}
	t.Cleanup(func() { _ = w.UnregisterConn(a) })

	srt, ok := w.(SyncRoundTripper)
	if !ok {
		t.Fatalf("worker %T does not implement SyncRoundTripper", w)
	}

	// Stage the response on the peer side BEFORE calling WriteAndPoll, and
	// verify the kernel has delivered it to our end of the socketpair
	// before issuing the request. The earlier "50µs sleep + goroutine"
	// shape was flaky on loaded CI runners — the goroutine wouldn't get
	// scheduled before Phase C's poll(1ms) timed out.
	if _, werr := unix.Write(b, []byte("pong")); werr != nil {
		t.Fatalf("peer write: %v", werr)
	}
	// Wait for the write to propagate to our end. On -race builds with
	// high scheduler jitter the write buffer can take a scheduler tick
	// to flip visible; poll() with a 50ms ceiling is a deterministic
	// hand-off signal that doesn't depend on timer resolution.
	var waitPfd [1]unix.PollFd
	waitPfd[0].Fd = int32(a)
	waitPfd[0].Events = unix.POLLIN
	if _, perr := unix.Poll(waitPfd[:], 50); perr != nil {
		t.Fatalf("poll for peer write: %v", perr)
	}
	if waitPfd[0].Revents&unix.POLLIN == 0 {
		t.Fatal("peer write did not become readable within 50ms")
	}

	var got []byte
	rbuf := make([]byte, 128)
	ok2, err := srt.WriteAndPoll(a, []byte("ping"), rbuf, func(data []byte) {
		got = append(got, data...)
	})
	if err != nil {
		t.Fatalf("WriteAndPoll err: %v", err)
	}
	if !ok2 {
		t.Fatal("WriteAndPoll returned ok=false; sync fast path not engaged")
	}
	if string(got) != "pong" {
		t.Fatalf("got %q, want pong", got)
	}

	// Verify the write arrived at the peer.
	peerBuf := make([]byte, 128)
	deadline := time.Now().Add(time.Second)
	var peerN int
	for time.Now().Before(deadline) {
		n, rerr := unix.Read(b, peerBuf)
		if n > 0 {
			peerN = n
			break
		}
		if rerr == unix.EAGAIN {
			time.Sleep(time.Millisecond)
			continue
		}
		if rerr != nil {
			t.Fatalf("peer read: %v", rerr)
		}
	}
	if string(peerBuf[:peerN]) != "ping" {
		t.Fatalf("peer got %q, want ping", peerBuf[:peerN])
	}
}
