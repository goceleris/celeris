//go:build linux

package epoll

import (
	"golang.org/x/sys/unix"
)

// parkedMinAcked is the least number of bytes the peer must acknowledge in one
// WriteTimeout window, for a response a keep-alive conn has parked on the
// socket, for the conn to be held another window (celeris#885 item 51). With
// WriteTimeout T the floor is parkedMinAcked/T: 64 KiB/s at 1 s, about 1 KiB/s
// at the 60 s default. The constant, and the window being WriteTimeout, are the
// rule; see parkedBelowRate.
const parkedMinAcked = 64 << 10

// parkedBelowRate is the sweep's minimum-rate rule for a live conn that holds a
// response the kernel has not taken whole (celeris#885 item 51); it reports
// whether the conn is to be closed. It replaces "no flush took bytes for
// WriteTimeout", which sees the peer only at an EPOLLOUT edge (about half a send
// queue drained) or a read of the peer, as the measure of a slow reader:
//
//   - a client that reads steadily but takes less than half a send queue per
//     WriteTimeout is closed while it reads (no edge, no flush);
//   - one that takes about half a queue per WriteTimeout, an edge every time,
//     is never closed, and a client that sends bytes while it reads is held
//     longer still (a pipelined request, an HTTP/2 PING).
//
// The measure here is the bytes the peer has acknowledged (tcp_info
// bytes_acked, which only grows, read by one getsockopt), and nothing the peer
// sends moves it. Windows of WriteTimeout: the first sweep that finds the
// response parked takes the baseline and opens a window (closeAcked,
// closeSampled; closeAckedKnown says there is one), and when the window has
// passed the conn is held another if the peer acknowledged at least
// parkedMinAcked in it (the baseline moves to that sample and so does the write
// clock, writeSince, which ReadTimeout and IdleTimeout slide with) and closed
// if it did not. The sample is one getsockopt per conn per window.
//
// So a peer that takes nothing (a zero window, a byte per second: the
// kernel acknowledges a window update, not a byte) is closed between
// WriteTimeout and WriteTimeout plus a sweep after the response parked, and one
// that stops after a window it passed within two WriteTimeouts. A peer that
// takes exactly parkedMinAcked per window holds the conn and what is queued for
// it for (response / parkedMinAcked) windows: there is no elapsed-time ceiling,
// and a client that takes the response at a reasonable rate is never cut.
//
// While the baseline is known the flush clock does not reap: its stamp moves at
// flushes the peer's reading did not cause as well (a request read, a flush of a
// byte), and the rule here is the one that counts bytes taken. A conn whose
// socket does not report them (not TCP, a failed getsockopt, which is retried
// at most every quarter of WriteTimeout) has the flush clock alone, as before.
// ws is the flush clock, writeSince. Loop thread; the caller's conn is parked
// (pending) and WriteTimeout wt > 0.
func (l *Loop) parkedBelowRate(cs *connState, now, ws, wt int64) bool {
	if !cs.closeAckedKnown {
		if now-cs.closeSampled < wt/4 {
			return false // a failed sample, not tried again yet
		}
		cs.closeSampled = now
		acked, err := l.bytesAcked(cs.fd)
		if err != nil {
			return false
		}
		cs.closeAcked, cs.closeAckedKnown = acked, true
		// A conn first met after the flush clock's bound had passed has no
		// window behind it and no evidence of progress: it is judged by the
		// clock, as before (a loop whose sweeps are further apart than
		// WriteTimeout).
		return now-ws > wt
	}
	if now-cs.closeSampled < wt {
		return false
	}
	acked, err := l.bytesAcked(cs.fd)
	if err != nil {
		cs.closeAckedKnown = false // the flush clock from here
		cs.closeSampled = now
		return false
	}
	if acked-cs.closeAcked >= parkedMinAcked {
		cs.closeAcked, cs.closeSampled = acked, now
		cs.writeSince = now
		return false
	}
	return true
}

// bytesAcked reads how many bytes the peer of fd has acknowledged (tcp_info
// bytes_acked, which only grows).
func (l *Loop) bytesAcked(fd int) (uint64, error) {
	if f := l.ackedProbe; f != nil {
		return f(fd)
	}
	ti, err := unix.GetsockoptTCPInfo(fd, unix.IPPROTO_TCP, unix.TCP_INFO)
	if err != nil {
		return 0, err
	}
	return ti.Bytes_acked, nil
}
