//go:build linux

package epoll

import (
	"golang.org/x/sys/unix"
)

// flushWrites attempts a single non-blocking write of pending data.
// Uses write position tracking to avoid O(n) memmove on partial writes.
// Compaction is amortized: only performed when more than half the buffer
// capacity is consumed, reducing the average cost to O(1).
func flushWrites(cs *connState) error {
	if cs.writePos >= len(cs.writeBuf) {
		cs.writeBuf = cs.writeBuf[:0]
		cs.writePos = 0
		return nil
	}
	n, err := unix.Write(cs.fd, cs.writeBuf[cs.writePos:])
	if err != nil {
		if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
			return nil // socket buffer full, will retry
		}
		cs.writeBuf = cs.writeBuf[:0]
		cs.writePos = 0
		return err
	}
	cs.writePos += n
	if cs.writePos >= len(cs.writeBuf) {
		// Fully flushed
		cs.writeBuf = cs.writeBuf[:0]
		cs.writePos = 0
	} else if cs.writePos > cap(cs.writeBuf)/2 {
		// Amortized compaction: only copy when more than half the buffer is consumed.
		// This reduces the average cost from O(n) per partial write to O(1) amortized.
		remaining := len(cs.writeBuf) - cs.writePos
		copy(cs.writeBuf, cs.writeBuf[cs.writePos:])
		cs.writeBuf = cs.writeBuf[:remaining]
		cs.writePos = 0
	}
	return nil
}
