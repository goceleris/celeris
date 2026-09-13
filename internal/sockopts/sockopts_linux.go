//go:build linux

package sockopts

import (
	"time"

	"golang.org/x/sys/unix"
)

func applyFD(fd int, opts Options) error {
	if opts.TCPNoDelay {
		if err := unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1); err != nil {
			return err
		}
	}
	if opts.TCPQuickAck {
		if err := unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_QUICKACK, 1); err != nil {
			return err
		}
	}
	if opts.SOBusyPoll > 0 {
		micros := int(opts.SOBusyPoll.Microseconds())
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_BUSY_POLL, micros); err != nil {
			return err
		}
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, 69, 1) // SO_PREFER_BUSY_POLL
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, 70, 8) // SO_BUSY_POLL_BUDGET
	}
	if opts.RecvBuf > 0 {
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, opts.RecvBuf); err != nil {
			return err
		}
	}
	if opts.SendBuf > 0 {
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, opts.SendBuf); err != nil {
			return err
		}
	}
	return nil
}

// drainRecvBufSize is the size of one drain read.
//
// drainRecvFallbackBudget is the byte bound for one close when neither
// SIOCINQ nor SO_RCVBUF can be read (a non-socket fd, or a socket type
// without them). It is the 32 KiB cap this drain carried between celeris#311
// and celeris#572, kept only as that fallback.
//
// drainRecvTimeBudget bounds the drain in the unit celeris#571 is actually
// about: the harm of a drain that chases a peer is holding the event-loop
// thread, and with it every other connection on that loop. It is checked
// between the reads, not only around them, so it bounds the call and not just
// a loop outside it. With the window clamp below in effect a real close needs
// ~14 us of it (measured on the celeris#583 rig); the budget is what keeps a
// kernel that ignores the clamp bounded rather than chasing.
const (
	drainRecvBufSize        = 4096
	drainRecvFallbackBudget = 32 << 10
	drainRecvTimeBudget     = time.Millisecond
)

