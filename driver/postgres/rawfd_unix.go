//go:build unix

package postgres

import (
	"syscall"

	"github.com/goceleris/celeris/driver/internal/eventloop"
)

// The raw-descriptor operations of the unix transport. conn.go calls them
// where it used to call syscall directly; each one is a single call, so the
// compiler inlines it into the caller (the hot-path peek keeps its code).

// peekFD returns the descriptor the direct-mode conn caches for
// recvNoWait, or 0 when none is usable.
func peekFD(fd uintptr) int { return int(fd) }

// recvNoWait reads whatever the kernel already holds for fd without blocking.
func recvNoWait(fd int, p []byte) (int, syscall.Sockaddr, error) {
	return syscall.Recvfrom(fd, p, syscall.MSG_DONTWAIT)
}

// closeRawFD closes a descriptor the conn owns outside an *os.File. A conn
// the loop adopted as a net.Conn has none (the loop closes it).
func closeRawFD(fd int) {
	if eventloop.IsNetConnID(fd) {
		return
	}
	_ = syscall.Close(fd)
}

// shutdownRawFD shuts fd's socket down in both directions, keeping its
// number. A conn the loop adopted as a net.Conn has no descriptor to shut
// down: the loop closes the net.Conn right after onClose returns.
func shutdownRawFD(fd int) {
	if eventloop.IsNetConnID(fd) {
		return
	}
	_ = syscall.Shutdown(fd, syscall.SHUT_RDWR)
}
