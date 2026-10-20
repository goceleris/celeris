//go:build linux

package iouring

import (
	"context"
	"testing"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// io_uring's half of the deterministic tests of the HTTP/2 parts of
// celeris#805 (celeris#889 item 1; the epoll half is in
// internal/engine/epoll/h2_write_pass_889_linux_test.go): the 64 MiB cap on a
// conn's unsent bytes (sendCap, maxSendQueueBytesH2) and the close of a conn
// whose write was refused, through the real pass (drainH2Writes, which run
// calls) on a conn built by hand, with frames put on its H2 write queue the way
// a handler goroutine's response is (H2State.EnqueueWrite). io_uring keeps no
// pendingBytes to resync: its backlog is the length of its buffers.

func newH2Worker889(t *testing.T) (*Worker, *connState) {
	t.Helper()
	w, cs := newClosingDrainWorker(t, newTestRing(t))
	// Nothing in flight: the conn the pass sees is idle until it is queued to.
	cs.sending, cs.kernelInflight, cs.sendBuf = false, 0, nil
	cs.lastActivity = w.cachedNow
	cs.writeFn = w.makeWriteFn(cs)
	cs.h2State = conn.NewH2State(&h2NopHandler889{}, conn.H2Config{}, cs.writeFn, nil)
	cs.protocol.Store(int32(engine.H2C))
	w.h2Conns = append(w.h2Conns, cs.fd)
	return w, cs
}

// TestH2SendCapIs64MiB889: an HTTP/2 conn's write hook accepts a frame while
// the bytes queued before it (writeBuf, sendBuf) are at most
// maxSendQueueBytesH2 (64 MiB) and refuses it, recording the refusal, once they
// are over it.
func TestH2SendCapIs64MiB889(t *testing.T) {
	const frame = 4096
	for _, tc := range []struct {
		name        string
		queued      int
		wantRefused bool
	}{
		{"32 MiB queued: accepted", 32 << 20, false},
		{"64 MiB queued: accepted", 64 << 20, false},
		{"64 MiB + 1 queued: refused", 64<<20 + 1, true},
		{"128 MiB queued: refused", 128 << 20, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, cs := newH2Worker889(t)
			_ = w
			if got := cs.sendCap(); got != 64<<20 {
				t.Fatalf("sendCap of an H2 conn = %d, want %d", got, 64<<20)
			}
			cs.writeBuf = make([]byte, tc.queued)
			cs.writeFn(make([]byte, frame))
			if cs.writeRefused != tc.wantRefused {
				t.Fatalf("writeRefused = %v with %d bytes queued, want %v", cs.writeRefused, tc.queued, tc.wantRefused)
			}
			if grew := len(cs.writeBuf) - tc.queued; tc.wantRefused && grew != 0 || !tc.wantRefused && grew != frame {
				t.Errorf("the frame grew writeBuf by %d bytes (refused=%v)", grew, tc.wantRefused)
			}
		})
	}
}

// TestH2RefusedWriteClosesTheConn889: a frame the write hook refuses is lost,
// and the framing of the conn with it, so the pass closes the conn. What was
// staged is sent first: the close is deferred (closing) behind it, not made at
// once.
func TestH2RefusedWriteClosesTheConn889(t *testing.T) {
	w, cs := newH2Worker889(t)
	fd := cs.fd
	cs.writeBuf = make([]byte, 64<<20+1) // a backlog over the cap: the next frame is refused
	cs.h2State.EnqueueWrite(1, make([]byte, 500))
	w.drainH2Writes()
	if !cs.writeRefused {
		t.Fatal("the frame over the cap was not refused")
	}
	if !cs.closing {
		t.Fatalf("the pass did not close the conn that lost a frame (closing=%v, still registered=%v)", cs.closing, w.conns[fd] == cs)
	}
	// The close moves what was staged on to the send side; the refused frame
	// is in neither buffer.
	if got := len(cs.writeBuf) + len(cs.sendBuf); got != 64<<20+1 {
		t.Errorf("the refused frame reached a send buffer: %d bytes queued, want %d", got, 64<<20+1)
	}
}

// h2NopHandler889 answers nothing: the tests queue frames by hand.
type h2NopHandler889 struct{}

func (h2NopHandler889) HandleStream(context.Context, *stream.Stream) error { return nil }
