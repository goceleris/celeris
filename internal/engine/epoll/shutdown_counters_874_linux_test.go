//go:build linux

package epoll

import (
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/engine"
)

// celeris#874 (epoll half, from #819 item 1). The run loop moves the three
// per-iteration batches (reqBatch, bytesReadBatch, bytesWrittenBatch) into the
// shared atomics once per iteration, and has returned by the time shutdown()
// runs. drainSends flushes the responses still queued with onLoopThread=true,
// which adds to bytesWrittenBatch, so every byte the shutdown drain sent was
// counted nowhere: Metrics().BytesWritten read after Shutdown fell short of
// what went out, by up to whole responses.

// drainRig874 is a shutdown-ready loop with one conn holding `queued` bytes
// the socket cannot take at once, and a reader on the peer that counts what
// arrives until the loop closes the conn.
type drainRig874 struct {
	l    *Loop
	fd   int
	peer int
	got  atomic.Int64
	done chan struct{}
}

func newDrainRig874(t *testing.T, queued int) *drainRig874 {
	t.Helper()
	l := shutdownLoop863(t, nil)
	l.async = false
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		t.Skipf("socketpair unavailable: %v", err)
	}
	r := &drainRig874{l: l, fd: pair[0], peer: pair[1], done: make(chan struct{})}
	t.Cleanup(func() { _ = unix.Close(r.peer) })
	if r.fd >= len(l.conns) {
		_ = unix.Close(r.fd)
		t.Skipf("socketpair fd %d outside the test conn table", r.fd)
	}
	_ = unix.SetsockoptInt(r.fd, unix.SOL_SOCKET, unix.SO_SNDBUF, 32<<10)
	cs := acquireConnState(t.Context(), r.fd, 4096, false)
	cs.protocol = engine.HTTP1
	cs.detected = true
	cs.h1State = conn.NewH1State()
	cs.remoteAddr = "127.0.0.1:9"
	cs.writeBuf = append(cs.writeBuf[:0], make([]byte, queued)...)
	l.conns[r.fd] = cs
	l.addLiveConn(cs)
	l.connCount++
	l.activeConns.Add(1)
	// The handler's response flushed as far as the socket takes it, then the
	// end of the loop iteration that did it: what run() does before the next
	// epoll_wait.
	if err := l.flushWrites(cs, true); err != nil {
		t.Fatalf("flushWrites: %v", err)
	}
	if !csWritePending(cs) {
		t.Fatalf("celeris874 PREMISE: %d queued bytes all went out; the socket must be too small", queued)
	}
	l.bytesWritten.Add(l.bytesWrittenBatch)
	l.bytesWrittenBatch = 0
	return r
}

// startReader reads the peer until the conn is closed (EOF) and counts bytes.
func (r *drainRig874) startReader() {
	go func() {
		defer close(r.done)
		buf := make([]byte, 64<<10)
		fds := []unix.PollFd{{Fd: int32(r.peer), Events: unix.POLLIN}}
		for {
			n, err := unix.Read(r.peer, buf)
			switch {
			case n > 0:
				r.got.Add(int64(n))
			case n == 0 && err == nil:
				return // EOF: shutdown closed the conn
			case err == unix.EAGAIN:
				_, _ = unix.Poll(fds, 20)
			case err == unix.EINTR:
			default:
				return
			}
		}
	}()
}

func (r *drainRig874) wait(t *testing.T) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(20 * time.Second):
		t.Fatal("the reader never saw the conn close")
	}
}

// TestShutdownCountsTheBytesItsSendDrainSends is the issue: every byte the
// shutdown drain sent must be in BytesWritten once shutdown has returned, so
// BytesWritten equals what the client received.
func TestShutdownCountsTheBytesItsSendDrainSends(t *testing.T) {
	const queued = 2 << 20
	r := newDrainRig874(t, queued)
	flushedBefore := r.l.bytesWritten.Load() // what the run loop counted before shutdown
	r.startReader()

	r.l.shutdown()
	r.wait(t)

	got := uint64(r.got.Load())
	counted := r.l.bytesWritten.Load()
	if got != queued {
		t.Fatalf("celeris874 PREMISE: the client received %d bytes, want the %d queued; the drain did not finish", got, queued)
	}
	if got <= flushedBefore {
		t.Fatalf("celeris874 PREMISE: the drain sent no bytes (client got %d, %d counted before shutdown)", got, flushedBefore)
	}
	if counted != got {
		t.Errorf("BytesWritten = %d after shutdown, but the client received %d: the %d bytes the shutdown drain sent "+
			"were added to a batch no iteration will flush (celeris#874)", counted, got, int64(got)-int64(counted))
	}
}

// TestShutdownFlushesEveryBatchedCounter pins the whole class rather than the
// drain's one instance: whatever the per-iteration batches hold when the loop
// stops reaches the shared counters, requests and received bytes too.
func TestShutdownFlushesEveryBatchedCounter(t *testing.T) {
	l := shutdownLoop863(t, nil)
	l.async = false
	l.reqBatch, l.bytesReadBatch, l.bytesWrittenBatch = 3, 5, 7

	l.shutdown()

	if got := l.reqCount.Load(); got != 3 {
		t.Errorf("reqCount = %d, want 3: reqBatch was dropped at shutdown", got)
	}
	if got := l.bytesRead.Load(); got != 5 {
		t.Errorf("bytesRead = %d, want 5: bytesReadBatch was dropped at shutdown", got)
	}
	if got := l.bytesWritten.Load(); got != 7 {
		t.Errorf("bytesWritten = %d, want 7: bytesWrittenBatch was dropped at shutdown", got)
	}
	if l.reqBatch != 0 || l.bytesReadBatch != 0 || l.bytesWrittenBatch != 0 {
		t.Errorf("batches not reset: req=%d read=%d written=%d", l.reqBatch, l.bytesReadBatch, l.bytesWrittenBatch)
	}
}
