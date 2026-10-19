//go:build linux

package iouring

import (
	"fmt"
	"testing"

	"github.com/goceleris/celeris/internal/engine"
)

// TestNoTierEnablesSQPoll884 pins the premise of two guards (celeris#884 item
// 18): hijackConn submits its cancels before it hands the socket over only
// when !w.sqpoll, and endOwedOpsAtShutdown (and queueListenAcceptCancel) is
// skipped under SQPOLL. Both are unreachable because no tier sets SQPOLL (every
// SQPollIdle is 0, celeris#377). A tier that does must first make hijackConn
// wait for its cancel's CQE and give the shutdown drain an SQPOLL form; the
// failure says so.
func TestNoTierEnablesSQPoll884(t *testing.T) {
	full := engine.CapabilityProfile{
		SQPoll: true, CoopTaskrun: true, DeferTaskrun: true, FixedFiles: true, SendZC: true,
		MultishotAccept: true, MultishotRecv: true,
	}
	base, high, optional := full, full, full
	base.IOUringTier = engine.Base
	high.IOUringTier, high.ProvidedBuffers = engine.High, true
	optional.IOUringTier, optional.ProvidedBuffers = engine.Optional, false // SQPoll is what selects it
	for _, tc := range []struct {
		name string
		p    engine.CapabilityProfile
		want string
	}{
		{"base", base, "*iouring.baseTier"},
		{"high", high, "*iouring.highTier"},
		{"optional", optional, "*iouring.optionalTier"},
	} {
		ts := SelectTier(tc.p, 0)
		if ts == nil {
			t.Fatalf("%s: no tier selected", tc.name)
		}
		if got := fmt.Sprintf("%T", ts); got != tc.want {
			t.Fatalf("%s: SelectTier built %s, want %s: the row no longer covers that tier", tc.name, got, tc.want)
		}
		if n := ts.SQPollIdle(); n != 0 {
			t.Errorf("%s: SQPollIdle() = %d: SQPOLL is on, and hijackConn and endOwedOpsAtShutdown have no SQPOLL form (celeris#884 item 18)", tc.name, n)
		}
		if ts.SetupFlags()&setupSQPoll != 0 {
			t.Errorf("%s: SetupFlags has IORING_SETUP_SQPOLL (celeris#884 item 18)", tc.name)
		}
	}
}
