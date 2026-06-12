//go:build linux

package epoll

// sendfile.go wires the Linux sendfile(2) zero-copy primitive into the
// epoll engine's H1 static-file response path. The kernel pages data
// from the source file directly into the socket: no userspace copy, no
// per-response body allocation. This is the standard path for
// static-file workloads (middleware/static, Context.File).
//
// The Sendfile field on engine.CapabilityProfile is populated by the
// probe at startup (Linux-only). The response writer type-asserts the
// engine for engine.SendfileCapable and falls back to the buffered
// read+write path on engines that do not implement it (iouring, std).
//
// MSG_ZEROCOPY (userspace-buffer zero-copy send) is deliberately NOT
// implemented here. A correct MSG_ZEROCOPY path needs SO_ZEROCOPY
// setsockopt, sendmsg(...MSG_ZEROCOPY), errqueue completion draining
// integrated into the event loop, and buffer pinning until the kernel
// reports completion on the socket error queue. That is a large feature
// whose accounting must be exact (a buffer reused before the kernel
// finishes DMA-reading it corrupts the response). sendfile(2) already
// delivers zero-copy for the file-serving workload that matters, so the
// engine.CapabilityProfile.Zerocopy flag is left false (see probe.go)
// rather than advertising a capability the tree does not deliver.
//
// Two kernel features we deliberately do NOT implement, and why:
//
//   - splice(2): pipes two file descriptors without copy. sendfile(2)
//     is built on top of splice in modern kernels (≥ 2.6.33), so we
//     get splice's benefits for free via the sendfile path. A direct
//     splice entry point (no sendfile) would only be useful for
//     proxy/relay workloads (pipe between two sockets), which is
//     outside the response-serving path celeris optimises.
//
//   - recvmmsg(2): batched datagram recv. TCP is a stream protocol;
//     recvmmsg only operates on datagram sockets. Not implementing is
//     correct, not an oversight.

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// sendfileState carries the resumable progress of a single sendfile
// response across EAGAIN-driven event-loop iterations. The response
// writer stores one of these on the connection when a sendfile cannot
// complete in one shot (kernel send buffer full) and resumes from it on
// the next EPOLLOUT edge.
//
// headerOff is the count of header bytes already written; off is the
// current file offset the kernel has advanced to; remaining is the body
// bytes still to send. done is true once both headers and body are fully
// out the door.
type sendfileState struct {
	file      *os.File
	headers   []byte
	headerOff int
	off       int64
	remaining int64
	done      bool
}

// resolveLength normalises the (offset, length) pair against the file.
// Per the engine.SendfileCapable contract, length <= 0 means "send to
// EOF": stat the file and compute length = size - offset. A negative
// result (offset past EOF) clamps to 0.
func resolveLength(file *os.File, offset, length int64) (int64, error) {
	if length > 0 {
		return length, nil
	}
	fi, err := file.Stat()
	if err != nil {
		return 0, err
	}
	length = fi.Size() - offset
	if length < 0 {
		length = 0
	}
	return length, nil
}

// newSendfileState builds the resumable state for a (file, offset,
// length, headers) tuple, resolving length<=0 to "send to EOF".
func newSendfileState(file *os.File, offset, length int64, headers []byte) (*sendfileState, error) {
	length, err := resolveLength(file, offset, length)
	if err != nil {
		return nil, err
	}
	return &sendfileState{
		file:      file,
		headers:   headers,
		off:       offset,
		remaining: length,
	}, nil
}

// close releases the file the sendfile state owns. The engine dups the
// caller's descriptor when it starts an async sendfile so the file's
// lifetime is independent of the handler's defer Close; this closes that
// dup once the transfer completes or the connection closes. Idempotent.
func (st *sendfileState) close() {
	if st == nil || st.file == nil {
		return
	}
	_ = st.file.Close()
	st.file = nil
}

// advance drives the sendfile as far as the kernel send buffer allows.
// It first flushes any unwritten header bytes via write(2), then runs
// the sendfile(2) loop for the body. It returns the number of BODY bytes
// sent on this call and an error.
//
// Backpressure contract (matches engine.SendfileCapable): a short
// write(2) / sendfile(2) under send-buffer pressure surfaces
// EAGAIN/EWOULDBLOCK so the caller defers and resumes later from the
// saved state. EINTR is retried in-loop. On EAGAIN the body byte count
// reflects only what was accepted this call; st.headerOff / st.off /
// st.remaining are advanced so a resume picks up exactly where this call
// stopped. st.done is set once headers and body are both fully flushed.
//
// Pre-conditions:
//   - outFD is a non-blocking socket (the epoll engine accepts with
//     SOCK_NONBLOCK).
//   - st.file is a regular file opened for reading.
func (st *sendfileState) advance(outFD int) (int64, error) {
	// 1. Flush remaining header bytes. A short write(2) on a non-blocking
	//    socket is normal under send-buffer pressure, not fatal: track
	//    progress in st.headerOff and surface EAGAIN so the caller resumes.
	for st.headerOff < len(st.headers) {
		n, err := unix.Write(outFD, st.headers[st.headerOff:])
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			// EAGAIN or a real error: nothing further can go out now.
			// Body bytes sent so far this call is 0 (we never reached the
			// sendfile loop). Surface the error for the caller to defer.
			return 0, err
		}
		st.headerOff += n
	}

	// 2. sendfile(2) loop for the body. The kernel advances st.off as it
	//    transfers; we snapshot it so a resume continues from the right
	//    file position.
	var sent int64
	for st.remaining > 0 {
		n, err := unix.Sendfile(outFD, int(st.file.Fd()), &st.off, int(st.remaining))
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
				// On EAGAIN, unix.Sendfile returns n == -1; do NOT add it to
				// the running total. The kernel already advanced st.off by
				// whatever it accepted before blocking. Surface EAGAIN so the
				// caller defers + resumes (per the SendfileCapable contract).
				return sent, err
			}
			return sent, err
		}
		if n == 0 {
			// EOF on the source before `remaining` bytes were sent. Treat
			// the body as complete to avoid an infinite loop; the caller's
			// content-length may have been stale, but we never re-send.
			st.remaining = 0
			break
		}
		sent += int64(n)
		st.remaining -= int64(n)
	}

	if st.headerOff >= len(st.headers) && st.remaining <= 0 {
		st.done = true
	}
	return sent, nil
}

// sendfileH1 writes headers via write(2) and the file body via
// sendfile(2) in one shot where the send buffer allows. It is the
// one-call convenience wrapper around sendfileState used by the
// SendfileCapable engine method and by tests.
//
// Returns the number of body bytes sent and an error. On EAGAIN it
// returns (bytesSentSoFar, EAGAIN) WITHOUT corrupting the count — the
// caller that wants resumability should use sendfileState directly and
// keep the state across EPOLLOUT edges; this wrapper makes a single
// best-effort attempt and surfaces backpressure to the caller.
//
// length <= 0 means "send to EOF" (see resolveLength).
func sendfileH1(outFD int, inFile *os.File, offset, length int64, headers []byte) (int64, error) {
	st, err := newSendfileState(inFile, offset, length, headers)
	if err != nil {
		return 0, err
	}
	return st.advance(outFD)
}
