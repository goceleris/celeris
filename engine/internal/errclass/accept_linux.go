//go:build linux

package errclass

import "golang.org/x/sys/unix"

// AcceptFailed routes one failed accept to its bucket by errno, so epoll's
// accept4 return and io_uring's negated completion result are classified by
// the same table. Pass the POSITIVE errno (io_uring callers negate Res first).
//
// The buckets are deliberately coarse. What celeris#645 needs to tell apart is
// "the host ran out of descriptors", "the accept was torn down or the peer
// left" and "something else"; the exact errno behind the third is a log line's
// job, not a metric's.
func (c *Counters) AcceptFailed(errno unix.Errno) {
	switch errno {
	case unix.EMFILE, unix.ENFILE:
		c.AcceptFDLimit.Add(1)
	case unix.ECANCELED, unix.EBADF, unix.ECONNABORTED, unix.EINTR:
		// ECANCELED and EBADF are what a PauseAccept leaves behind: the
		// in-flight accept is cancelled and the listen descriptor closed
		// out from under any re-arm that raced the close. On the adaptive
		// engine that is a switch cost, not a fault.
		c.AcceptCancelled.Add(1)
	default:
		c.AcceptOther.Add(1)
	}
}

// SendFailed routes one failed send completion to its bucket by errno. Pass
// the POSITIVE errno (io_uring callers negate Res first).
//
// The only distinction drawn here is the one celeris#645 needed: a peer that
// left before the response flushed is a property of the CLIENT population and
// the load, and it is what an io_uring column counts while an epoll column
// counts nothing at all. Everything else is a transmit fault.
func (c *Counters) SendFailed(errno unix.Errno) {
	switch errno {
	case unix.EPIPE, unix.ECONNRESET, unix.ECONNABORTED, unix.ENOTCONN:
		c.SendPeerGone.Add(1)
	default:
		c.Send.Add(1)
	}
}
