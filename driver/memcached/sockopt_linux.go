//go:build linux

package memcached

import "golang.org/x/sys/unix"

// tuneConnSocket sets Linux-specific socket options that reduce round-trip
// latency on driver connections:
//
//   - TCP_QUICKACK disables delayed ACK so the server's response reaches our
//     read(2) faster (the kernel ACKs our write immediately instead of waiting
//     up to 40ms for the delayed ACK timer).
//   - SO_BUSY_POLL tells the kernel to busy-poll the NIC for up to 50us
//     before parking the socket, reducing last-mile receive latency.
func tuneConnSocket(fd int) {
	_ = unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_QUICKACK, 1)
	_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_BUSY_POLL, 50)
}
