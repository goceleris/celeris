//go:build linux

package errclass

import (
	"testing"

	"golang.org/x/sys/unix"
)

// celeris#886 item 13: an accept4 failure the engine retries in place is
// counted the way the same errno is when io_uring's accept completes with it.
// ECONNABORTED (a peer that reset between SYN and accept) lands in
// AcceptCancelled; EINTR (a signal) is retried and counted nowhere; anything
// else is not a retry at all and counts nothing here (AcceptFailed counts it).
func TestAcceptRetriedCountsAConnAborted886(t *testing.T) {
	for _, tc := range []struct {
		errno     unix.Errno
		retry     bool
		cancelled uint64
	}{
		{unix.ECONNABORTED, true, 1},
		{unix.EINTR, true, 0},
		{unix.EAGAIN, false, 0},
		{unix.EMFILE, false, 0},
		{unix.ENFILE, false, 0},
		{unix.EBADF, false, 0},
		{unix.ECANCELED, false, 0},
		{unix.EINVAL, false, 0},
	} {
		var c Counters
		if got := c.AcceptRetried(tc.errno); got != tc.retry {
			t.Errorf("AcceptRetried(%v) = %v, want %v", tc.errno, got, tc.retry)
		}
		s := c.Snapshot()
		if s.AcceptCancelled != tc.cancelled || s.Total() != tc.cancelled {
			t.Errorf("AcceptRetried(%v): AcceptCancelled = %d, Total = %d, want %d and %d",
				tc.errno, s.AcceptCancelled, s.Total(), tc.cancelled, tc.cancelled)
		}
	}
}

// TestAcceptRetriedAndFailedAgree886 pins the rule the two functions share:
// ECONNABORTED lands in the same bucket whichever of them sees it (epoll's
// retry, io_uring's completion), once.
func TestAcceptRetriedAndFailedAgree886(t *testing.T) {
	var a, b Counters
	a.AcceptRetried(unix.ECONNABORTED)
	b.AcceptFailed(unix.ECONNABORTED)
	if a.Snapshot() != b.Snapshot() {
		t.Errorf("ECONNABORTED retried in place = %+v, as an io_uring completion = %+v: the engines disagree", a.Snapshot(), b.Snapshot())
	}
}
