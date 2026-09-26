//go:build linux

package iouring

import (
	"sync/atomic"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine/internal/errclass"
)

// fillSQ fills every free SQ slot with a NOP (opcode 0) so the next
// prepareAccept has nowhere to go — exactly the state a churn burst leaves
// the ring in when an accept termination CQE is processed mid-iteration.
func fillSQ(t *testing.T, ring *Ring) {
	t.Helper()
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
}

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
//
// It also pins the retry's gate (celeris#662). The gate is the LISTENER, not
// the pause: a paused worker whose listener is still open is lingering, and
// must keep taking the connections the kernel promotes into that listener's
// queue; a worker whose pause has closed the listener (listenFD < 0) must
// never arm an accept on it. It used to be "never re-arm while paused",
// which would leave a lingering worker deaf until its close.
// TestAcceptRearmedDuringLinger drives the same retry inside a real linger.
func TestAcceptRearmRetriedAfterSQFull(t *testing.T) {
	ring := newTestRing(t)
	var paused atomic.Bool
	w := &Worker{
		ring:         ring,
		listenFD:     3, // prepareAccept only encodes it
		errs:         &errclass.Counters{},
		tier:         &highTier{multishotAccept: true},
		acceptPaused: &paused,
	}

	fillSQ(t, ring)

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
	w.rearmAcceptIfPending()
	if got := ring.Pending(); got != 1 {
		t.Fatalf("accept was not re-armed after the ring drained: pending=%d (want 1)", got)
	}
	if w.acceptRearmPending {
		t.Fatal("pending flag still set after a successful re-arm")
	}

	// Paused, listener still open: the worker is lingering and must re-arm.
	paused.Store(true)
	w.acceptRearmPending = true
	w.rearmAcceptIfPending()
	if got := ring.Pending(); got != 2 {
		t.Fatalf("a paused worker whose listener is still open (lingering) did not re-arm "+
			"accept: pending=%d (want 2). Its listener is still receiving the connections the "+
			"kernel promotes after TCP_DEFER_ACCEPT is cleared (celeris#662)", got)
	}

	// Paused and the listener closed: nothing may be armed on it.
	w.listenFD = -1
	w.acceptRearmPending = true
	w.rearmAcceptIfPending()
	if got := ring.Pending(); got != 2 {
		t.Fatalf("a worker whose pause had closed its listener re-armed accept: pending=%d (want 2)", got)
	}
}

// TestAcceptRearmedDuringLinger is T7 of celeris#662. A worker lingers after
// a pause with its listener open and TCP_DEFER_ACCEPT cleared, and the
// connections the kernel promotes during that linger reach it only through
// an armed accept. If the multishot accept terminates while the SQ ring is
// full, the dropped re-arm must be retried during the linger, exactly as it
// is outside a pause. The linger here is entered through the real pause step
// on a real listen socket; the accept termination is forced.
func TestAcceptRearmedDuringLinger(t *testing.T) {
	ring := newTestRing(t)
	lfd, err := createListenSocket("127.0.0.1:0", true)
	if err != nil {
		t.Fatalf("createListenSocket: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(lfd) })

	var paused atomic.Bool
	paused.Store(true)
	w := &Worker{
		ring:         ring,
		listenFD:     lfd,
		deferCapable: true,
		errs:         &errclass.Counters{},
		tier:         &highTier{multishotAccept: true},
		acceptPaused: &paused,
	}

	// ACTIVE → LINGERING, through the step the run loop calls.
	w.stepAcceptPause(t.Context(), true)
	if w.lingerUntil == 0 || w.listenFD != lfd {
		t.Fatalf("the pause step did not enter a linger (lingerUntil=%d listenFD=%d)",
			w.lingerUntil, w.listenFD)
	}
	if v, gerr := unix.GetsockoptInt(lfd, unix.IPPROTO_TCP, unix.TCP_DEFER_ACCEPT); gerr != nil || v != 0 {
		t.Fatalf("premise: TCP_DEFER_ACCEPT reads %d (%v) during the linger, want 0", v, gerr)
	}

	fillSQ(t, ring)
	pendingBefore := ring.Pending()
	w.handleAccept(t.Context(), &completionEntry{Res: -12, Flags: 0}, 0, 0)
	if got := ring.Pending(); got != pendingBefore || !w.acceptRearmPending {
		t.Fatalf("premise: the forced termination did not leave a pending re-arm "+
			"(pending %d→%d, acceptRearmPending=%v)", pendingBefore, got, w.acceptRearmPending)
	}
	if _, err := ring.Submit(); err != nil {
		t.Fatalf("submit NOPs: %v", err)
	}

	w.rearmAcceptIfPending()
	if got := ring.Pending(); got != 1 || w.acceptRearmPending {
		t.Fatalf("a lingering worker did not re-arm its dropped accept: pending=%d (want 1), "+
			"acceptRearmPending=%v. Until its close, the connections TCP_DEFER_ACCEPT held "+
			"back reach this listener's queue, and only an armed accept takes them "+
			"(celeris#662)", got, w.acceptRearmPending)
	}
}
