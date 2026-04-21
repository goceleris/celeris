//go:build linux

package epoll

import (
	"golang.org/x/sys/unix"
)

// flushWrites attempts a single non-blocking write of pending data.
// Uses write position tracking to avoid O(n) memmove on partial writes.
// Compaction is amortized: only performed when more than half the buffer
// capacity is consumed, reducing the average cost to O(1).
//
// Scatter-gather path: when cs.bodyBuf is set (zero-copy body staged by
// the response adapter's writeBody fast-path), the first write uses
// writev(2) with iovec = [writeBuf[writePos:], bodyBuf]. Saves one
// full body-sized memcpy per request compared to appending the body
// into writeBuf first.
func flushWrites(cs *connState) error {
	if len(cs.bodyBuf) > 0 {
		return flushWritesV(cs)
	}
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

// flushWritesV handles the writev path: headers in cs.writeBuf[writePos:]
// plus the body slice in cs.bodyBuf. Invoked by flushWrites when bodyBuf
// is non-nil; clears bodyBuf once fully sent. Partial writev collapses
// the remainder into writeBuf so the next flushWrites call uses the
// plain write path.
func flushWritesV(cs *connState) error {
	headerRem := cs.writeBuf[cs.writePos:]
	iovs := [2][]byte{headerRem, cs.bodyBuf}
	n, err := unix.Writev(cs.fd, iovs[:])
	if err != nil {
		if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
			return nil
		}
		cs.writeBuf = cs.writeBuf[:0]
		cs.writePos = 0
		cs.bodyBuf = nil
		return err
	}
	total := len(headerRem) + len(cs.bodyBuf)
	switch {
	case n >= total:
		// Everything out the door.
		cs.writeBuf = cs.writeBuf[:0]
		cs.writePos = 0
		cs.bodyBuf = nil
	case n >= len(headerRem):
		// Headers fully sent; body partially sent. Collapse remaining
		// body bytes into writeBuf so the next flushWrites call uses the
		// plain write path. A body-sized memcpy only on partial writes.
		rem := cs.bodyBuf[n-len(headerRem):]
		cs.writeBuf = cs.writeBuf[:0]
		cs.writeBuf = append(cs.writeBuf, rem...)
		cs.writePos = 0
		cs.bodyBuf = nil
	default:
		// Partial headers. Keep bodyBuf pending and advance writePos;
		// next flushWrites call repeats the writev with advanced offset.
		cs.writePos += n
	}
	return nil
}
