//go:build linux

package iouring

import (
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// celeris#884 item 53: the single-shot accept (the base tier's, and the other
// tiers' when multishot accept is off) passed accept_flags 0, where the
// multishot paths pass SOCK_NONBLOCK|SOCK_CLOEXEC. Nothing else makes the
// accepted descriptor non-blocking (sockopts.ApplyFD does not), so on the base
// tier a detached producer's inline unix.Write waited for the client in the
// kernel, holding detachMu, and the descriptor was inheritable by a child
// process. The live site is Worker.prepareAccept; the three tiers'
// PrepareAccept methods have no caller today but build the same SQE.

// acceptedFlags884 arms an accept through arm, connects to the listener, reaps
// the accept's CQE and returns the status and descriptor flags of the fd it
// delivered.
func acceptedFlags884(t *testing.T, r *acceptRig896, arm func(*acceptRig896)) (fl, fdFlags int) {
	t.Helper()
	arm(r)
	if _, err := r.ring.Submit(); err != nil {
		t.Fatalf("submit accept: %v", err)
	}
	c, err := net.DialTimeout("tcp", r.addr, time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", r.addr, err)
	}
	defer func() { _ = c.Close() }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := r.ring.SubmitAndWaitTimeout(100 * time.Millisecond); err != nil {
			t.Fatalf("wait for the accept: %v", err)
		}
		head, tail := r.ring.BeginCQ()
		for ; head != tail; head++ {
			cqe := r.ring.cqeAt(head)
			if cqe.UserData&udMask != udAccept {
				continue
			}
			res := cqe.Res
			r.ring.EndCQ(head + 1)
			if res < 0 {
				t.Fatalf("accept completed with %v", unix.Errno(-res))
			}
			fd := int(res)
			defer func() { _ = unix.Close(fd) }()
			fl, err = unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
			if err != nil {
				t.Fatalf("F_GETFL: %v", err)
			}
			fdFlags, err = unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
			if err != nil {
				t.Fatalf("F_GETFD: %v", err)
			}
			return fl, fdFlags
		}
		r.ring.EndCQ(head)
	}
	t.Fatal("the accept never completed")
	return 0, 0
}

// TestAcceptedFDIsNonBlockingAndCloseOnExec884: whatever tier arms the
// accept, by the worker's own arm or by the tier's PrepareAccept, the
// accepted descriptor is O_NONBLOCK and FD_CLOEXEC. The multishot arms pass on
// the unfixed code and say the oracle can see both flags.
func TestAcceptedFDIsNonBlockingAndCloseOnExec884(t *testing.T) {
	viaWorker := func(r *acceptRig896) { r.w.prepareAccept() }
	viaTier := func(r *acceptRig896) { r.w.tier.PrepareAccept(r.ring, r.w.listenFD) }
	for _, tc := range []struct {
		name string
		tier func() TierStrategy
		arm  func(*acceptRig896)
	}{
		{"worker/base-single-shot", func() TierStrategy { return &baseTier{} }, viaWorker},
		{"worker/high-single-shot", func() TierStrategy { return &highTier{} }, viaWorker},
		{"worker/optional-single-shot", func() TierStrategy { return &optionalTier{} }, viaWorker},
		{"tier-method/base", func() TierStrategy { return &baseTier{} }, viaTier},
		{"tier-method/high-single-shot", func() TierStrategy { return &highTier{} }, viaTier},
		{"tier-method/optional-single-shot", func() TierStrategy { return &optionalTier{} }, viaTier},
		{"worker/high-multishot", func() TierStrategy { return &highTier{multishotAccept: true} }, viaWorker},
		{"tier-method/high-multishot", func() TierStrategy { return &highTier{multishotAccept: true} }, viaTier},
		{"tier-method/optional-multishot", func() TierStrategy { return &optionalTier{multishotAccept: true} }, viaTier},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newAcceptRig896(t, tc.tier(), false)
			fl, fdFlags := acceptedFlags884(t, r, tc.arm)
			t.Logf("celeris884 %s: F_GETFL=%#x (O_NONBLOCK=%v) F_GETFD=%#x (FD_CLOEXEC=%v)", tc.name,
				fl, fl&unix.O_NONBLOCK != 0, fdFlags, fdFlags&unix.FD_CLOEXEC != 0)
			if fl&unix.O_NONBLOCK == 0 {
				t.Errorf("the accepted descriptor is blocking (F_GETFL=%#x)", fl)
			}
			if fdFlags&unix.FD_CLOEXEC == 0 {
				t.Errorf("the accepted descriptor is not close-on-exec (F_GETFD=%#x)", fdFlags)
			}
		})
	}
}
