//go:build linux

package redis

import (
	"context"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// directBlockSupported reports whether this platform can wait in poll(2).
const directBlockSupported = true

// directPollSlice is the longest the thread waits in one poll(2) call. At the
// end of each slice the wait looks at ctx again, so a cancel is seen within a
// millisecond (celeris#854).
const directPollSlice = time.Millisecond

// directPollBudget is the longest one wait stays in poll(2), slice after slice.
// When it is used up with the reply still not there, the wait is handed to Go's
// netpoll (tcp.Read), which parks the goroutine instead of holding the thread:
// a stalled server, or a ctx with no deadline, costs a thread for at most this
// long per wait, and the window in which the conn's fd is used outside Go's
// reference count is bounded by it too. It is a few times the 1-5 ms round
// trips the blocking wait is for.
const directPollBudget = 20 * time.Millisecond

// directPollIn waits on the calling thread, in poll(2), until fd is readable
// (data, FIN or an error, all of which the caller's next peek handles), ctx's
// deadline passes, ctx ends or directPollBudget is used up. It reports whether
// the caller should peek at fd again; false means fall back to tcp.Read, which
// owns the socket deadline and returns its error once the deadline is past. It
// counts one block per call, one slice expiry per slice that ended with nothing
// to read and one handoff when the budget ran out.
//
// The caller owns fd for the whole call: the conn is out of the pool and
// nothing else closes it (fd is outside Go's descriptor reference count, as it
// is for the non-blocking peek).
func directPollIn(ctx context.Context, fd sockFD, dl time.Time, hasDl bool) bool {
	directWaitStats.blocks.Add(1)
	start := time.Now()
	pfd := [1]unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for {
		if time.Since(start) >= directPollBudget {
			directWaitStats.handoffs.Add(1)
			return false
		}
		to := int(directPollSlice / time.Millisecond)
		if hasDl {
			rem := time.Until(dl)
			if rem <= 0 {
				return false
			}
			// Round up to whole milliseconds: poll(2) overshoots the deadline
			// by up to 1 ms, never undershoots it into a busy loop.
			if ms := int((rem + time.Millisecond - 1) / time.Millisecond); ms < to {
				to = ms
			}
		}
		n, err := unix.Poll(pfd[:], to)
		if n > 0 {
			return true
		}
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return false
		}
		directWaitStats.sliceExpiries.Add(1)
		if ctx.Err() != nil {
			return false
		}
	}
}
