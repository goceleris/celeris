//go:build linux

package epoll

import (
	"bytes"
	"sync/atomic"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/engine"
)

// Deterministic tests of the HTTP/2 parts of celeris#805, which the end-to-end
// TestLargeResponseIsDeliveredH2 caught only intermittently when they were
// mutated (celeris#889 item 1): the 64 MiB cap on a conn's unsent bytes
// (writeCap, maxPendingBytesH2), the resync of pendingBytes after the loop's
// write-queue pass, and the close of a conn whose write was refused. They run
// the real pass (drainH2Writes, which run calls) on a conn built by hand over a
// socketpair, with frames put on its H2 write queue the way a handler
// goroutine's response is (H2State.EnqueueWrite), so nothing depends on a
// timing or on how fast a peer reads.

// h2Rig889 is a loop with one H2 conn on a socketpair: peer is the other end.
type h2Rig889 struct {
	l    *Loop
	cs   *connState
	peer int
	// asks counts the closeWhenFlushed calls of the conn.
	asks atomic.Int32
}

func newH2Rig889(t *testing.T) *h2Rig889 {
	t.Helper()
	l := newReapLoop(t)
	local, peer := socketpairNonblocking(t)
	t.Cleanup(func() { _ = unix.Close(peer) })
	if local >= connTableSize {
		_ = unix.Close(local)
		t.Skip("fd out of table range")
	}
	cs := &connState{fd: local, liveIdx: -1}
	cs.writeFn = l.makeWriteFn(cs)
	cs.h2State = conn.NewH2State(&asyncReuseHandler{}, conn.H2Config{}, cs.writeFn, nil)
	cs.protocol = engine.H2C
	registerForOut885(t, l, cs)
	l.conns[local] = cs
	l.addLiveConn(cs)
	l.connCount++
	l.activeConns.Add(1)
	l.h2Conns = append(l.h2Conns, local)
	r := &h2Rig889{l: l, cs: cs, peer: peer}
	hook := func(c *connState) {
		if c == cs {
			r.asks.Add(1)
		}
	}
	testHookCloseWhenFlushed.Store(&hook)
	t.Cleanup(func() {
		testHookCloseWhenFlushed.Store(nil)
		if l.conns[local] != nil {
			l.closeConn(local)
		}
	})
	return r
}

// queue puts n frames of size bytes each on the conn's H2 write queue.
func (r *h2Rig889) queue(n, size int) {
	f := bytes.Repeat([]byte{'f'}, size)
	for i := 0; i < n; i++ {
		r.cs.h2State.EnqueueWrite(1, f)
	}
}

// TestH2WriteCapIs64MiB889: an HTTP/2 conn's write hook accepts a frame while
// the bytes still unsent before it are at most maxPendingBytesH2 (64 MiB) and
// refuses it, recording the refusal, once they are over it. A cap that is lower
// refuses a healthy conn at 32 MiB, a cap that is gone accepts at 64 MiB + 1.
func TestH2WriteCapIs64MiB889(t *testing.T) {
	const frame = 4096
	for _, tc := range []struct {
		name        string
		pending     int
		wantRefused bool
	}{
		{"32 MiB unsent: accepted", 32 << 20, false},
		{"64 MiB unsent: accepted", 64 << 20, false},
		{"64 MiB + 1 unsent: refused", 64<<20 + 1, true},
		{"128 MiB unsent: refused", 128 << 20, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newH2Rig889(t)
			cs := r.cs
			if got := cs.writeCap(); got != 64<<20 {
				t.Fatalf("writeCap of an H2 conn = %d, want %d", got, 64<<20)
			}
			cs.pendingBytes = tc.pending
			before := len(cs.writeBuf)
			cs.writeFn(make([]byte, frame))
			if cs.writeRefused != tc.wantRefused {
				t.Fatalf("writeRefused = %v with %d bytes unsent, want %v", cs.writeRefused, tc.pending, tc.wantRefused)
			}
			if grew := len(cs.writeBuf) - before; tc.wantRefused && grew != 0 || !tc.wantRefused && grew != frame {
				t.Errorf("the frame grew writeBuf by %d bytes (refused=%v)", grew, tc.wantRefused)
			}
		})
	}
}

