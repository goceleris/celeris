//go:build linux

package iouring

import "testing"

// TestFixedFilesRequireExplicitOptIn pins celeris#541.
//
// Fixed-file support is incomplete: an audit of every cs.fixedFile branch
// found eleven defects, none refuted, including the DEFAULT receive path —
// prepRecv has no fixed-file variant and never sets the flags byte, so every
// connection would arm a recv against a raw fd equal to its slot index.
//
// It was off only by accident: prepMultishotAcceptDirect set SOCK_CLOEXEC
// alongside a fixed file slot, io_accept_prep rejects that with -EINVAL, and
// the probe read the rejection as the kernel refusing ACCEPT_DIRECT. That SQE
// bug is now fixed, so the tier reports support — and this gate is what keeps
// the feature off. The load-bearing case is tier support WITHOUT the opt-in.
func TestFixedFilesRequireExplicitOptIn(t *testing.T) {
	for _, tc := range []struct {
		name         string
		tierSupports bool
		env          string
		want         bool
	}{
		{"unsupported, no opt-in", false, "", false},
		{"unsupported, opt-in ignored", false, "1", false},
		{"supported but NOT opted in", true, "", false},
		{"supported and opted in", true, "1", true},
		{"opt-in must be exactly 1", true, "true", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envFixedFiles, tc.env)
			if got := fixedFilesEnabled(tc.tierSupports); got != tc.want {
				t.Errorf("fixedFilesEnabled(tierSupports=%v) with %s=%q = %v, want %v",
					tc.tierSupports, envFixedFiles, tc.env, got, tc.want)
			}
		})
	}
}
