//go:build linux

package iouring

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine/internal/errclass"
)

// celeris#886 item 13: an io_uring worker's pause-time accept drain retries an accept4 that failed with
// ECONNABORTED (a peer that reset between SYN and accept) or EINTR in place.
// The retried ECONNABORTED is counted in AcceptCancelled, as the same errno is
// when io_uring's accept completes with it; EINTR and anything that is not a
// retry count nothing here.
func TestIouringAcceptRetryCountsAConnAborted886(t *testing.T) {
	w := &Worker{errs: &errclass.Counters{}}
	for _, tc := range []struct {
		err       error
		retry     bool
		cancelled uint64 // AcceptCancelled after this call, cumulative
	}{
		{unix.ECONNABORTED, true, 1},
		{unix.EINTR, true, 1},
		{unix.ECONNABORTED, true, 2},
		{unix.EAGAIN, false, 2},
		{unix.EMFILE, false, 2},
		{errors.New("not an errno"), false, 2},
	} {
		if got := w.acceptRetry(tc.err); got != tc.retry {
			t.Errorf("acceptRetry(%v) = %v, want %v", tc.err, got, tc.retry)
		}
		if got := w.errs.Snapshot().AcceptCancelled; got != tc.cancelled {
			t.Errorf("after acceptRetry(%v): AcceptCancelled = %d, want %d", tc.err, got, tc.cancelled)
		}
	}
	if total := w.errs.Snapshot().Total(); total != 2 {
		t.Errorf("ErrorCount = %d after two retried ECONNABORTED, want 2", total)
	}
}
