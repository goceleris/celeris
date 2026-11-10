//go:build linux

package postgres

import (
	"time"

	"golang.org/x/sys/unix"
)

// setsockoptInt is unix.SetsockoptInt behind a seam, so a test can record
// which options tuneConnSocket sets (celeris#1228).
var setsockoptInt = unix.SetsockoptInt

// tuneConnSocket sets Linux-specific socket options on a driver connection:
//
//   - TCP_QUICKACK disables delayed ACK so the server's response reaches our
//     read(2) faster (the kernel ACKs our write immediately instead of waiting
//     up to 40ms for the delayed ACK timer).
//   - SO_BUSY_POLL, only when busyPoll is at least one microsecond, makes a read
//     that finds the socket empty first run one pass of the device's receive
//     polling on the calling CPU. It is opt-in, as Config.SocketBusyPoll is for
//     the accepted sockets: with busyPoll 0, the default, the socket keeps
//     what the kernel gave it and no setsockopt is made. Before v1.6.0 it was
//     set to 50 microseconds on every connection (celeris#1228).
func tuneConnSocket(fd int, busyPoll time.Duration) {
	_ = setsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_QUICKACK, 1)
	if micros := int(busyPoll.Microseconds()); micros > 0 {
		_ = setsockoptInt(fd, unix.SOL_SOCKET, unix.SO_BUSY_POLL, micros)
	}
}
