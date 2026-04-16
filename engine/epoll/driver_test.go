//go:build linux

package epoll

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	celerisengine "github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// newTestEngine spins up a single-worker epoll engine bound to a free
// loopback port and returns the engine plus a stop func. The stream.Handler
// is a no-op because the driver tests never dispatch an HTTP request.
func newTestEngine(t *testing.T) (*Engine, func()) {
	t.Helper()

	// Pick a free port via net.Listen, close it, and hand the address to
	// the engine. SO_REUSEPORT makes the brief gap harmless.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfg := resource.Config{
		Addr: addr,
		Resources: resource.Resources{
			Workers: 2,
		},
	}
	eng, err := New(cfg, stream.HandlerFunc(func(_ context.Context, _ *stream.Stream) error { return nil }))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = eng.Listen(ctx)
		close(done)
	}()

	// Wait for the engine to bind.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if eng.Addr() != nil && eng.NumWorkers() > 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if eng.Addr() == nil {
		cancel()
		<-done
		t.Fatalf("engine did not bind within deadline")
	}

	return eng, func() {
		cancel()
		<-done
	}
}

// socketpairNonblocking creates a connected AF_UNIX SOCK_STREAM pair and
// returns (local, peer) where local is the fd handed to the engine and
// peer is the fd the test uses to read/write from the "other side".
func socketpairNonblocking(t *testing.T) (int, int) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	for _, fd := range fds {
		if err := unix.SetNonblock(fd, true); err != nil {
			t.Fatalf("setnonblock: %v", err)
		}
	}
	return fds[0], fds[1]
}

// readAll reads from peer with a short deadline, spinning on EAGAIN. The
// engine worker flushes via epoll, so there's no guaranteed sync point;
// we poll for up to timeout.
func readWithDeadline(t *testing.T, peer int, want int, timeout time.Duration) []byte {
	t.Helper()
	buf := make([]byte, 0, want)
	scratch := make([]byte, 4096)
	deadline := time.Now().Add(timeout)
	for len(buf) < want && time.Now().Before(deadline) {
		n, err := unix.Read(peer, scratch)
		if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
			time.Sleep(500 * time.Microsecond)
			continue
		}
		if err != nil {
			t.Fatalf("read peer: %v", err)
		}
		if n == 0 {
			break
		}
		buf = append(buf, scratch[:n]...)
	}
	return buf
}

