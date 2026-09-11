//go:build linux

package sockopts

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestDrainRecvBufferNonBlocking is the regression guard for celeris#311.
//
// DrainRecvBuffer runs on the io_uring event-loop worker thread during the
// graceful detached-close path (SHUT_WR → drain → close). Those sockets are in
// blocking mode, so the original unix.Read implementation blocked forever when
// a connection was closed before the peer's FIN arrived (the churn-close race),
// wedging the worker — and with enough concurrent detached closes the whole
// engine stopped servicing requests.
//
// This test puts a blocking-mode socket in exactly that state (empty receive
// buffer, peer still open so no FIN) and asserts the drain returns promptly.
// With a blocking Read it hangs and trips the timeout; with MSG_DONTWAIT it
// returns immediately.
func TestDrainRecvBufferNonBlocking(t *testing.T) {
	fds := socketpair(t)

	done := make(chan struct{})
	go func() {
		DrainRecvBuffer(fds[0])
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("DrainRecvBuffer blocked on an empty blocking socket (celeris#311 regression)")
	}
}

// TestDrainRecvBufferDrainsThenReturns verifies the drain still does its job:
// it consumes data already queued in the receive buffer (so close() does not
// RST away a staged GOAWAY / close frame) and then returns once the buffer is
// empty — without blocking on the still-open peer.
func TestDrainRecvBufferDrainsThenReturns(t *testing.T) {
	fds := socketpair(t)

	if _, err := unix.Write(fds[1], []byte("queued bytes the drain must discard")); err != nil {
		t.Fatalf("write: %v", err)
	}

	done := make(chan struct{})
	go func() {
		DrainRecvBuffer(fds[0])
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("DrainRecvBuffer blocked after draining queued data (celeris#311 regression)")
	}

	var buf [64]byte
	n, _, rerr := unix.Recvfrom(fds[0], buf[:], unix.MSG_DONTWAIT)
	if n > 0 {
		t.Fatalf("DrainRecvBuffer left %d bytes undrained", n)
	}
	if rerr != unix.EAGAIN && rerr != unix.EWOULDBLOCK {
		t.Fatalf("expected EAGAIN after drain, got n=%d err=%v", n, rerr)
	}
}

// TestDrainRecvBufferStopsAtTheBound is the regression guard for celeris#571:
// the epoll engine's copy of this drain had no bound, so a peer delivering
// bytes as fast as the loop consumed them held the event-loop thread and
// starved every other connection on it.
//
// It queues well past the 32 KiB cap and asserts the drain stops with data
// still unread. An unbounded drain empties the socket and fails here.
func TestDrainRecvBufferStopsAtTheBound(t *testing.T) {
	fds := socketpair(t)

	capBytes := drainRecvBufSize * drainRecvMaxReads
	// Queue 4x the cap, writing non-blockingly so a small socket buffer
	// cannot deadlock the test. Whatever lands is what the drain sees.
	if err := unix.SetNonblock(fds[1], true); err != nil {
		t.Fatalf("setnonblock: %v", err)
	}
	chunk := make([]byte, 4096)
	queued := 0
	for queued < 4*capBytes {
		n, err := unix.Write(fds[1], chunk)
		if n > 0 {
			queued += n
		}
		if err != nil {
			break
		}
	}
	if queued <= capBytes {
		t.Skipf("socket buffer held only %d bytes, not more than the %d-byte cap", queued, capBytes)
	}

	DrainRecvBuffer(fds[0])

	var buf [64]byte
	n, _, rerr := unix.Recvfrom(fds[0], buf[:], unix.MSG_DONTWAIT)
	if n <= 0 {
		t.Fatalf("drain consumed the whole %d-byte backlog: it is unbounded (celeris#571); recv n=%d err=%v", queued, n, rerr)
	}
}

func socketpair(t *testing.T) [2]int {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	t.Cleanup(func() {
		_ = unix.Close(fds[0])
		_ = unix.Close(fds[1])
	})
	return [2]int{fds[0], fds[1]}
}
