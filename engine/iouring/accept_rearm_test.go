//go:build linux

package iouring

import (
	"context"
	"sync/atomic"
	"testing"
)

// newTestRing creates a bare io_uring for unit tests that need to observe SQE
// submission. Skips the test when io_uring is unavailable (CI runners without
// the syscall, or RLIMIT_MEMLOCK too low). Plain flags (no SINGLE_ISSUER /
// SQPOLL) keep it usable from the test goroutine without OS-thread pinning.
func newTestRing(t *testing.T) *Ring {
	t.Helper()
	ring, err := NewRing(64, 0, 0)
	if err != nil {
		t.Skipf("io_uring unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ring.Close() })
	return ring
}

// TestHandleAcceptRearmsOnError is the regression guard for v1.5.0 review 2.1:
// an error CQE on the accept SQE with F_MORE cleared must re-arm accept — even
// in multishot mode — otherwise the worker permanently stops accepting after a
// transient ENOMEM / EMFILE. The pre-fix code only re-armed when the tier did
// NOT support multishot accept, so a multishot worker silently went deaf.
func TestHandleAcceptRearmsOnError(t *testing.T) {
	ring := newTestRing(t)
	w := &Worker{
		ring:     ring,
		listenFD: 3, // any valid-looking fd; prepareAccept only encodes it
		errCount: &atomic.Uint64{},
		// highTier with multishotAccept reports multishot accept support so
		// the test exercises the multishot branch the bug lived in.
		tier: &highTier{multishotAccept: true},
	}

	before := ring.Pending()
	// Synthetic error CQE: ENOMEM (-12), F_MORE cleared (Flags=0) → kernel
	// terminated the (multishot) accept.
	w.handleAccept(context.Background(), &completionEntry{Res: -12, Flags: 0}, 0, 0)
	after := ring.Pending()

	if after != before+1 {
		t.Fatalf("handleAccept did not submit a re-arm accept SQE: pending %d→%d (want +1)", before, after)
	}
	if got := w.errCount.Load(); got != 1 {
		t.Errorf("errCount = %d, want 1 (the error CQE should be counted)", got)
	}
}

// TestHandleAcceptNoRearmWhenMoreSet verifies that an error CQE with F_MORE
// still set (multishot continues) does NOT submit a redundant accept SQE —
// the kernel will keep delivering completions from the existing SQE.
func TestHandleAcceptNoRearmWhenMoreSet(t *testing.T) {
	ring := newTestRing(t)
	w := &Worker{
		ring:     ring,
		listenFD: 3,
		errCount: &atomic.Uint64{},
		tier:     &highTier{multishotAccept: true},
	}

	before := ring.Pending()
	w.handleAccept(context.Background(), &completionEntry{Res: -12, Flags: cqeFMore}, 0, 0)
	after := ring.Pending()

	if after != before {
		t.Fatalf("handleAccept re-armed despite F_MORE set: pending %d→%d (want unchanged)", before, after)
	}
}
