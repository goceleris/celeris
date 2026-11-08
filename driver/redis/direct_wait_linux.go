//go:build linux

package redis

import (
	"time"
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