// DrainRecvBuffer reads and discards what is in fd's socket receive buffer, so
// the close(2) that follows sends FIN rather than RST. An RST would
// additionally destroy data still staged in the SEND buffer — a queued GOAWAY
// or WebSocket close frame the peer has not read yet. (close(2) frees the
// receive queue either way; draining is about the reset, not about the inbound
// bytes. See celeris#569.)
//
// The drain must leave the queue EMPTY to have that effect: close(2) resets on
// one unread byte. Two measured facts (celeris#583) shape what it does:
//
//   - The old fixed 32 KiB cap (8 x 4 KiB) sits below the autotuned receive
//     buffer, so on the close this exists for — a recv-paused connection whose
//     peer filled the window, 127-131 KiB queued — it stopped ~96 KiB short and
//     close(2) reset every time, exactly as if the drain had not run: 2304 of
//     2304 closes on both loop engines, the peer receiving neither the staged
//     echo backlog nor the WebSocket Close frame.
//   - Draining that snapshot is not enough either, because every read reopens
//     the receive window and the peer's ALREADY-COMMITTED backlog follows the
//     drain in: the same cell drained its full 128-129 KiB snapshot and still
//     found 64-126 KiB queued at close(2), resetting 32 of 32. Emptying it that
//     way took 20-22 snapshots and 2.6 MB — the peer's whole send buffer.
//
// So the drain first clamps the receive window shut (TCP_WINDOW_CLAMP below
// one MSS makes the kernel keep advertising a zero window), which is what turns
// the second fact around: the peer stays window-blocked where it already is,
// nothing follows the drain in, and the entry snapshot empties the queue.
// Measured on the same cell: ~128 KiB in ~14 us, and close(2) sends FIN on 32
// of 32 closes on both engines, the peer receiving the Close frame and every
// staged byte.
//
// TWO bounds hold one close, and BOTH are fixed before the first read:
//
//   - Bytes: at most min(SIOCINQ, SO_RCVBUF) — the queue as it stands once the
//     window is clamped, never more than the buffer that queue lives in.
//     Computed ONCE, by [drainRecvBudget]. A budget re-snapshotted per round
//     would not bound the call at all: a peer that keeps delivering extends it
//     round after round and only the wall clock ever stops it, which is
//     celeris#571's complaint.
//   - Time: drainRecvTimeBudget, checked BETWEEN THE READS, so the worst case
//     is that budget plus one 4 KiB read — not that budget plus a whole
//     SO_RCVBUF's worth of reading.
//
// Neither bound can be raised by anything the peer does after the call starts.
// Chasing further could not help anyway — bytes that arrive after close(2) are
// answered by the kernel with an RST no matter what userspace did beforehand,
// so the close is only ever as clean as the queue at the moment it runs.
//
// The byte bound is the one celeris#569's own fix candidate asked for ("drain
// up to the socket's SO_RCVBUF; one SIOCINQ read bounds the work"). Measurement
// added the clamp in front of it: without the clamp that same budget is spent
// on backlog the reads themselves invited in, and the queue is still not empty.
//
// The clamp is best-effort, and with a fixed byte bound behind it that is a
// real limit rather than a free one: on a kernel that ignores TCP_WINDOW_CLAMP,
// or on a socket that is not TCP, the reads reopen the window, the budget is
// spent on what follows them in, and the queue can still be non-empty at
// close(2) — that close then resets exactly as it did before this fix, which is
// the floor this replaces, not a regression below it. The dependency does not
// run the other way: clamping can never make a close worse than not clamping.
//
// What the clamp deliberately does NOT do is consume the backlog the peer has
// already queued. Those bytes stay in the peer's send buffer, and when its
// kernel pushes them at the socket we just closed it is answered with an RST,
// which purges its receive queue — our Close frame included — if it has not
// read by then. The alternative was measured: drain the backlog instead of
// clamping (20-22 snapshots, 2.6 MB, 250-550 us per close) and a peer that
// reads late keeps the Close frame on 128 of 128 flood-cell closes on epoll
// and 127 of 128 on io_uring, rather than 0 of 128 and 28 of 128 with the
// clamp. It was not taken because the cost is 20-40x this drain's and it is
// itself unreliable at the bound: with the same 1 ms budget the backlog drain
// overran on 1 of 768 closes per engine and that close reset — which is where
// the 127 of 128 above comes from — losing the frame for a prompt reader too.
// A peer that reads what is already in its receive queue before writing again
// — every well-behaved client, since the FIN is there to be read — sees the
// Close frame and the whole staged backlog either way, which is the outcome
// celeris#569 asks for.
//
// Clamping the window is safe HERE because of where this runs: all three call
// sites (epoll/closeConn, iouring/finishClose, iouring/finishCloseDetached)
// are shutdown(SHUT_WR) -> this -> close(2), with nothing in between. The
// socket is already half-closed and is destroyed microseconds later, so
// shrinking its receive window cannot throttle a connection that is still
// serving.
//
// It MUST NOT block. The io_uring worker calls it on the event-loop thread
// with the socket in BLOCKING mode, where a plain read waits for data or a FIN
// that a half-closed peer may never send — wedging the worker, and with enough
// concurrent detached closes the whole engine (celeris#311). MSG_DONTWAIT
// makes every read return EAGAIN the moment the queue is empty, on either
// engine's sockets.
//
// Both loop engines share this one implementation so the bound cannot drift
// back apart (celeris#571).
//
// It returns the number of bytes it consumed. The close path ignores the
// value; it exists so the drain's effect can be measured against the
// kernel's FIN/RST decision (celeris#583).
func DrainRecvBuffer(fd int) int {
	// An empty queue is the common close: one ioctl, nothing else.
	inq, inqErr := unix.IoctlGetInt(fd, unix.SIOCINQ)
	if inqErr == nil && inq <= 0 {
		return 0
	}
	// Shut the receive window before the first read, so the reads cannot
	// invite the peer's backlog in behind them. The kernel raises the value
	// to its own minimum, which is still below any MSS, so the advertised
	// window stays zero.
	_ = unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_WINDOW_CLAMP, 1)

	var buf [drainRecvBufSize]byte
	drained, _ := drainRecvRound(fd, buf[:],
		drainRecvBudget(fd, inq, inqErr),
		time.Now().Add(drainRecvTimeBudget))
	return drained
}

// drainRecvBudget is the byte bound for one close: the queue as SIOCINQ found
// it, never more than the SO_RCVBUF it lives in, and the 32 KiB fallback when
// the fd answers neither. It is called ONCE per close, before any read — a
// budget recomputed after a read is one the peer can extend (celeris#571).
func drainRecvBudget(fd int, inq int, inqErr error) int {
	budget := drainRecvFallbackBudget
	if rcvbuf, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF); err == nil && rcvbuf > 0 {
		budget = rcvbuf
	}
	if inqErr == nil && inq < budget {
		budget = inq
	}
	return budget
}

// drainRecvRound discards at most budget bytes from fd's receive queue, giving
// up at deadline, and reports whether a read found the queue empty (EAGAIN).
// Both bounds are enforced BETWEEN the reads, so neither the byte count nor
// the wall time can overrun by more than the single read in flight.
func drainRecvRound(fd int, buf []byte, budget int, deadline time.Time) (drained int, empty bool) {
	for drained < budget {
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return drained, false
		}
		chunk := buf
		if rem := budget - drained; rem < len(chunk) {
			chunk = chunk[:rem]
		}
		n, _, err := unix.Recvfrom(fd, chunk, unix.MSG_DONTWAIT)
		if n <= 0 || err != nil {
			return drained, true
		}
		drained += n
	}
	return drained, false
}