func TestDriverRegisterUnregister(t *testing.T) {
	eng, stop := newTestEngine(t)
	defer stop()

	wl := eng.WorkerLoop(0)
	local, peer := socketpairNonblocking(t)
	defer unix.Close(local)
	defer unix.Close(peer)

	var (
		recvMu   sync.Mutex
		received []byte
		closed   atomic.Bool
		closeErr error
	)
	onRecv := func(b []byte) {
		recvMu.Lock()
		received = append(received, b...)
		recvMu.Unlock()
	}
	onClose := func(err error) {
		closeErr = err
		closed.Store(true)
	}

	if err := wl.RegisterConn(local, onRecv, onClose); err != nil {
		t.Fatalf("RegisterConn: %v", err)
	}

	// Peer -> engine
	msg := []byte("hello from peer")
	if _, err := unix.Write(peer, msg); err != nil {
		t.Fatalf("peer write: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		recvMu.Lock()
		got := append([]byte(nil), received...)
		recvMu.Unlock()
		if bytes.Equal(got, msg) {
			break
		}
		time.Sleep(1 * time.Millisecond)
	}
	recvMu.Lock()
	if !bytes.Equal(received, msg) {
		recvMu.Unlock()
		t.Fatalf("onRecv payload: got %q want %q", received, msg)
	}
	recvMu.Unlock()

	// Engine -> peer
	reply := []byte("hello from engine")
	if err := wl.Write(local, reply); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := readWithDeadline(t, peer, len(reply), 2*time.Second)
	if !bytes.Equal(got, reply) {
		t.Fatalf("peer recv: got %q want %q", got, reply)
	}

	if err := wl.UnregisterConn(local); err != nil {
		t.Fatalf("UnregisterConn: %v", err)
	}
	if !closed.Load() {
		t.Fatalf("onClose not invoked after UnregisterConn")
	}
	if closeErr != nil {
		t.Fatalf("onClose err: got %v want nil", closeErr)
	}
}

func TestDriverHTTPZeroOverhead(t *testing.T) {
	// This test asserts the zero-cost gate: when no drivers are registered,
	// the HTTP dispatch path pays only a single atomic.Bool load. We verify
	// by inspecting the Loop state rather than micro-benchmarking (which is
	// noisy in CI). The gate is provably zero-cost because it's an atomic
	// load with no map access; we sanity-check the gate state transitions.
	eng, stop := newTestEngine(t)
	defer stop()

	l := eng.loops[0]
	if l.hasDriverConns.Load() {
		t.Fatalf("hasDriverConns should be false on fresh engine")
	}

	local, peer := socketpairNonblocking(t)
	defer unix.Close(local)
	defer unix.Close(peer)

	if err := eng.WorkerLoop(0).RegisterConn(local, func([]byte) {}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn: %v", err)
	}
	if !l.hasDriverConns.Load() {
		t.Fatalf("hasDriverConns should be true after register")
	}
	if err := eng.WorkerLoop(0).UnregisterConn(local); err != nil {
		t.Fatalf("UnregisterConn: %v", err)
	}
	if l.hasDriverConns.Load() {
		t.Fatalf("hasDriverConns should be false after unregister")
	}
}

func TestDriverWriteSerialization(t *testing.T) {
	eng, stop := newTestEngine(t)
	defer stop()

	local, peer := socketpairNonblocking(t)
	defer unix.Close(local)
	defer unix.Close(peer)

	// Enlarge peer's recv buffer so the test doesn't block on SO_SNDBUF
	// back-pressure (we want to exercise serialization, not flow control).
	_ = unix.SetsockoptInt(peer, unix.SOL_SOCKET, unix.SO_RCVBUF, 4<<20)

	if err := eng.WorkerLoop(0).RegisterConn(local, func([]byte) {}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn: %v", err)
	}

	// Each goroutine writes a distinct 4-byte fixed-width sequence number.
	// The receiver reads the full stream and checks: (a) total byte count
	// matches, (b) every 4-byte chunk is a contiguous sequence payload.
	const n = 100
	const payloadLen = 16
	writes := make([][]byte, n)
	for i := range writes {
		writes[i] = bytes.Repeat([]byte(fmt.Sprintf("%04d", i)), payloadLen/4)
	}

	// Start reader first so we don't lose bytes to buffer overflow.
	readDone := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 0, n*payloadLen)
		scratch := make([]byte, 4096)
		deadline := time.Now().Add(5 * time.Second)
		for len(buf) < n*payloadLen && time.Now().Before(deadline) {
			m, err := unix.Read(peer, scratch)
			if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
				time.Sleep(200 * time.Microsecond)
				continue
			}
			if err != nil || m == 0 {
				break
			}
			buf = append(buf, scratch[:m]...)
		}
		readDone <- buf
	}()

	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(data []byte) {
			defer wg.Done()
			if err := eng.WorkerLoop(0).Write(local, data); err != nil {
				t.Errorf("Write: %v", err)
			}
		}(writes[i])
	}
	wg.Wait()

	buf := <-readDone
	if len(buf) != n*payloadLen {
		t.Fatalf("total bytes: got %d want %d", len(buf), n*payloadLen)
	}
	// Each 16-byte chunk must be 4 copies of a valid sequence number. Order
	// across goroutines is nondeterministic; within a chunk, all 4 tags
	// must be identical (no interleaving within a single Write).
	seen := make(map[int]bool, n)
	for off := 0; off < len(buf); off += payloadLen {
		chunk := buf[off : off+payloadLen]
		tag := string(chunk[:4])
		for i := 4; i < payloadLen; i += 4 {
			if string(chunk[i:i+4]) != tag {
				t.Fatalf("chunk %d interleaved: %q", off/payloadLen, chunk)
			}
		}
		seq, err := strconv.Atoi(tag)
		if err != nil {
			t.Fatalf("chunk %d non-numeric tag %q", off/payloadLen, tag)
		}
		if seen[seq] {
			t.Fatalf("duplicate sequence %d", seq)
		}
		seen[seq] = true
	}
	if len(seen) != n {
		t.Fatalf("unique sequences: got %d want %d", len(seen), n)
	}
}

