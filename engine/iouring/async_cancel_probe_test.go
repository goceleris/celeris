//go:build linux

package iouring

import (
	"context"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/probe"
	"github.com/goceleris/celeris/resource"
)

// celeris#681 C1: the hand-off's REAP cancels an armed recv with
// IORING_ASYNC_CANCEL_ALL, and cancel flags exist only from Linux 5.19.
// Through 5.18 the kernel fails every such cancel with -EINVAL and leaves the
// recv armed (measured on 5.15.0-191), so the engine probes for the flags at
// startup and places no reap where they are rejected.

// TestAsyncCancelProbeClassifies pins how the probe reads its cancel's
// completion: a cancel with CANCEL_ALL of a user_data nothing carries.
func TestAsyncCancelProbeClassifies(t *testing.T) {
	for _, tc := range []struct {
		res    int32
		ok     bool
		reason string
	}{
		{0, true, ""}, // CANCEL_ALL: res is the number cancelled, and nothing was
		{1, true, ""},
		{-int32(unix.ENOENT), true, ""}, // the flag-free form's miss
		{-int32(unix.EINVAL), false, "5.19"},
		{-int32(unix.EBADF), false, "cqe.res=-9"},
		{-int32(unix.ECANCELED), false, "cqe.res=-125"},
	} {
		ok, reason := classifyAsyncCancelProbe(tc.res)
		if ok != tc.ok || (tc.reason == "") != (reason == "") || !strings.Contains(reason, tc.reason) {
			t.Errorf("classifyAsyncCancelProbe(%d) = (%v, %q), want (%v, containing %q)",
				tc.res, ok, reason, tc.ok, tc.reason)
		}
	}
}

// kernelHasCancelFlags reports whether the running kernel's version is one
// that has IORING_ASYNC_CANCEL flags (5.19 or later).
func kernelHasCancelFlags(t *testing.T) (bool, string) {
	t.Helper()
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		t.Fatalf("uname: %v", err)
	}
	rel := unix.ByteSliceToString(u.Release[:])
	kv, err := probe.ParseKernelVersion(rel)
	if err != nil {
		t.Fatalf("parse kernel release %q: %v", rel, err)
	}
	return kv.AtLeast(5, 19), rel
}

// TestAsyncCancelProbeOnThisKernel runs the probe itself against the running
// kernel and checks its answer against the kernel's version.
func TestAsyncCancelProbeOnThisKernel(t *testing.T) {
	r, err := NewRing(8, 0, 0)
	if err != nil {
		skipOrFail656(t, "io_uring unavailable: %v", err)
	}
	_ = r.Close()
	ok, reason := probeAsyncCancelFlags()
	want, rel := kernelHasCancelFlags(t)
	t.Logf("celeris681 async cancel flags probe: kernel=%s ok=%v reason=%q", rel, ok, reason)
	if ok != want {
		t.Fatalf("probeAsyncCancelFlags() = (%v, %q) on kernel %s, want %v", ok, reason, rel, want)
	}
	if cok, _ := probeAsyncCancelFlagsCached(); cok != ok {
		t.Fatalf("the cached probe says %v, the probe %v", cok, ok)
	}
}

// TestWorkersCarryTheAsyncCancelProbe: New stores the probe's answer and
// createWorkers gives every worker that answer, so a worker on a kernel
// without cancel flags never places a reap, and one on a kernel with them
// does.
func TestWorkersCarryTheAsyncCancelProbe(t *testing.T) {
	e, err := New(resource.Config{
		Addr:      "127.0.0.1:0",
		Protocol:  engine.HTTP1,
		Resources: resource.Resources{Workers: 2},
	}, transplantTestHandler{})
	if err != nil {
		skipOrFail656(t, "iouring engine unavailable: %v", err)
	}
	if want, _ := probeAsyncCancelFlagsCached(); e.asyncCancelFlags != want {
		t.Fatalf("New stored asyncCancelFlags=%v, the probe says %v", e.asyncCancelFlags, want)
	}
	resolved := e.cfg.Resources.Resolve()
	for _, answer := range []bool{false, true} {
		e.asyncCancelFlags = answer
		workers, err := e.createWorkers(SelectTier(e.profile, 0), make([]int, resolved.Workers), resolved)
		if err != nil {
			skipOrFail656(t, "cannot create io_uring workers here: %v", err)
		}
		for i, w := range workers {
			if w.asyncCancelFlags != answer {
				t.Errorf("engine answer %v: worker %d has asyncCancelFlags=%v", answer, i, w.asyncCancelFlags)
			}
		}
		for _, w := range workers {
			w.shutdown()
		}
	}
}

