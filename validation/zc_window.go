//go:build validation

package validation

import (
	"sync/atomic"
	"time"
)

// zcWindowHoldNanos is the celeris#587 window hold. See [ZCWindowHold].
var zcWindowHoldNanos atomic.Int64

// Enabled is true in a -tags=validation build and false (a constant, so
// guarded code compiles away) in production (disabled.go).
const Enabled = true

// SetZCWindowHold makes every io_uring worker pause for d right after its
// handleSend has recorded a SEND_ZC first completion (CQE_F_MORE:
// cs.zcNotifPending set, cs.detachMu released on return) and before it
// does anything else; d <= 0 turns the pause off. It exists for one
// measurement only (celeris#587) and is compiled in only under
// -tags=validation: in production the call sites are guarded by the false
// Enabled constant and compile away.
//
// Why it is needed. Between the first completion of a SEND_ZC (the kernel
// has queued the bytes) and its notification (the kernel has released the
// pinned buffer) the detached inline-egress guard on the dispatch goroutine
// must refuse the raw unix.Write fast path; the two goroutines hand
// cs.sending / cs.zcNotifPending to each other under cs.detachMu. Two
// things hid that hand-off from the race detector:
//
//  1. On loopback the kernel copies (IORING_NOTIF_USAGE_ZC_COPIED), so the
//     window is a few hundred nanoseconds wide: celeris#601 measured
//     IouringInlineGuardBlockedZC == 0 on an unmodified build.
//  2. Where the window does open naturally (the notification waits for a
//     slow reader), the worker's own per-iteration flush takes and releases
//     cs.detachMu before the dispatch goroutine reads, which orders the two
//     accesses: -race cannot report a missing lock around the first
//     completion then, whatever the code does (celeris#587 review 2).
//
// Holding the worker HERE -- after the first completion's writes and the
// unlock, before any other release -- makes every guarded read during the
// hold concurrent with those writes unless the lock is present. The
// window's state machine is unchanged; only its duration is.
func SetZCWindowHold(d time.Duration) {
	if d < 0 {
		d = 0
	}
	zcWindowHoldNanos.Store(int64(d))
}

// ZCWindowHold sleeps for the duration set by [SetZCWindowHold], if any.
func ZCWindowHold() {
	if d := zcWindowHoldNanos.Load(); d > 0 {
		time.Sleep(time.Duration(d))
	}
}
