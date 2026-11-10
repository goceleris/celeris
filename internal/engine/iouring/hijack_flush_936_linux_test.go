//go:build linux

package iouring

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// celeris#936 follow-up (maintainer decision U-15): a reply an earlier
// pipelined request left in the write queue (the sends of one recv go out
// after its last request is handled) must reach the peer before the hijacked
// socket is handed over. hijackConn used to refuse any hijack with bytes in
// writeBuf ("cannot hijack with pending sends"), so a handler could not
// hijack a connection whose client pipelines. It writes the queue to the
// socket itself now, without blocking the worker: what the socket does not
// take at once is left queued and the hijack is refused, as before.
//
// The fixture's socketpair is the conn; the reply is put in writeBuf as
// makeWriteFn does.

func readAvailable936(t *testing.T, fd int, want int) []byte {
	t.Helper()
	var got []byte
	buf := make([]byte, 64<<10)
	deadline := time.Now().Add(5 * time.Second)
	for len(got) < want && time.Now().Before(deadline) {
		n, err := unix.Read(fd, buf)
		if err == unix.EAGAIN {
			time.Sleep(time.Millisecond)
			continue
		}
		if err != nil || n == 0 {
			break
		}
		got = append(got, buf[:n]...)
	}
	return got
}

func TestHijackFlushesTheQueuedReply936(t *testing.T) {
	f := newFDLFixture(t, false)
	w, cs := f.w, f.cs
	const reply = "HTTP/1.1 200 OK\r\nContent-Length: 7\r\n\r\nreply-A"
	cs.writeBuf = append(cs.writeBuf, reply...)

	c, err := w.hijackConn(f.fd)
	if err != nil {
		t.Fatalf("hijackConn with a reply still queued: %v", err)
	}
	defer func() { _ = c.Close() }()
	_ = unix.SetNonblock(f.peer, true)
	if got := readAvailable936(t, f.peer, len(reply)); string(got) != reply {
		t.Fatalf("the peer read %q before the hand-over, want the queued reply %q", got, reply)
	}
	// What the hijacker writes comes after the reply, on the same socket.
	if _, err := io.WriteString(c, "hijacker"); err != nil {
		t.Fatal(err)
	}
	if got := readAvailable936(t, f.peer, len("hijacker")); string(got) != "hijacker" {
		t.Fatalf("the hijacker's write read %q, want %q", got, "hijacker")
	}
}

// A reply the socket cannot take at once is not dropped and does not block the
// worker: the hijack is refused, what the socket took has gone, the rest is
// still queued in order, and the conn is as it was.
func TestHijackWithAReplyTheSocketCannotTakeIsRefused936(t *testing.T) {
	f := newFDLFixture(t, false)
	w, cs := f.w, f.cs
	reply := bytes.Repeat([]byte("0123456789abcdef"), 1<<18) // 4 MiB: more than a socketpair buffers
	cs.writeBuf = append(cs.writeBuf, reply...)
	if !w.prepareRecv(cs, cs.buf) {
		t.Fatal("recv not armed")
	}
	_ = takeSQEs(w.ring)

	c, err := w.hijackConn(f.fd)
	if err == nil {
		_ = c.Close()
		t.Fatal("hijackConn succeeded with most of a 4 MiB reply still queued: it would be dropped")
	}
	t.Logf("refused: %v", err)
	// The socket-full refusal is not the in-flight one: a log or an assertion
	// can tell which branch fired.
	if !strings.Contains(err.Error(), "send buffer") {
		t.Errorf("refusal %q does not say the socket's send buffer is the limit", err)
	}
	if w.conns[f.fd] != cs || w.connCount != 1 || cs.closing {
		t.Errorf("a refused hijack changed the conn: registered=%v connCount=%d closing=%v", w.conns[f.fd] == cs, w.connCount, cs.closing)
	}
	_ = unix.SetNonblock(f.peer, true)
	sent := len(reply) - len(cs.writeBuf)
	got := readAvailable936(t, f.peer, sent)
	if len(got) != sent || !bytes.Equal(got, reply[:sent]) {
		t.Fatalf("the peer read %d bytes, the worker says it sent %d, and what was read is not the reply's prefix", len(got), sent)
	}
	if !bytes.Equal(cs.writeBuf, reply[sent:]) {
		t.Errorf("the unsent remainder of the reply is not the reply's tail: %d bytes queued, want %d", len(cs.writeBuf), len(reply)-sent)
	}
}

// A send already in flight (the kernel holds its buffer) is refused as before:
// writing the queue behind it would reorder the stream.
func TestHijackWithASendInFlightIsRefused936(t *testing.T) {
	f := newFDLFixture(t, false)
	w, cs := f.w, f.cs
	cs.sending = true
	cs.writeBuf = append(cs.writeBuf, "later"...)
	c, err := w.hijackConn(f.fd)
	if err == nil {
		_ = c.Close()
		t.Fatal("hijackConn succeeded with a send in flight")
	}
	if !strings.Contains(err.Error(), "pending sends") {
		t.Errorf("in-flight refusal %q does not say pending sends", err)
	}
	if string(cs.writeBuf) != "later" {
		t.Errorf("a refused hijack changed the queue: %q", cs.writeBuf)
	}
}

// A detached conn belongs to the middleware that detached it, whose guarded
// writer is live and which the dispatch goroutine no longer serializes with
// the worker: the engine refuses to hijack it, whatever is queued, and leaves
// it as it was. Context.Hijack refuses first; this is the engine enforcing it
// for any other caller of the Hijacker.
func TestHijackOfADetachedConnIsRefused936(t *testing.T) {
	for _, queued := range []bool{false, true} {
		name := "nothing-queued"
		if queued {
			name = "reply-queued"
		}
		t.Run(name, func(t *testing.T) {
			f := newFDLFixture(t, false)
			w, cs := f.w, f.cs
			cs.h1State.Detached.Store(true)
			if queued {
				cs.writeBuf = append(cs.writeBuf, "reply"...)
			}
			c, err := w.hijackConn(f.fd)
			if err == nil {
				_ = c.Close()
				t.Fatal("hijackConn handed over a detached conn")
			}
			t.Logf("refused: %v", err)
			if w.conns[f.fd] != cs || w.connCount != 1 || !fdOpen704(f.fd) {
				t.Errorf("a refused hijack changed the conn: registered=%v connCount=%d open=%v", w.conns[f.fd] == cs, w.connCount, fdOpen704(f.fd))
			}
			if queued && string(cs.writeBuf) != "reply" {
				t.Errorf("a refused hijack changed the queue: %q", cs.writeBuf)
			}
		})
	}
}
