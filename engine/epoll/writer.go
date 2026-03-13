//go:build linux

package epoll

import (
	"golang.org/x/sys/unix"
)

// flushWrites attempts a single non-blocking write of pending data.
// Returns the number of bytes written and any error. Does not block.
func flushWrites(cs *connState) error {
	if len(cs.writeBuf) == 0 {
		return nil
	}
	n, err := unix.Write(cs.fd, cs.writeBuf)
	if err != nil {
		if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
			return nil // socket buffer full, will retry
		}
		cs.writeBuf = cs.writeBuf[:0]
		return err
	}
	if n >= len(cs.writeBuf) {
		cs.writeBuf = cs.writeBuf[:0]
	} else {
		remaining := len(cs.writeBuf) - n
		copy(cs.writeBuf, cs.writeBuf[n:])
		cs.writeBuf = cs.writeBuf[:remaining]
	}
	return nil
}
