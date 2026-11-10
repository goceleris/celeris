//go:build !celeris_monoskew

package mono

import "testing"

// The default build carries no skew code at all.
func TestNoSkewInANormalBuild(t *testing.T) {
	if skew != 0 {
		t.Fatalf("skew = %d in a build without -tags celeris_monoskew", skew)
	}
}
