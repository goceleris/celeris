//go:build linux

package iouring

import (
	"fmt"
	"strings"
	"testing"

	"github.com/goceleris/celeris/internal/engine"
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