func TestDriverFDCollisionRejected(t *testing.T) {
	eng, stop := newTestEngine(t)
	defer stop()

	// Dialing a real conn would force the worker to write l.conns[fd] on
	// its own goroutine while this test reads it — a benign TOCTOU race
	// in production but a race-detector failure under -race. Instead,
	// plant a fake connState directly at a fd the worker never touches
	// (our own socketpair half). This still exercises RegisterConn's
	// HTTP-collision guard without racing with acceptAll/closeConn.
	local, peer := socketpairNonblocking(t)
	defer unix.Close(local)
	defer unix.Close(peer)

	l := eng.loops[0]
	if local >= connTableSize {
		t.Fatalf("socketpair fd %d exceeds connTableSize %d", local, connTableSize)
	}
	prev := l.conns[local]
	l.conns[local] = &connState{fd: local}
	defer func() { l.conns[local] = prev }()

	err := eng.WorkerLoop(0).RegisterConn(local, func([]byte) {}, func(error) {})
	if err == nil {
		_ = eng.WorkerLoop(0).UnregisterConn(local)
		t.Fatalf("RegisterConn on HTTP fd should have failed")
	}
}

func TestDriverCallbackOnWorkerGoroutine(t *testing.T) {
	eng, stop := newTestEngine(t)
	defer stop()

	// Stamp the worker goroutine ID when it first runs our callback, then
	// verify subsequent deliveries observe the same ID. The worker runs
	// one iteration per epoll_wait so all callbacks fire from it.
	local, peer := socketpairNonblocking(t)
	defer unix.Close(local)
	defer unix.Close(peer)

	var (
		firstGID atomic.Int64
		mismatch atomic.Bool
		recvWG   sync.WaitGroup
	)
	recvWG.Add(3)
	onRecv := func([]byte) {
		gid := currentGoroutineID()
		if firstGID.CompareAndSwap(0, gid) {
			recvWG.Done()
			return
		}
		if firstGID.Load() != gid {
			mismatch.Store(true)
		}
		recvWG.Done()
	}
	if err := eng.WorkerLoop(0).RegisterConn(local, onRecv, func(error) {}); err != nil {
		t.Fatalf("RegisterConn: %v", err)
	}

	for i := 0; i < 3; i++ {
		if _, err := unix.Write(peer, []byte{byte(i)}); err != nil {
			t.Fatalf("peer write: %v", err)
		}
		// Space writes so the worker processes them as distinct events.
		time.Sleep(10 * time.Millisecond)
	}

	done := make(chan struct{})
	go func() { recvWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("onRecv did not fire 3 times")
	}

	if mismatch.Load() {
		t.Fatalf("onRecv fired on more than one goroutine")
	}
}

func TestDriverUnknownFDReturnsErr(t *testing.T) {
	eng, stop := newTestEngine(t)
	defer stop()

	wl := eng.WorkerLoop(0)
	// Use an fd that's extremely unlikely to be live in our process.
	const bogus = 0x3FFE

	if err := wl.Write(bogus, []byte("x")); !errors.Is(err, celerisengine.ErrUnknownFD) {
		t.Fatalf("Write unknown fd: got %v want ErrUnknownFD", err)
	}
	if err := wl.UnregisterConn(bogus); !errors.Is(err, celerisengine.ErrUnknownFD) {
		t.Fatalf("UnregisterConn unknown fd: got %v want ErrUnknownFD", err)
	}
}

// currentGoroutineID parses "goroutine N [running]:" from the first line
// of runtime.Stack. Test-only; not used on the hot path.
func currentGoroutineID() int64 {
	// runtime.Callers doesn't expose GIDs; fall back to stack parsing.
	// debug.Stack() is the canonical way used by test infra.
	b := debug.Stack()
	// Format: "goroutine 42 [running]:\n..."
	const prefix = "goroutine "
	if !bytes.HasPrefix(b, []byte(prefix)) {
		return -1
	}
	b = b[len(prefix):]
	i := bytes.IndexByte(b, ' ')
	if i < 0 {
		return -1
	}
	gid, err := strconv.ParseInt(string(b[:i]), 10, 64)
	if err != nil {
		return -1
	}
	return gid
}

// Silence unused-import warnings when the test build trims functions.
var _ = runtime.NumCPU
