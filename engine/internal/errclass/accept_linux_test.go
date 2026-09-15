//go:build linux

package errclass

import (
	"testing"

	"golang.org/x/sys/unix"
)

// TestAcceptFailedRoutesEachErrno pins the classification table itself. The
// point of celeris#645's split is that "the host is out of descriptors",
// "the accept was cancelled or the peer left" and "something else" are three
// different findings; routing one into another is exactly the mistake the
// single counter used to make by construction, and the reflective guards
// cannot see it because every bucket is still nonzero.
func TestAcceptFailedRoutesEachErrno(t *testing.T) {
	for _, tc := range []struct {
		errno  unix.Errno
		bucket string
	}{
		{unix.EMFILE, "AcceptFDLimit"},
		{unix.ENFILE, "AcceptFDLimit"},
		{unix.ECANCELED, "AcceptCancelled"},
		{unix.EBADF, "AcceptCancelled"},
		{unix.ECONNABORTED, "AcceptCancelled"},
		{unix.EINTR, "AcceptCancelled"},
		{unix.ENOMEM, "AcceptOther"},
		{unix.EPERM, "AcceptOther"},
		{unix.EINVAL, "AcceptOther"},
	} {
		var c Counters
		c.AcceptFailed(tc.errno)
		s := c.Snapshot()
		got := map[string]uint64{
			"AcceptFDLimit":   s.AcceptFDLimit,
			"AcceptCancelled": s.AcceptCancelled,
			"AcceptOther":     s.AcceptOther,
		}
		for name, v := range got {
			want := uint64(0)
			if name == tc.bucket {
				want = 1
			}
			if v != want {
				t.Errorf("AcceptFailed(%v): %s = %d, want %d (expected bucket %s)",
					tc.errno, name, v, want, tc.bucket)
			}
		}
		if s.Total() != 1 {
			t.Errorf("AcceptFailed(%v): Total = %d, want exactly 1 — one accept failure "+
				"must land in exactly one bucket", tc.errno, s.Total())
		}
	}
}
