//go:build linux

package iouring

import (
	"sync/atomic"
	"testing"
	"unsafe"
)

// TestAcceptRearmRetriedAfterSQFull guards the accept-loss found by the
// 2026-09-04 24h soak (auth_session_ratelimit / io_uring / arm64: new
// connections hung ≥10s while keep-alive traffic stayed healthy).
//
// In multishot mode the listen socket has exactly one accept SQE in flight
// and it is re-armed only from handleAccept when a CQE arrives without
// F_MORE. prepareAccept used to return silently when GetSQE found the SQ
// ring full, and nothing retried: that worker's SO_REUSEPORT listen socket
// went deaf while the kernel kept completing TCP handshakes into its
// backlog, so clients "connected" and then waited forever.
//
// The test fills the SQ ring, terminates the multishot accept (error CQE,
// F_MORE clear), drains the ring with Submit, and asserts that the loop's
// per-iteration retry re-arms accept.
func TestAcceptRearmRetriedAfterSQFull(t *testing.T) {
	ring := newTestRing(t)
	w := &Worker{
		ring:     ring,
		listenFD: 3, // prepareAccept only encodes it
		errCount: &atomic.Uint64{},
		tier:     &highTier{multishotAccept: true},
	}

	// Fill every SQ slot with a NOP (opcode 0) so the accept re-arm has
	// nowhere to go — exactly the state a churn burst leaves the ring in
	// when the termination CQE is processed mid-iteration.
	filled := 0
	for {
		sqe := ring.GetSQE()
		if sqe == nil {
			break
		}
		clear(unsafe.Slice((*byte)(sqe), sqeSize))
		setSQEUserData(sqe, 0)
		filled++
	}
	if filled == 0 {
		t.Fatal("could not fill the SQ ring")
	}

	// Kernel terminated the multishot accept: ENOMEM, F_MORE clear.
	pendingBefore := ring.Pending()
	w.handleAccept(t.Context(), &completionEntry{Res: -12, Flags: 0}, 0, 0)
	if got := ring.Pending(); got != pendingBefore {
		t.Fatalf("re-arm landed on a full ring: pending %d→%d", pendingBefore, got)
	}
	if !w.acceptRearmPending {
		t.Fatal("dropped accept re-arm was not recorded as pending")
	}

	// The loop submits (NOPs are consumed, ring drains) and then retries.
	if _, err := ring.Submit(); err != nil {
		t.Fatalf("submit NOPs: %v", err)
	}
	w.rearmAcceptIfPending(false)
	if got := ring.Pending(); got != 1 {
		t.Fatalf("accept was not re-armed after the ring drained: pending=%d (want 1)", got)
	}
	if w.acceptRearmPending {
		t.Fatal("pending flag still set after a successful re-arm")
	}

	// A paused worker (listen socket being closed) must not re-arm.
	w.acceptRearmPending = true
	w.rearmAcceptIfPending(true)
	if got := ring.Pending(); got != 1 {
		t.Fatalf("paused worker re-armed accept: pending=%d (want 1)", got)
	}
}
