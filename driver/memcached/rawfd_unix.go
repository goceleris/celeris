//go:build unix

package memcached

import "syscall"

// The raw-descriptor operations of the unix transport. conn.go calls them
// where it used to call syscall directly; each one is a single call, so the
// compiler inlines it into the caller.

// sockFD is the type of the descriptor the direct-mode conn caches for its
// non-blocking peek. The read loops call syscall.Recvfrom on it themselves,
// with msgDontWait, rather than through a wrapper: an inlined wrapper keeps
// its results in stack slots of its own and changes the loop's code.
type sockFD = int

// msgDontWait is the flag of the read loops' non-blocking peek.
const msgDontWait = syscall.MSG_DONTWAIT

// peekFD returns the descriptor the direct-mode conn caches for its
// non-blocking peek, or 0 when none is usable.
func peekFD(fd uintptr) sockFD { return sockFD(fd) }

// closeRawFD closes a descriptor the conn owns outside an *os.File. A conn
// dialed through dial*ConnNetConn (compiled on unix for its tests) holds a
// synthetic loop identifier (>= 1<<30), not a descriptor, in the same field;
// closing it fails with EBADF, which is ignored: a process would need over a
// billion open descriptors (a descriptor table of 8 GiB or more) to own that
// number, so the call cannot reach a descriptor of the process. The
// IsNetConnID guards were dropped on purpose: they changed the hot-path code
// and bought nothing.
func closeRawFD(fd int) { _ = syscall.Close(fd) }
