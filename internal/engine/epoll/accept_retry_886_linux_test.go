//go:build linux

package epoll

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine/internal/errclass"
)

// celeris#886 item 13: an epoll loop retries an accept4 that failed with
// ECONNABORTED (a peer that reset between SYN and accept) or EINTR in place.
// The retried ECONNABORTED is counted in AcceptCancelled, as the same errno is
// when io_uring's accept completes with it; EINTR and anything that is not a
// retry count nothing here.
func TestEpollAcceptRetryCountsAConnAborted886(t *testing.T) {
	l := &Loop{errs: &errclass.Counters{}}
	for _, tc := range []struct {
		err       error
		cancelled uint64 // AcceptCancelled after this call, cumulative
	}{
		{unix.ECONNABORTED, 1},
		{unix.EINTR, 1},
		{unix.ECONNABORTED, 2},
		{unix.EAGAIN, 2},
		{unix.EMFILE, 2},
		{errors.New("not an errno"), 2},
	} {
		l.countAcceptRetry(tc.err)
		if got := l.errs.Snapshot().AcceptCancelled; got != tc.cancelled {
			t.Errorf("after countAcceptRetry(%v): AcceptCancelled = %d, want %d", tc.err, got, tc.cancelled)
		}
	}
	if total := l.errs.Snapshot().Total(); total != 2 {
		t.Errorf("ErrorCount = %d after two retried ECONNABORTED, want 2", total)
	}
}
