//go:build linux

package iouring

import (
	"fmt"
	"strings"
	"testing"

	"github.com/goceleris/celeris/engine"
)

// TestRequireAsyncCancelFlags pins New's io_uring floor (celeris#682) for
// every probe answer on both sides of Linux 5.19. The kernel's own answer
// decides wherever it gave one: accepted builds the engine on any version (a
// vendor backport), rejected refuses it on any version. Without an answer
// (the probe's ring failed, or its completion was one it does not recognise)
// the version decides: before 5.19 the flags cannot be there and io_uring is
// refused; from 5.19 they are, and the engine is built with the hand-off's
// reap off, as before.
func TestRequireAsyncCancelFlags(t *testing.T) {
	for _, tc := range []struct {
		p            asyncCancelProbe
		major, minor int
		refuse       bool
	}{
		{asyncCancelAccepted, 5, 15, false},
		{asyncCancelAccepted, 5, 19, false},
		{asyncCancelAccepted, 6, 8, false},
		{asyncCancelRejected, 5, 10, true},
		{asyncCancelRejected, 5, 15, true},
		{asyncCancelRejected, 5, 18, true},
		{asyncCancelRejected, 6, 8, true},
		{asyncCancelNoAnswer, 5, 10, true},
		{asyncCancelNoAnswer, 5, 18, true},
		{asyncCancelNoAnswer, 5, 19, false},
		{asyncCancelNoAnswer, 6, 8, false},
		{asyncCancelUnexpected, 5, 15, true},
		{asyncCancelUnexpected, 5, 19, false},
		{asyncCancelUnexpected, 7, 0, false},
	} {
		version := fmt.Sprintf("%d.%d.0-test", tc.major, tc.minor)
		profile := engine.CapabilityProfile{KernelVersion: version, KernelMajor: tc.major, KernelMinor: tc.minor}
		err := requireAsyncCancelFlags(tc.p, "celeris682 reason", profile)
		if (err != nil) != tc.refuse {
			t.Errorf("probe %v on kernel %s: refused=%v (%v), want refused=%v", tc.p, version, err != nil, err, tc.refuse)
			continue
		}
		if err == nil {
			continue
		}
		msg := err.Error()
		for _, want := range []string{"io_uring not available on this system", "5.19", version, "celeris682 reason", "epoll"} {
			if !strings.Contains(msg, want) {
				t.Errorf("probe %v on kernel %s: error %q does not contain %q", tc.p, version, msg, want)
			}
		}
	}
}