// TestH2WritePassResyncsPendingBytes889: the write hook adds every frame the
// pass drains to pendingBytes, and only a flush point brings it back to what is
// really queued; left alone it outgrew the queue until the hook refused frames
// of a conn with nothing queued (celeris#761). After the pass pendingBytes is
// what is still queued: nothing after a flush the socket took whole, the
// remainder after one it took part of.
func TestH2WritePassResyncsPendingBytes889(t *testing.T) {
	t.Run("flushed whole", func(t *testing.T) {
		r := newH2Rig889(t)
		r.cs.pendingBytes = 1 << 20 // stale: what earlier frames added and no flush point took back
		r.queue(3, 1000)
		r.l.drainH2Writes()
		if csPendingBytes(r.cs) != 0 {
			t.Fatalf("the pass left %d bytes queued on a socket with room", csPendingBytes(r.cs))
		}
		if r.cs.pendingBytes != 0 {
			t.Errorf("pendingBytes = %d after the pass flushed all 3000 bytes, want 0 (not resynced)", r.cs.pendingBytes)
		}
		var got bytes.Buffer
		drainPeer(t, r.peer, &got)
		if got.Len() != 3000 {
			t.Errorf("the peer received %d bytes, want 3000", got.Len())
		}
	})
	t.Run("flushed in part", func(t *testing.T) {
		r := newH2Rig889(t)
		fillSendBuffer(t, r.cs.fd)  // the socket takes nothing more
		r.cs.pendingBytes = 1 << 20 // stale, as above
		r.queue(3, 1000)
		r.l.drainH2Writes()
		queued := csPendingBytes(r.cs)
		if queued == 0 {
			t.Fatal("the full socket took the frames: backpressure not reproduced")
		}
		if r.cs.pendingBytes != queued {
			t.Errorf("pendingBytes = %d after the pass, want what is queued (%d)", r.cs.pendingBytes, queued)
		}
	})
}

// TestH2RefusedWriteClosesTheConn889: a frame the write hook refuses is lost,
// and the framing of the conn with it, so the pass closes the conn once what
// was staged has gone out (closeWhenFlushed), asks once however many frames the
// handlers go on queueing (celeris#885 item 38), and drops those frames instead
// of appending them behind the cut ones.
func TestH2RefusedWriteClosesTheConn889(t *testing.T) {
	r := newH2Rig889(t)
	cs := r.cs
	fillSendBuffer(t, cs.fd) // staged bytes stay queued: the close is deferred
	// A backlog over the cap: the next frame is refused.
	cs.writeBuf = append(cs.writeBuf, make([]byte, 1024)...)
	cs.pendingBytes = 64<<20 + 1
	r.queue(1, 500)
	staged := len(cs.writeBuf)
	r.l.drainH2Writes()
	if !cs.writeRefused {
		t.Fatal("the frame over the cap was not refused")
	}
	if got := r.asks.Load(); got != 1 {
		t.Fatalf("closeWhenFlushed asked %d times after the refusal, want 1", got)
	}
	if !cs.peerClosed || cs.closeSince == 0 {
		t.Errorf("the close was not deferred behind the staged bytes (peerClosed=%v closeSince=%d)", cs.peerClosed, cs.closeSince)
	}
	if len(cs.writeBuf) != staged {
		t.Errorf("the refused frame reached writeBuf: %d bytes, want %d", len(cs.writeBuf), staged)
	}
	// The handlers go on queueing: dropped, and the close is not asked again.
	cs.pendingBytes = 0 // the backlog has fallen below the cap
	r.queue(5, 500)
	r.l.drainH2Writes()
	if got := r.asks.Load(); got != 1 {
		t.Errorf("closeWhenFlushed asked %d times after more frames, want 1", got)
	}
	if len(cs.writeBuf) != staged {
		t.Errorf("frames queued after the refusal were appended: writeBuf %d bytes, want %d", len(cs.writeBuf), staged)
	}
}
