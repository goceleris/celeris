//go:build !unix

package memcached

import "syscall"

// The raw-descriptor operations on a platform without unix descriptors
// (windows). The conn's transport is a net.Conn the event loop owns, so there
// is nothing to peek at, close or shut down by number.

// peekFD reports no descriptor: the direct-mode conn skips the non-blocking
// peek and reads through the net.Conn.
func peekFD(uintptr) int { return 0 }

// recvNoWait is never called (peekFD leaves the cached descriptor at 0); it
// reports that no data is ready.
func recvNoWait(int, []byte) (int, syscall.Sockaddr, error) {
	return 0, nil, syscall.EAGAIN
}

// closeRawFD does nothing: the loop closes the net.Conn on UnregisterConn.
func closeRawFD(int) {}
