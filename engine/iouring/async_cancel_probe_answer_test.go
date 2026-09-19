//go:build linux

package iouring

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/resource"
)

// The cancel-flags probe's answer (celeris#681 round 4). Each check here is a
// function run as a subtest of one of the probe tests the CI witness step
// names, so it runs there with skipping forbidden. They use only the probe's
// API as it stood before round 4, plus the test seams round 4 adds, so each
// can also be run against the round-3 head.

// probeZeroValueIsNoAnswer (N3): an answer that was never set must keep the
// reap off. The zero value of asyncCancelProbe is therefore no answer; it
// used to be accepted, which is the one value that turns the reap on.
func probeZeroValueIsNoAnswer(t *testing.T) {
	var unset asyncCancelProbe
	if unset == asyncCancelAccepted {
		t.Fatalf("the zero value of asyncCancelProbe reads as %v: an answer that was never set would turn "+
			"the hand-off's reap on", unset)
	}
	if unset != asyncCancelNoAnswer || unset.String() != "no answer" {
		t.Errorf("the zero value of asyncCancelProbe is %v (%q), want %v: an unset answer reads as no answer",
			unset, unset.String(), asyncCancelNoAnswer)
	}
}

// probeUnrecognisedAnswerIsItsOwnClass (N2): a completion that is neither an
// acceptance (res >= 0, -ENOENT) nor the rejection every kernel before 5.19
// gives (-EINVAL) is an answer the probe does not recognise. It used to be
// read as a rejection and logged at Info, even on a 5.19+ kernel where the
// flags exist. It is a class of its own ("unexpected"): the reap stays off,
// the reason names the errno, and New logs it at Warn on every kernel.
// Written against String(), so it runs against the round-3 head too.
func probeUnrecognisedAnswerIsItsOwnClass(t *testing.T) {
	for _, res := range []int32{-int32(unix.EBADF), -int32(unix.ECANCELED), -int32(unix.EPERM)} {
		errno := unix.ErrnoName(unix.Errno(-res))
		p, reason := classifyAsyncCancelProbe(res)
		if p == asyncCancelAccepted {
			t.Fatalf("classifyAsyncCancelProbe(%d) = %v: an answer the probe does not recognise turned the reap on", res, p)
		}
		if p == asyncCancelRejected || p == asyncCancelNoAnswer || p.String() != "unexpected" {
			t.Errorf("classifyAsyncCancelProbe(%d) = %v (%q), want a class of its own, \"unexpected\": "+
				"an answer the probe does not recognise is neither the kernel's rejection nor no answer", res, p, reason)
		}
		if !strings.Contains(reason, errno) {
			t.Errorf("classifyAsyncCancelProbe(%d) reason %q does not name the errno %s", res, reason, errno)
		}
		for _, k := range [][2]int{{5, 15}, {6, 8}} {
			var buf lockedBuffer
			logAsyncCancelProbe(slog.New(slog.NewJSONHandler(&buf, nil)), p, reason, k[0], k[1])
			recs := buf.records(t)
			level, logged := "", ""
			if len(recs) == 1 {
				level, _ = recs[0]["level"].(string)
				logged, _ = recs[0]["reason"].(string)
			}
			if len(recs) != 1 || level != "WARN" || !strings.Contains(logged, errno) {
				t.Errorf("cqe.res=%d on kernel %d.%d was logged as %v, want one WARN record whose reason names %s",
					res, k[0], k[1], recs, errno)
			}
		}
	}
}

