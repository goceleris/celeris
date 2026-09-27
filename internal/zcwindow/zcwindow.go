//go:build validation

package zcwindow

import (
	"sync/atomic"
	"time"
)

// holdNanos is the window hold. See [Hold].
var holdNanos atomic.Int64

// Enabled is true in a -tags=validation build and false (a constant, so
// guarded code compiles away) in production (zcwindow_off.go).
const Enabled = true

// SetHold makes every io_uring worker pause for d right after its
// handleSend has recorded a SEND_ZC first completion (CQE_F_MORE:
// cs.zcNotifPending set, cs.detachMu released on return) and before it
// does anything else; d <= 0 turns the pause off. It exists for one
// measurement only (celeris#587).
//
// Why it is needed. Between the first completion of a SEND_ZC (the kernel
// has queued the bytes) and its notification (the kernel has released the
// pinned buffer) the detached inline-egress guard on the dispatch goroutine
// must refuse the raw unix.Write fast path; the two goroutines hand
// cs.sending / cs.zcNotifPending to each other under cs.detachMu. The race
// detector can only judge that hand-off when a guarded read and a
// first-completion write meet with no other release of cs.detachMu between
// them, and how often that happens depends on the load and the host: on
// loopback with a fast reader the notification lands in the same
// completion batch (celeris#601 measured IouringInlineGuardBlockedZC == 0),
// while a slow reader keeps it pending long enough to be seen. Holding the
// worker HERE -- after the first completion's writes and the unlock,
// before any other release -- makes every guarded read during the hold
// concurrent with those writes unless the lock is present, so the detector
// control does not depend on the host's timing. The window's state machine
// is unchanged; only its duration is.
func SetHold(d time.Duration) {
	if d < 0 {
		d = 0
	}
	holdNanos.Store(int64(d))
}

// Hold sleeps for the duration set by [SetHold], if any.
func Hold() {
	if d := holdNanos.Load(); d > 0 {
		time.Sleep(time.Duration(d))
	}
}
