//go:build linux

package sockopts

import (
	"golang.org/x/sys/unix"
)

func applyFD(fd int, opts Options) error {
	if opts.TCPNoDelay {
		if err := unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1); err != nil {
			return err
		}
	}
	if opts.TCPQuickAck {
		if err := unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_QUICKACK, 1); err != nil {
			return err
		}
	}
	if opts.SOBusyPoll > 0 {
		micros := int(opts.SOBusyPoll.Microseconds())
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_BUSY_POLL, micros); err != nil {
			return err
		}
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, 69, 1) // SO_PREFER_BUSY_POLL
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, 70, 8) // SO_BUSY_POLL_BUDGET
	}
	if opts.RecvBuf > 0 {
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, opts.RecvBuf); err != nil {
			return err
		}
	}
	if opts.SendBuf > 0 {
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, opts.SendBuf); err != nil {
			return err
		}
	}
	return nil
}

// drainRecvBufSize and drainRecvMaxReads bound [DrainRecvBuffer]. Their
// product, 32 KiB, is far more than any legitimate post-shutdown tail (a
// queued GOAWAY, a WebSocket close echo) and is the cap the io_uring worker
// has carried since celeris#311. The bound is what stops a peer that keeps
// delivering bytes as fast as the drain consumes them from holding the
// event-loop thread, and with it every other connection on that loop.
const (
	drainRecvBufSize  = 4096
	drainRecvMaxReads = 8
)

// DrainRecvBuffer reads and discards up to 32 KiB of whatever is already in
// fd's socket receive buffer, so the close(2) that follows sends FIN rather
// than RST. An RST would additionally destroy data still staged in the SEND
// buffer — a queued GOAWAY or WebSocket close frame the peer has not read yet.
// (close(2) frees the receive queue either way; draining is about the reset,
// not about the inbound bytes. See celeris#569.)
//
// It MUST NOT block. The io_uring worker calls it on the event-loop thread
// with the socket in BLOCKING mode, where a plain read waits for data or a FIN
// that a half-closed peer may never send — wedging the worker, and with enough
// concurrent detached closes the whole engine (celeris#311). MSG_DONTWAIT
// makes every read return EAGAIN the moment the queue is empty, on either
// engine's sockets.
//
// Both loop engines share this one implementation so the bound cannot drift
// back apart (celeris#571).
func DrainRecvBuffer(fd int) {
	var buf [drainRecvBufSize]byte
	for range drainRecvMaxReads {
		n, _, err := unix.Recvfrom(fd, buf[:], unix.MSG_DONTWAIT)
		if n <= 0 || err != nil {
			return
		}
	}
}
