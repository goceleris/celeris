//go:build linux

package iouring

import (
	"errors"

	"golang.org/x/sys/unix"
)

// shutdownRingWait is the ring wait of endOwedOpsAtShutdown's drain, behind a
// var so a test can answer it the way a kernel with a full CQ ring does
// (celeris#873).
var shutdownRingWait = (*Ring).SubmitAndWaitTimeout

// ringWaitRetryable reports whether err, an enter's, says only that the CQ
// ring has no room for the completions the enter would reap or flush, so that
// reaping the ring and entering again is the answer. io_uring_enter(2) gives
// it as EBUSY, when IORING_FEAT_NODROP has overflowed completions that cannot
// be flushed into the full ring; kernels 5.19 to 6.1 return it from the wait
// after the submit has happened (io_cqring_wait: "if we can't even flush
// overflow, don't wait for more"), where 6.2 and later flush as the loop
// reaps and do not. EAGAIN is the kernel's "try again" (retryPending treats
// the two alike). Any other error is the ring failing.
func ringWaitRetryable(err error) bool {
	return errors.Is(err, unix.EBUSY) || errors.Is(err, unix.EAGAIN)
}
