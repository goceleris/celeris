//go:build unix

package postgres

import "syscall"

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
// dialed through dial*ConnNetConn (compiled on unix for its tests) holds a
// synthetic loop identifier (>= 1<<30), not a descriptor, in the same field;
// closing it fails with EBADF, which is ignored. The IsNetConnID guards were
// dropped on purpose: they changed the hot-path code and bought nothing.
func closeRawFD(fd int) { _ = syscall.Close(fd) }

// shutdownRawFD shuts fd's socket down in both directions, keeping its
// number. The same applies to a synthetic identifier: EBADF, ignored.
func shutdownRawFD(fd int) { _ = syscall.Shutdown(fd, syscall.SHUT_RDWR) }
