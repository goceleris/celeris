//go:build linux

package eventloop

// celeris#1357: the standalone loop capped a conn's pending outbound bytes at
// 4 MiB, so a single write of 4 MiB or more (a Redis SET of an 8 MiB value)
// failed with "worker write queue full" where the engine-hosted paths take
// 64 MiB. The cap is now the engines' 64 MiB, a write above it names the limit,
// and a conn does not keep a big buffer after the write has drained.

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
)

// drainPeer reads fd until want bytes arrived (or the deadline), failing on a
// byte that is not 'x', and returns the count.
func drainPeer(t *testing.T, fd, want int) int {
	t.Helper()
	got := 0
	buf := make([]byte, 256<<10)
	deadline := time.Now().Add(20 * time.Second)
	for got < want && time.Now().Before(deadline) {
		n, err := unix.Read(fd, buf)
		if err == unix.EAGAIN {
			time.Sleep(200 * time.Microsecond)
			continue
		}
		if err != nil {
			t.Fatalf("peer read after %d bytes: %v", got, err)
		}
		if n == 0 {
			break
		}
		if !bytes.Equal(buf[:n], bytes.Repeat([]byte{'x'}, n)) {
			t.Fatalf("peer read corrupt bytes at offset %d", got)
		}
		got += n
	}
	return got
}

func newWriteCapRig(t *testing.T) (w *worker, a, b int) {
	t.Helper()
	l, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	a, b = socketPair(t)
	t.Cleanup(func() { _ = unix.Close(a); _ = unix.Close(b) })
	wl := l.WorkerLoop(0)
	if err := wl.RegisterConn(a, func([]byte) {}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn: %v", err)
	}
	t.Cleanup(func() { _ = wl.UnregisterConn(a) })
	return wl.(*worker), a, b
}

func TestStandaloneWriteOf8MiBIsAccepted1357(t *testing.T) {
	w, a, b := newWriteCapRig(t)
	payload := bytes.Repeat([]byte{'x'}, 8<<20)
	if err := w.Write(a, payload); err != nil {
		t.Fatalf("Write of 8 MiB: %v", err)
	}
	if got := drainPeer(t, b, len(payload)); got != len(payload) {
		t.Fatalf("peer received %d bytes, want %d", got, len(payload))
	}
}

func TestStandaloneWriteAboveTheLimitNamesIt1357(t *testing.T) {
	w, a, _ := newWriteCapRig(t)
	err := w.Write(a, make([]byte, 64<<20+1))
	if !errors.Is(err, engine.ErrQueueFull) {
		t.Fatalf("Write of 64 MiB + 1: err = %v, want a match for engine.ErrQueueFull", err)
	}
	if !strings.Contains(err.Error(), "exceeds the per-write limit of 64 MiB") {
		t.Fatalf("message %q does not name the limit", err)
	}
	// Nothing was queued: the conn is still usable.
	if err := w.Write(a, []byte("x")); err != nil {
		t.Fatalf("Write after the refused one: %v", err)
	}
}

// A write that fits by itself but not beside what is pending is backpressure:
// the bare sentinel, retry later.
func TestStandaloneWriteBehindPendingStaysTheBareSentinel1357(t *testing.T) {
	w, a, b := newWriteCapRig(t)
	_ = unix.SetsockoptInt(b, unix.SOL_SOCKET, unix.SO_RCVBUF, 4096)
	_ = unix.SetsockoptInt(a, unix.SOL_SOCKET, unix.SO_SNDBUF, 4096)
	chunk := make([]byte, 40<<20)
	if err := w.Write(a, chunk); err != nil {
		t.Fatalf("first 40 MiB: %v", err)
	}
	err := w.Write(a, chunk)
	if err != engine.ErrQueueFull { //nolint:errorlint // the point: the identity, not a wrap
		t.Fatalf("second 40 MiB behind a pending 40 MiB: err = %v, want engine.ErrQueueFull itself", err)
	}
}

// After a big write has drained the conn lets go of the buffer, so one 64 MiB
// frame does not cost 64 MiB for the life of the conn.
func TestStandaloneDoesNotKeepAHugeBufferAfterDrain1357(t *testing.T) {
	w, a, b := newWriteCapRig(t)
	payload := bytes.Repeat([]byte{'x'}, 32<<20)
	if err := w.Write(a, payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := drainPeer(t, b, len(payload)); got != len(payload) {
		t.Fatalf("peer received %d bytes, want %d", got, len(payload))
	}
	// The worker drains the tail on EPOLLOUT; wait for the buffer to be empty.
	deadline := time.Now().Add(5 * time.Second)
	for {
		w.mu.RLock()
		c := w.conns[a]
		w.mu.RUnlock()
		c.mu.Lock()
		pending, capNow := len(c.writeBuf)-c.writePos, cap(c.writeBuf)
		c.mu.Unlock()
		if pending == 0 && capNow <= 4<<20 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the drain the conn holds a %d-byte buffer (pending %d), want at most 4 MiB", capNow, pending)
		}
		time.Sleep(time.Millisecond)
	}
}
