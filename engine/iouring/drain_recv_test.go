//go:build linux

package iouring

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestDrainRecvBufferNonBlocking is the regression guard for celeris#311.
//
// drainRecvBuffer runs on the io_uring event-loop worker thread during the
// graceful detached-close path (SHUT_WR → drain → close). The sockets are in
// blocking mode, so the prior unix.Read implementation would block forever when
// a connection was closed before the peer's FIN arrived (the churn-close race),
// wedging the worker — and with enough concurrent detached-closes the whole
// engine stopped servicing requests.
//
// This test puts a blocking-mode socket in exactly that state (empty receive
// buffer, peer still open so no FIN) and asserts drainRecvBuffer returns
// promptly instead of blocking. With the old blocking Read it hangs and trips
// the timeout; with the MSG_DONTWAIT drain it returns immediately.
func TestDrainRecvBufferNonBlocking(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	defer func() { _ = unix.Close(fds[0]) }()
	defer func() { _ = unix.Close(fds[1]) }()

	// fds[0] is blocking (socketpair default) with an empty recv buffer and the
	// peer (fds[1]) still open — the exact state that hung the old Read.
	done := make(chan struct{})
	go func() {
		drainRecvBuffer(fds[0])
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("drainRecvBuffer blocked on an empty blocking socket (celeris#311 regression)")
	}
}

// TestDrainRecvBufferDrainsThenReturns verifies the drain still does its job:
// it consumes data already queued in the receive buffer (so close() does not
// RST away a staged GOAWAY / close frame) and then returns once the buffer is
// empty — without blocking on the still-open peer.
func TestDrainRecvBufferDrainsThenReturns(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	defer func() { _ = unix.Close(fds[0]) }()
	defer func() { _ = unix.Close(fds[1]) }()

	if _, err := unix.Write(fds[1], []byte("queued bytes the drain must discard")); err != nil {
		t.Fatalf("write: %v", err)
	}

	done := make(chan struct{})
	go func() {
		drainRecvBuffer(fds[0])
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("drainRecvBuffer blocked after draining queued data (celeris#311 regression)")
	}

	// Buffer must now be empty: a non-blocking read returns EAGAIN/EWOULDBLOCK.
	var buf [64]byte
	n, _, rerr := unix.Recvfrom(fds[0], buf[:], unix.MSG_DONTWAIT)
	if n > 0 {
		t.Fatalf("drainRecvBuffer left %d bytes undrained", n)
	}
	if rerr != unix.EAGAIN && rerr != unix.EWOULDBLOCK {
		t.Fatalf("expected EAGAIN after drain, got n=%d err=%v", n, rerr)
	}
}
