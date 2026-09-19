//go:build linux

package iouring

import (
	"context"
	"errors"
	"fmt"
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
// completion is the kernel's answer: accepted, rejected, or (celeris#681 N2)
// one the probe does not recognise, which is a class of its own. A probe that
// fails before reading one has no answer, which is not a rejection
// (celeris#681 R2).
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
		{-int32(unix.EBADF), asyncCancelUnexpected, "cqe.res=-9 (EBADF)"},
		{-int32(unix.ECANCELED), asyncCancelUnexpected, "cqe.res=-125 (ECANCELED)"},
	} {
		got, reason := classifyAsyncCancelProbe(tc.res)
		if got != tc.want || (tc.reason == "") != (reason == "") || !strings.Contains(reason, tc.reason) {
			t.Errorf("classifyAsyncCancelProbe(%d) = (%v, %q), want (%v, containing %q)",
				tc.res, got, reason, tc.want, tc.reason)
		}
	}

	// An answer never set keeps the reap off (celeris#681 N3).
	t.Run("zero_value_is_no_answer", probeZeroValueIsNoAnswer)

	// An answer the probe does not recognise is not a rejection, and New
	// warns about it with the errno (celeris#681 N2).
	t.Run("unrecognised_answer_is_its_own_class", probeUnrecognisedAnswerIsItsOwnClass)

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
	// kernel's rejection, Warn on every kernel for an answer the probe does
	// not recognise, and for no answer Warn where the kernel's version has
	// the flags and Info below it.
	t.Run("log_levels", func(t *testing.T) {
		for _, tc := range []struct {
			p            asyncCancelProbe
			major, minor int
			want         string
		}{
			{asyncCancelAccepted, 6, 8, ""},
			{asyncCancelRejected, 5, 15, "INFO"},
			{asyncCancelRejected, 6, 8, "INFO"},
			{asyncCancelUnexpected, 5, 15, "WARN"},
			{asyncCancelUnexpected, 6, 8, "WARN"},
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
			if tc.p == asyncCancelUnexpected && len(recs) == 1 {
				if msg, _ := recs[0]["msg"].(string); !strings.Contains(msg, "does not recognise") {
					t.Errorf("the unexpected-answer record reads %q, want it to say the probe does not recognise the answer", msg)
				}
			}
		}
	})
}

// kernelRelease describes the running kernel for a log line: its release and
// whether that version is one that has IORING_ASYNC_CANCEL flags (5.19 or
// later). It is never a verdict (celeris#681 R3): a vendor kernel can claim a
// version its feature surface does not match, which is why the engine probes
// for the flags at all, so the tests compare the probe with what the running
// kernel does, not with its version.
func kernelRelease() string {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return "uname: " + err.Error()
	}
	rel := unix.ByteSliceToString(u.Release[:])
	kv, err := probe.ParseKernelVersion(rel)
	if err != nil {
		return fmt.Sprintf("%s (unparsed: %v)", rel, err)
	}
	return fmt.Sprintf("%s (a version with the flags: %v)", rel, kv.AtLeast(5, 19))
}

// TestAsyncCancelProbeOnThisKernel runs the probe itself against the running
// kernel, logs its answer beside the kernel's version, and checks that the
// probe reads the kernel's answer: a cancel flag no kernel defines (bit 31)
// is rejected with -EINVAL everywhere, so the same path must then report a
// rejection. Whether the answer is right for this kernel is
// TestReapOnTheRunningKernel/probe_matches_the_kernel's check.
func TestAsyncCancelProbeOnThisKernel(t *testing.T) {
	r, err := NewRing(8, 0, 0)
	if err != nil {
		skipOrFail656(t, "io_uring unavailable: %v", err)
	}
	_ = r.Close()
	res, reason := probeAsyncCancelFlags()
	t.Logf("celeris681 async cancel flags probe: kernel=%s result=%v reason=%q", kernelRelease(), res, reason)
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
		t.Logf("celeris681 kernel=%s probe=%v", kernelRelease(), res)
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

	// The probe's answer checked against the running kernel itself rather
	// than its version (celeris#681 R3): with the flags forced on, the reap
	// goes to the kernel, and what the kernel does with it must be what the
	// probe said. A kernel that accepts the flags cancels the recv, and the
	// conn is handed off at its -ECANCELED; one that rejects them fails the
	// reap (TransplantReapFailed) and leaves the recv armed, and the conn
	// then leaves after its next, held, response.
	t.Run("probe_matches_the_kernel", func(t *testing.T) {
		res, reason := probeAsyncCancelFlagsCached()
		f := idleArmed(t, true)
		if n := f.w.ring.Pending(); n != 1 {
			t.Fatalf("tryTransplant placed %d SQE(s) with the flags forced on, want the reap", n)
		}
		run(t, f, "the kernel's answer to the reap", func() bool {
			return f.tgt.adopted.Load() == 1 || f.e.metrics.handoffLoss.reapFailed.Load() == 1
		})
		accepts := f.tgt.adopted.Load() == 1
		t.Logf("celeris681 kernel=%s executed the reap: %v; the probe says %v (%q)", kernelRelease(), accepts, res, reason)
		if accepts != (res == asyncCancelAccepted) {
			t.Fatalf("the running kernel executed the reap: %v, but the probe says %v (%q): the probe's answer "+
				"is not what this kernel does", accepts, res, reason)
		}
		if !accepts {
			if _, err := unix.Write(f.peer, []byte(fdlGET)); err != nil {
				t.Fatalf("client write: %v", err)
			}
			run(t, f, "the next request", func() bool { return f.tgt.adopted.Load() == 1 })
		}
		if n := f.e.metrics.handoffLoss.handoffInFlight.Load(); n != 0 {
			t.Fatalf("TransplantHandoffInFlight = %d, want 0", n)
		}
	})
}
