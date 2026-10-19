//go:build linux

package iouring

import "testing"

// TestMultishotRecvRequiresExplicitOptIn pins the gate the worker's
// provided-buffer ring and the engine's start-up line share (checklist #884
// item 48): the ring is mapped only where the tier supports multishot recv
// AND CELERIS_IOURING_MULTISHOT_RECV=1. The load-bearing case is tier support
// WITHOUT the opt-in, the default, where the line used to print the
// capability. Cloned from TestFixedFilesRequireExplicitOptIn.
func TestMultishotRecvRequiresExplicitOptIn(t *testing.T) {
	for _, tc := range []struct {
		name         string
		tierSupports bool
		env          string
		want         bool
	}{
		{"unsupported, no opt-in", false, "", false},
		{"unsupported, opt-in ignored", false, "1", false},
		{"supported but NOT opted in", true, "", false},
		{"supported, 0", true, "0", false},
		{"supported and opted in", true, "1", true},
		{"opt-in must be exactly 1", true, "true", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envMultishotRecv, tc.env)
			if got := multishotRecvEnabled(tc.tierSupports); got != tc.want {
				t.Errorf("multishotRecvEnabled(tierSupports=%v) with %s=%q = %v, want %v",
					tc.tierSupports, envMultishotRecv, tc.env, got, tc.want)
			}
		})
	}
}
