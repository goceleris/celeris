//go:build !unix

package postgres

import "syscall"

// The raw-descriptor operations on a platform without unix descriptors
// (windows). The conn's transport is a net.Conn the event loop owns, so there
// is nothing to peek at, close or shut down by number.

// sockFD is the type syscall.Recvfrom takes here. driveDirect's peek is
// compiled but never runs: peekFD leaves the cached descriptor at 0.
type sockFD = syscall.Handle

// msgDontWait is unused at run time (see sockFD); windows has no
// MSG_DONTWAIT.
const msgDontWait = 0

// peekFD reports no descriptor: the direct-mode conn skips the non-blocking
// peek and reads through the net.Conn.
func peekFD(uintptr) sockFD { return 0 }

// closeRawFD does nothing: the loop closes the net.Conn on UnregisterConn.
func closeRawFD(int) {}

// shutdownRawFD does nothing: the loop closes the net.Conn right after
// onClose returns.
func shutdownRawFD(int) {}
