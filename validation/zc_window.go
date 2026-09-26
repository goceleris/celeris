//go:build validation

package validation

import (
	"sync/atomic"
	"time"
)

// zcNotifDelayNanos is the celeris#587 window widener. See [ZCNotifDelay].
var zcNotifDelayNanos atomic.Int64

// SetZCNotifDelay makes every io_uring worker pause for d before it
// processes a SEND_ZC notification completion (CQE_F_NOTIF); d <= 0
// turns the pause off. It exists for one measurement only (celeris#587)
// and is compiled in only under -tags=validation: the production stub in
// disabled.go is an empty function, so the call site in the engine's
// NOTIF branch inlines to nothing.
//
// Why it is needed. Between the first completion of a SEND_ZC (the
// kernel has queued the bytes, CQE_F_MORE set, cs.zcNotifPending = true)
// and its notification (the kernel has released the pinned buffer,
// cs.zcNotifPending = false) the detached inline-egress guard on the
// dispatch goroutine must refuse the raw unix.Write fast path, and the
// two goroutines hand cs.sending / cs.zcNotifPending to each other under
// cs.detachMu. On loopback the kernel copies (IORING_NOTIF_USAGE_ZC_COPIED),
// so the notification lands in the same completion batch as the first
// completion and that window is a few hundred nanoseconds wide:
// celeris#601 measured IouringInlineGuardBlockedZC == 0 on an unmodified
// build. A race detector cannot judge an interleaving that never happens,
// so the #587 test holds the window open for d, on the worker thread,
// with no lock held, exactly where a real NIC's DMA latency would hold it
// open. The window's state machine is unchanged: only its duration is.
func SetZCNotifDelay(d time.Duration) {
	if d < 0 {
		d = 0
	}
	zcNotifDelayNanos.Store(int64(d))
}

// ZCNotifDelay sleeps for the duration set by [SetZCNotifDelay], if any.
// Called by the io_uring worker at the top of its SEND_ZC notification
// branch, before it takes cs.detachMu.
func ZCNotifDelay() {
	if d := zcNotifDelayNanos.Load(); d > 0 {
		time.Sleep(time.Duration(d))
	}
}
