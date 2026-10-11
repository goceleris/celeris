//go:build unix

package memcached

import "syscall"

// The raw-descriptor operations of the unix transport. conn.go calls them
// where it used to call syscall directly; each one is a single call, so the
// compiler inlines it into the caller (the hot-path peeks keep their code).

// peekFD returns the descriptor the direct-mode conn caches for
// recvNoWait, or 0 when none is usable.
func peekFD(fd uintptr) int { return int(fd) }

// recvNoWait reads whatever the kernel already holds for fd without blocking.
func recvNoWait(fd int, p []byte) (int, syscall.Sockaddr, error) {
	return syscall.Recvfrom(fd, p, syscall.MSG_DONTWAIT)
}

// closeRawFD closes a descriptor the conn owns outside an *os.File.
func closeRawFD(fd int) { _ = syscall.Close(fd) }
