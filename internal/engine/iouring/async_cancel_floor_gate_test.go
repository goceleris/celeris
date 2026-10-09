//go:build linux

package iouring

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/probe"
	"github.com/goceleris/celeris/internal/resource"
)

// TestRequireAsyncCancelFlags pins New's io_uring floor (celeris#682, #872)
// for every probe answer on both sides of Linux 5.19. The floor is a version:
// before 5.19 io_uring is refused whatever the probe answered, because the
// probe tests one of the four cancel forms the engine builds (user_data with
// CANCEL_ALL, no skip-success) and a kernel that carries only some of the
// flags passes it while the others fail with -EINVAL on every close
// (celeris#872). From 5.19 the kernel's own answer decides: rejected refuses
// the engine on any version, and accepted, no answer, or an answer the probe
// does not recognise builds it (the last two with the hand-off's reap off, as
// before).
func TestRequireAsyncCancelFlags(t *testing.T) {
	for _, tc := range []struct {
		p            asyncCancelProbe
		major, minor int
		refuse       bool
	}{
		// A kernel before 5.19 whose probe answers accepted: a partial vendor
		// backport. The probe sends only the user_data+CANCEL_ALL form; the
		// close path's skip-success form and the by-fd forms can still fail
		// with -EINVAL there (celeris#872). Refused on every version below the
		// floor, 5.10 (the lowest the profile reports) through 5.18.
		{asyncCancelAccepted, 5, 10, true},
		{asyncCancelAccepted, 5, 15, true},
		{asyncCancelAccepted, 5, 18, true},
		{asyncCancelAccepted, 5, 19, false},
		{asyncCancelAccepted, 6, 8, false},
		{asyncCancelAccepted, 7, 0, false},
		{asyncCancelRejected, 5, 10, true},
		{asyncCancelRejected, 5, 15, true},
		{asyncCancelRejected, 5, 18, true},
		{asyncCancelRejected, 5, 19, true},
		{asyncCancelRejected, 6, 8, true},
		{asyncCancelNoAnswer, 5, 10, true},
		{asyncCancelNoAnswer, 5, 18, true},
		{asyncCancelNoAnswer, 5, 19, false},
		{asyncCancelNoAnswer, 6, 8, false},
		{asyncCancelUnexpected, 5, 15, true},
		{asyncCancelUnexpected, 5, 18, true},
		{asyncCancelUnexpected, 5, 19, false},
		{asyncCancelUnexpected, 7, 0, false},
	} {
		version := fmt.Sprintf("%d.%d.0-test", tc.major, tc.minor)
		t.Run(fmt.Sprintf("%s/%d.%d", strings.ReplaceAll(tc.p.String(), " ", "_"), tc.major, tc.minor), func(t *testing.T) {
			profile := engine.CapabilityProfile{KernelVersion: version, KernelMajor: tc.major, KernelMinor: tc.minor}
			err := requireAsyncCancelFlags(tc.p, "celeris682 reason", profile)
			if (err != nil) != tc.refuse {
				t.Fatalf("probe %v on kernel %s: refused=%v (%v), want refused=%v", tc.p, version, err != nil, err, tc.refuse)
			}
			if err == nil {
				return
			}
			msg := err.Error()
			for _, want := range []string{"io_uring not available on this system", "5.19", version, "celeris682 reason", "epoll"} {
				if !strings.Contains(msg, want) {
					t.Errorf("probe %v on kernel %s: error %q does not contain %q", tc.p, version, msg, want)
				}
			}
		})
	}
}

// TestNewFloorHoldsWhenTheProbeAnswersAccepted872 runs the floor through New
// on the running kernel (celeris#872). The probe is given the answer a
// partial vendor backport gives (accepted: the one form it sends works) on
// whatever kernel this runs on. Before 5.19 New must refuse io_uring with the
// kernel requirement in its error; from 5.19 it must build the engine. On a
// kernel before 5.19 this is the defect itself: the answer decided, and an
// engine was built whose other cancel forms can fail on every close.
func TestNewFloorHoldsWhenTheProbeAnswersAccepted872(t *testing.T) {
	r, err := NewRing(8, 0, 0)
	if err != nil {
		skipOrFail656(t, "io_uring unavailable: %v", err)
	}
	_ = r.Close()
	profile := probe.Probe()
	if !profile.IOUringTier.Available() {
		skipOrFail656(t, "the profile reports no io_uring tier on kernel %s", profile.KernelVersion)
	}
	fromFloor := profile.KernelMajor > 5 || (profile.KernelMajor == 5 && profile.KernelMinor >= 19)
	saved := runAsyncCancelProbe
	runAsyncCancelProbe = func() (asyncCancelProbe, string) { return asyncCancelAccepted, "" }
	resetAsyncCancelProbeCache()
	t.Cleanup(func() {
		runAsyncCancelProbe = saved
		resetAsyncCancelProbeCache() // the next New probes the kernel again
	})
	e, err := New(resource.Config{
		Addr:     "127.0.0.1:0",
		Protocol: engine.HTTP1,
		Logger:   slog.New(slog.DiscardHandler),
	}, transplantTestHandler{})
	t.Logf("celeris872 kernel=%s (from 5.19: %v) probe=accepted: engine built=%v err=%v", profile.KernelVersion, fromFloor, e != nil, err)
	switch {
	case fromFloor && err != nil:
		t.Fatalf("New refused io_uring on kernel %s, which has the floor, for a probe that answered accepted: %v", profile.KernelVersion, err)
	case !fromFloor && err == nil:
		t.Fatalf("New built an io_uring engine on kernel %s, before 5.19, because the probe answered accepted: "+
			"a kernel that carries only some of the IORING_ASYNC_CANCEL flags answers so while the close path's "+
			"other cancel forms fail with -EINVAL (celeris#872)", profile.KernelVersion)
	case !fromFloor && (!strings.Contains(err.Error(), "5.19") || !strings.HasPrefix(err.Error(), "io_uring not available on this system")):
		t.Errorf("New's error %q: want it to say io_uring is not available and name Linux 5.19", err)
	}
}

// TestRequireAsyncCancelFlagsErrorReads872 pins what an explicit
// Engine: IOUring user sees on a kernel before 5.19 (celeris#872): the
// version leads, an accepted answer carries no empty "()" for its blank
// reason, and only that answer gets the partial-backport explanation (it is
// false for a probe that got no answer).
func TestRequireAsyncCancelFlagsErrorReads872(t *testing.T) {
	profile := engine.CapabilityProfile{KernelVersion: "5.15.0-test", KernelMajor: 5, KernelMinor: 15}
	accepted := requireAsyncCancelFlags(asyncCancelAccepted, "", profile)
	if accepted == nil {
		t.Fatal("accepted on 5.15 was not refused")
	}
	if msg := accepted.Error(); strings.Contains(msg, "()") || !strings.Contains(msg, "probe result: accepted") ||
		!strings.Contains(msg, "only some of the flags") || !strings.Contains(msg, "5.15.0-test predates it") {
		t.Errorf("accepted on 5.15: error %q", msg)
	}
	noAnswer := requireAsyncCancelFlags(asyncCancelNoAnswer, "NewRing failed: EMFILE", profile)
	if noAnswer == nil {
		t.Fatal("no answer on 5.15 was not refused")
	}
	if msg := noAnswer.Error(); strings.Contains(msg, "only some of the flags") || !strings.Contains(msg, "probe result: no answer (NewRing failed: EMFILE)") {
		t.Errorf("no answer on 5.15: error %q", msg)
	}
}
