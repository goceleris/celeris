//go:build linux

package iouring

import (
	"context"
	"errors"
	"log/slog"
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
// completion: a cancel with CANCEL_ALL of a user_data nothing carries. A
// completion is the kernel's answer, accepted or rejected; a probe that fails
// before reading one has no answer, which is not a rejection (celeris#681 R2).
func TestAsyncCancelProbeClassifies(t *testing.T) {
	for _, tc := range []struct {
		res    int32
		want   asyncCancelProbe
		reason string
	}{
		{0, asyncCancelAccepted, ""}, // CANCEL_ALL: res is the number cancelled, and nothing was
		{1, asyncCancelAccepted, ""},
		{-int32(unix.ENOENT), asyncCancelAccepted, ""}, // the flag-free form's miss
		{-int32(unix.EINVAL), asyncCancelRejected, "5.19"},
		{-int32(unix.EBADF), asyncCancelRejected, "cqe.res=-9"},
		{-int32(unix.ECANCELED), asyncCancelRejected, "cqe.res=-125"},
	} {
		got, reason := classifyAsyncCancelProbe(tc.res)
		if got != tc.want || (tc.reason == "") != (reason == "") || !strings.Contains(reason, tc.reason) {
			t.Errorf("classifyAsyncCancelProbe(%d) = (%v, %q), want (%v, containing %q)",
				tc.res, got, reason, tc.want, tc.reason)
		}
	}

	// The probe's ring cannot be set up: nothing reached the kernel.
	t.Run("no_answer_is_not_a_rejection", func(t *testing.T) {
		saved := newAsyncCancelProbeRing
		newAsyncCancelProbeRing = func() (*Ring, error) { return nil, errors.New("celeris681 injected: no ring") }
		t.Cleanup(func() { newAsyncCancelProbeRing = saved })
		got, reason := probeAsyncCancel(cancelAll)
		if got != asyncCancelNoAnswer || !strings.Contains(reason, "injected") {
			t.Errorf("a probe whose ring could not be set up = (%v, %q), want (%v, naming the failure)",
				got, reason, asyncCancelNoAnswer)
		}
	})

	// New's record of the answer: nothing when accepted, Info for the
	// kernel's rejection, and for no answer Warn where the kernel's version
	// has the flags and Info below it.
	t.Run("log_levels", func(t *testing.T) {
		for _, tc := range []struct {
			p            asyncCancelProbe
			major, minor int
			want         string
		}{
			{asyncCancelAccepted, 6, 8, ""},
			{asyncCancelRejected, 5, 15, "INFO"},
			{asyncCancelRejected, 6, 8, "INFO"},
			{asyncCancelNoAnswer, 5, 18, "INFO"},
			{asyncCancelNoAnswer, 5, 19, "WARN"},
			{asyncCancelNoAnswer, 6, 8, "WARN"},
		} {
			var buf lockedBuffer
			logAsyncCancelProbe(slog.New(slog.NewJSONHandler(&buf, nil)), tc.p, "why", tc.major, tc.minor)
			recs := buf.records(t)
			got := ""
			if len(recs) == 1 {
				got, _ = recs[0]["level"].(string)
			}
			if len(recs) > 1 || got != tc.want {
				t.Errorf("%v on kernel %d.%d logged %v, want one record at %q (none when empty)",
					tc.p, tc.major, tc.minor, recs, tc.want)
			}
			if tc.p == asyncCancelNoAnswer && len(recs) == 1 {
				if msg, _ := recs[0]["msg"].(string); !strings.Contains(msg, "no answer") {
					t.Errorf("the no-answer record reads %q, want it to say the probe got no answer", msg)
				}
			}
		}
	})
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
// kernel and checks its answer against the kernel's version, and checks that
// the probe reads the kernel's answer: a cancel flag no kernel defines
// (bit 31) is rejected with -EINVAL everywhere, so the same path must then
// report the flags unsupported.
func TestAsyncCancelProbeOnThisKernel(t *testing.T) {
	r, err := NewRing(8, 0, 0)
	if err != nil {
		skipOrFail656(t, "io_uring unavailable: %v", err)
	}
	_ = r.Close()
	res, reason := probeAsyncCancelFlags()
	ok := res == asyncCancelAccepted
	want, rel := kernelHasCancelFlags(t)
	t.Logf("celeris681 async cancel flags probe: kernel=%s result=%v reason=%q", rel, res, reason)
	if ok != want {
		t.Fatalf("probeAsyncCancelFlags() = (%v, %q) on kernel %s, want accepted=%v", res, reason, rel, want)
	}
	if cres, _ := probeAsyncCancelFlagsCached(); cres != res {
		t.Fatalf("the cached probe says %v, the probe %v", cres, res)
	}
	if bad, why := probeAsyncCancel(1 << 31); bad != asyncCancelRejected || !strings.Contains(why, "EINVAL") {
		t.Fatalf("a cancel flag no kernel defines was read as (%v, %q), want rejected with EINVAL: "+
			"the probe does not read the kernel's answer", bad, why)
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
	if res, _ := probeAsyncCancelFlagsCached(); e.asyncCancelFlags != (res == asyncCancelAccepted) {
		t.Fatalf("New stored asyncCancelFlags=%v, the probe says %v", e.asyncCancelFlags, res)
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

	// A probe that fails before the kernel answers keeps the reap off too,
	// and New reports it apart from a rejection (celeris#681 R2).
	t.Run("no_answer_is_logged_apart_from_a_rejection", newWarnsWhenTheProbeGetsNoAnswer)
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
		res, _ := probeAsyncCancelFlagsCached()
		ok := res == asyncCancelAccepted
		want, rel := kernelHasCancelFlags(t)
		t.Logf("celeris681 kernel=%s probe=%v", rel, res)
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
