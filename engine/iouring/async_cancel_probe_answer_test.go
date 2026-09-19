//go:build linux

package iouring

import "testing"

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
