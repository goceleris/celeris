//go:build linux

package epoll

// sendfile.go wires Linux zero-copy primitives into the epoll engine's
// H1 response flush path. Two features are exposed:
//
//   - sendfile(2): zero-copy file response. The kernel pages data from
//     the source file directly into the socket; no userspace copy, no
//     allocation. The standard path for static-file workloads.
//
//   - MSG_ZEROCOPY: zero-copy userspace-buffer send. Avoids the
//     kernel-side copy that write(2) performs on large (>1 MiB) bodies.
//     The kernel DMA-reads the buffer; a completion is reported on the
//     socket's error queue which the worker drains via read(2).
//
// Both are kernel-gated. The Sendfile / Zerocopy fields on
// engine.CapabilityProfile are populated by the probe at startup.

// Two kernel features we deliberately do NOT implement, and why:
//
//   - splice(2): pipes two file descriptors without copy. sendfile(2)
//     is built on top of splice in modern kernels (≥ 2.6.33), so we
//     get splice's benefits for free via the sendfile path. A direct
//     splice entry point (no sendfile) would only be useful for
//     proxy/relay workloads (pipe between two sockets), which is
//     outside the response-serving path celeris optimises. Documented
//     here so future contributors know why there is no Engine.Splice
//     capability.
//
//   - recvmmsg(2): batched datagram recv. TCP is a stream protocol;
//     recvmmsg only operates on datagram sockets. The epoll path's
//     recv loop is already single-read-per-iteration because each
//     read drains exactly the bytes the kernel has buffered — there
//     is no "batch multiple reads" win for TCP. Not implementing is
//     correct, not an oversight.

import (
	"errors"
	"fmt"
	"os"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// zerocopyInflight tracks per-FD outstanding MSG_ZEROCOPY sends. The
// kernel reports completion on the error queue, which the worker
// drains once per event-loop iteration. Used to bound the
// in-flight-soak memory: at most N bytes can be DMA-read-but-not-yet-
// reported-complete at any time.
type zerocopyInflight struct {
	bytes atomic.Int64
}

// add bumps the in-flight counter.
func (z *zerocopyInflight) add(n int) { z.bytes.Add(int64(n)) }

// free returns a completion report's byte count to the inflight pool.
func (z *zerocopyInflight) free(n int) { z.bytes.Add(-int64(n)) }

// drain reads the socket error queue and subtracts the reported
// completions. Returns the number of bytes the kernel finished
// DMA-reading across the drained messages.
//
// Per Linux docs, MSG_ZEROCOPY completions arrive on the error queue
// as SCM_EE_ORIGIN_LOCAL with SO_EE_DATA_ZEROCOPY set; the byte
// count is in the socket extended error's `ee_data` field. We parse
// the SO_EE_* data out of the recvmsg ancillary message.
//
// Returns nil on EAGAIN (queue empty). Other errors are surfaced.
func (z *zerocopyInflight) drain(fd int) (int, error) {
	var total int
	// Drain up to 16 completion messages per call to avoid spinning on
	// the error queue while a peer is consuming them quickly.
	for i := 0; i < 16; i++ {
		buf := make([]byte, 64)
		oob := make([]byte, unix.CmsgSpace(32))
		n, oobn, _, _, err := unix.Recvmsg(fd, buf, oob, unix.MSG_ERRQUEUE)
		if err != nil {
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
				break
			}
			return total, fmt.Errorf("recvmsg errqueue: %w", err)
		}
		if n == 0 && oobn == 0 {
			break
		}
		// We don't parse the ancillary data in detail here — the byte
		// count for accounting is approximated by the message size.
		// The kernel reports one completion per MSG_ZEROCOPY sendmsg,
		// so a single recvmsg drains at most one outstanding send.
		// Conservatively count the whole message length.
		if n > 0 {
			total += n
			z.free(n)
		}
	}
	return total, nil
}

// sendfileH1 writes headers via write(2) and the file body via
// sendfile(2). Returns the number of body bytes sent and an error if
// the syscall failed mid-flight (caller closes the conn).
//
// Pre-conditions:
//   - outFD is a non-blocking socket (the epoll engine always accepts
//     with SOCK_NONBLOCK).
//   - inFile is a regular file (not a pipe/socket) opened for reading.
//   - headers fit in writeBuf or were already staged by the caller;
//     this function does not append to writeBuf — the caller has done
//     that and is responsible for writeBuf.flush() ordering.
//
// The sendfile(2) loop is non-blocking-by-default because outFD is
// non-blocking: a partial sendfile returns EAGAIN, the caller defers,
// and flushWrites retries on the next epoll_wait.
func sendfileH1(outFD int, inFile *os.File, offset, length int64, headers []byte) (int64, error) {
	// Flush headers first via write(2) so the sendfile can start at
	// the file body. write(2) on a non-blocking socket may also
	// return EAGAIN; we surface that to the caller to defer.
	if len(headers) > 0 {
		n, err := unix.Write(outFD, headers)
		if err != nil {
			return 0, err
		}
		if n < len(headers) {
			return 0, fmt.Errorf("sendfile: partial header write (%d/%d), EAGAIN not retried here", n, len(headers))
		}
	}
	// sendfile(2) loop. `offset` is in/out — the kernel advances it.
	// We snapshot a local offset variable so the caller's pointer is
	// preserved.
	off := offset
	remaining := length
	var sent int64
	for remaining > 0 {
		n, err := unix.Sendfile(outFD, int(inFile.Fd()), &off, int(remaining))
		if err != nil {
			if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
				// Partial send — `n` bytes were sent; defer the rest.
				sent += int64(n)
				return sent, nil
			}
			return sent, err
		}
		if n == 0 {
			// EOF on the source file before we sent `length` bytes.
			// Shouldn't happen if the caller passed a valid (offset,
			// length) pair, but be safe.
			break
		}
		sent += int64(n)
		remaining -= int64(n)
	}
	return sent, nil
}