// probeOnlyAnAnswerIsCached (N1): the kernel's answer, accepted or rejected,
// is kept for the process. A probe that got no answer is not, so the next
// call, and the next New, probe again. It used to be cached like an answer:
// one transient failure, such as EMFILE or ENOMEM setting up the probe's ring
// while the adaptive engine builds its io_uring engine under load, kept the
// reap off for the life of the process.
func probeOnlyAnAnswerIsCached(t *testing.T) {
	saved := runAsyncCancelProbe
	var calls int
	var give asyncCancelProbe
	runAsyncCancelProbe = func() (asyncCancelProbe, string) {
		calls++
		return give, "celeris681 injected: " + give.String()
	}
	t.Cleanup(func() {
		runAsyncCancelProbe = saved
		resetAsyncCancelProbeCache() // the next New probes the kernel again
	})
	for _, tc := range []struct {
		p      asyncCancelProbe
		probes int // probes run by three calls
	}{
		{asyncCancelAccepted, 1},
		{asyncCancelRejected, 1},
		{asyncCancelNoAnswer, 3},
	} {
		resetAsyncCancelProbeCache()
		calls, give = 0, tc.p
		for i := 0; i < 3; i++ {
			if got, _ := probeAsyncCancelFlagsCached(); got != tc.p {
				t.Fatalf("call %d with the probe answering %v returned %v", i+1, tc.p, got)
			}
		}
		if calls != tc.probes {
			t.Errorf("three calls with the probe answering %v ran it %d time(s), want %d: only the kernel's answer "+
				"(accepted or rejected) is kept, a probe that got no answer is run again", tc.p, calls, tc.probes)
		}
	}

	// Through New: the probe gets no answer when the first engine is built
	// and is accepted when the second is.
	resetAsyncCancelProbeCache()
	calls, give = 0, asyncCancelNoAnswer
	cfg := resource.Config{
		Addr:     "127.0.0.1:0",
		Protocol: engine.HTTP1,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	first, err := New(cfg, transplantTestHandler{})
	if err != nil {
		skipOrFail656(t, "iouring engine unavailable: %v", err)
	}
	give = asyncCancelAccepted
	second, err := New(cfg, transplantTestHandler{})
	if err != nil {
		skipOrFail656(t, "iouring engine unavailable: %v", err)
	}
	if first.asyncCancelFlags || !second.asyncCancelFlags || calls != 2 {
		t.Errorf("the first New's probe got no answer, the second's is accepted: asyncCancelFlags %v then %v, "+
			"probes run %d; want false, true and 2: a probe with no answer kept the reap off for the next engine",
			first.asyncCancelFlags, second.asyncCancelFlags, calls)
	}
}

// probeCutShortWaitIsRetriedOnce (N1): the probe waits for its cancel's
// completion only when the completion is not there after the submit, and
// SubmitAndWaitTimeout returns nil both when that wait times out and when a
// signal cuts it short (EINTR). A wait that comes back early with nothing to
// read was cut short, and is repeated once for the rest of the time; a second
// early return, or a wait that ran its full time, is no answer. The probe
// used to give up after the first wait, so an EINTR read as no answer.
func probeCutShortWaitIsRetriedOnce(t *testing.T) {
	r, err := NewRing(8, 0, 0)
	if err != nil {
		skipOrFail656(t, "io_uring unavailable: %v", err)
	}
	_ = r.Close()
	want, why := probeAsyncCancel(cancelAll)
	if want != asyncCancelAccepted && want != asyncCancelRejected {
		t.Fatalf("with nothing held back the probe got %v (%q): there is no kernel answer to compare with", want, why)
	}
	savedSubmit, savedWait, savedTimeout := asyncCancelProbeSubmit, asyncCancelProbeWait, asyncCancelProbeTimeout
	t.Cleanup(func() {
		asyncCancelProbeSubmit, asyncCancelProbeWait, asyncCancelProbeTimeout = savedSubmit, savedWait, savedTimeout
	})
	// Hold the cancel back at the submit, so nothing has completed when the
	// probe first looks; the first real wait submits it.
	asyncCancelProbeSubmit = func(*Ring) (int, error) { return 0, nil }
	var waits int
	cutShort := func(cuts int) func(*Ring, time.Duration) error {
		return func(r *Ring, d time.Duration) error {
			waits++
			if waits <= cuts {
				return nil // what SubmitAndWaitTimeout returns for a wait a signal cut short (EINTR)
			}
			return r.SubmitAndWaitTimeout(d)
		}
	}
	for _, tc := range []struct {
		name  string
		cuts  int
		want  asyncCancelProbe
		waits int
	}{
		{"cut short once", 1, want, 2},
		{"cut short twice", 2, asyncCancelNoAnswer, 2},
	} {
		waits, asyncCancelProbeWait = 0, cutShort(tc.cuts)
		if got, reason := probeAsyncCancel(cancelAll); got != tc.want || waits != tc.waits {
			t.Errorf("wait %s: the probe gave (%v, %q) after %d wait(s), want %v after %d: a wait cut short is "+
				"repeated once, and only once", tc.name, got, reason, waits, tc.want, tc.waits)
		}
	}

	// A wait that ran its full time is not repeated.
	asyncCancelProbeTimeout = 50 * time.Millisecond
	waits = 0
	asyncCancelProbeWait = func(_ *Ring, d time.Duration) error {
		waits++
		time.Sleep(d)
		return nil
	}
	if got, reason := probeAsyncCancel(cancelAll); got != asyncCancelNoAnswer || waits != 1 {
		t.Errorf("a full-length wait with no completion: the probe gave (%v, %q) after %d wait(s), want %v after 1",
			got, reason, waits, asyncCancelNoAnswer)
	}
}