// TestReapOnTheRunningKernel drives the hand-off of an idle conn whose recv
// is armed through the running kernel, which completes every op: with the
// probe's own answer, and with the flags taken as rejected. With the flags a
// reap cancels the recv and the conn is handed off at its -ECANCELED. Without
// them no reap is placed, on this iteration or any later one; the client's
// next request completes the recv, its response is held, and the conn is
// handed off at that SEND's completion with nothing in flight.
func TestReapOnTheRunningKernel(t *testing.T) {
	// run submits and lets the kernel complete ops until pred holds,
	// processing every completion the way the worker loop does.
	run := func(t *testing.T, f *fdlFixture, what string, pred func() bool) {
		t.Helper()
		if _, err := f.w.ring.Submit(); err != nil {
			t.Fatalf("%s: submit: %v", what, err)
		}
		for deadline := time.Now().Add(2 * time.Second); !pred(); {
			if time.Now().After(deadline) {
				t.Fatalf("%s: not done within 2s (adopted %d, recvArmed %v, sending %v)",
					what, f.tgt.adopted.Load(), f.cs.recvArmed, f.cs.sending)
			}
			_ = f.w.ring.WaitCQETimeout(50 * time.Millisecond)
			head, tail := f.w.ring.BeginCQ()
			for ; head != tail; head++ {
				c := *f.w.ring.cqeAt(head)
				f.w.processCQE(context.Background(), &c, time.Now().UnixNano())
			}
			f.w.ring.EndCQ(head)
			if _, err := f.w.ring.Submit(); err != nil {
				t.Fatalf("%s: submit: %v", what, err)
			}
		}
	}
	idleArmed := func(t *testing.T, flags bool) *fdlFixture {
		t.Helper()
		f := newFDLFixture(t, false)
		f.w.asyncCancelFlags = flags
		if !f.w.prepareRecv(f.cs, f.cs.buf) {
			t.Fatal("arm refused")
		}
		if _, err := f.w.ring.Submit(); err != nil {
			t.Fatalf("submit recv: %v", err)
		}
		f.startDrain()
		f.w.tryTransplant(f.fd)
		return f
	}
	withoutFlags := func(t *testing.T, f *fdlFixture) {
		t.Helper()
		if n := f.w.ring.Pending(); n != 0 {
			t.Fatalf("tryTransplant placed %d SQE(s) without cancel flags, want none", n)
		}
		for i := 1; i <= 3; i++ {
			f.w.drainDetachQueue()
			if n := f.w.ring.Pending(); n != 0 {
				t.Fatalf("loop iteration %d placed %d SQE(s) without cancel flags, want none", i, n)
			}
		}
		if f.tgt.adopted.Load() != 0 || !f.cs.recvArmed {
			t.Fatalf("adopted %d, recvArmed %v: want 0 and true", f.tgt.adopted.Load(), f.cs.recvArmed)
		}
		if _, err := unix.Write(f.peer, []byte(fdlGET)); err != nil {
			t.Fatalf("client write: %v", err)
		}
		run(t, f, "the next request", func() bool { return f.tgt.adopted.Load() == 1 })
		if n := f.e.metrics.handoffLoss.handoffInFlight.Load(); n != 0 {
			t.Fatalf("TransplantHandoffInFlight = %d, want 0", n)
		}
		if n := f.e.metrics.handoffLoss.held.Load(); n != 1 {
			t.Fatalf("TransplantHeld = %d, want 1: the conn left after its held response", n)
		}
		if n := f.e.metrics.handoffLoss.reaps.Load(); n != 0 {
			t.Fatalf("TransplantReaps = %d, want 0", n)
		}
	}

	t.Run("probe_answer", func(t *testing.T) {
		ok, _ := probeAsyncCancelFlagsCached()
		want, rel := kernelHasCancelFlags(t)
		t.Logf("celeris681 kernel=%s probe=%v", rel, ok)
		if ok != want {
			t.Fatalf("the probe says %v on kernel %s, want %v", ok, rel, want)
		}
		f := idleArmed(t, ok)
		if !ok {
			withoutFlags(t, f)
			return
		}
		if n := f.w.ring.Pending(); n != 1 {
			t.Fatalf("tryTransplant placed %d SQE(s), want the reap", n)
		}
		run(t, f, "the reap", func() bool { return f.tgt.adopted.Load() == 1 })
		if n := f.e.metrics.handoffLoss.handoffInFlight.Load(); n != 0 {
			t.Fatalf("TransplantHandoffInFlight = %d, want 0", n)
		}
	})

	t.Run("flags_rejected", func(t *testing.T) {
		withoutFlags(t, idleArmed(t, false))
	})
}
