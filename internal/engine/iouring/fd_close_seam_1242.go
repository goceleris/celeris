//go:build linux

package iouring

import (
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// fdCloseSeam is a test seam (celeris#1242): when set, every close(2) the
// engine makes of a descriptor it owns (closeFD, closeFDErr) goes through it
// instead of unix.Close, on whichever goroutine closes. A test makes it take
// the number out of use in the same step as the close (dup3 of a placeholder
// over it, so no other open can get the number in between) and then reports a
// second close of a number nothing could have re-opened, which is a stale
// close in a real process: the number may by then name another connection or
// a sibling's listener. Several workers call it, so a test keeps its state
// atomic. Nil in production, where each close costs one load of this
// variable. An atomic.Pointer, as ringWaitSeam is, because a worker an
// earlier test left running may still read it when a later test sets or
// restores it (celeris#1229).
var fdCloseSeam atomic.Pointer[func(fd int) error]

// closeFDErr is close(2) of one descriptor number the engine owns, through
// fdCloseSeam. Every close of an engine-owned descriptor in this package goes
// through here (or closeFD, which drops the error): a bare unix.Close is
// invisible to the seam test of celeris#1242. The one other closer is the
// wake eventfd's handle (internal/wakefd), which retires the number under its
// lock before it closes it (celeris#655) and so cannot close it twice.
func closeFDErr(fd int) error {
	if h := fdCloseSeam.Load(); h != nil {
		return (*h)(fd)
	}
	return unix.Close(fd)
}
