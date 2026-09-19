//go:build linux

package iouring

import (
	"log/slog"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
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
