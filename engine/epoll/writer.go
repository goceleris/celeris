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
		// writeBuf is empty — if a zero-copy sendfile response is pending,
		// drive it now (after any prior pipelined writeBuf bytes flushed).
		return flushSendfile(cs)
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
		// writeBuf drained this call — continue into the sendfile body if a
		// sendfile response is pending. Keeps header→body ordering correct.
		return flushSendfile(cs)
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

// flushSendfile drives a pending zero-copy sendfile(2) response as far as
// the kernel send buffer allows. Called by flushWrites only once
// writeBuf/bodyBuf are fully drained, so the sendfile header+body always
// follow any prior pipelined bytes in order.
//
// On EAGAIN the kernel send buffer is full: returns nil (not an error)
// and leaves cs.sendfile in place so the EPOLLOUT/dirty resume machinery
// retries on the next writable edge — identical backpressure handling to
// the writeBuf path above. On a real I/O error the file is released and
// the error is surfaced (the caller closes the conn). On completion the
// dup'd file is closed and cs.sendfile cleared.
func flushSendfile(cs *connState) error {
	st := cs.sendfile
	if st == nil {
		return nil
	}
	_, err := st.advance(cs.fd)
	if err != nil {
		if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
			return nil // send buffer full; resume on next writable edge
		}
		// Real error (or the header write surfaced one): release the file
		// and surface to the caller, which closes the connection.
		st.close()
		cs.sendfile = nil
		return err
	}
	if st.done {
		st.close()
		cs.sendfile = nil
	}
	return nil
}

// csWritePending reports whether cs has any unsent output: buffered bytes
// in writeBuf, a staged scatter-gather bodyBuf, or an in-progress
// sendfile response. The flush paths use it to decide whether the
// connection is fully drained (disarm EPOLLOUT, clear dirty) or still
// needs a writable-edge resume.
func csWritePending(cs *connState) bool {
	return cs.writePos < len(cs.writeBuf) || len(cs.bodyBuf) > 0 || cs.sendfile != nil
}

// csPendingBytes returns the number of bytes still queued for this
// connection across all output sources: the unflushed writeBuf tail, the
// staged scatter-gather bodyBuf, and any header+body remainder of an
// in-progress sendfile response. Used to keep cs.pendingBytes (the
// back-pressure accounting that gates writeCap) accurate while a sendfile
// is draining so a stalled file transfer is not masked.
func csPendingBytes(cs *connState) int {
	n := len(cs.writeBuf) - cs.writePos + len(cs.bodyBuf)
	if st := cs.sendfile; st != nil {
		n += len(st.headers) - st.headerOff
		if st.remaining > 0 {
			n += int(st.remaining)
		}
	}
	return n
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
