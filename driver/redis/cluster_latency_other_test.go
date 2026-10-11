//go:build !unix

package redis

import (
	"testing"
	"time"
)

// Without unix the clock may be as coarse as the system tick, so a round trip
// it cannot resolve is stored as measured.
func TestMeasuredNanosKeepsAnUnresolvedRoundTripOnWindows(t *testing.T) {
	if got := measuredNanos(0); got != 1 {
		t.Errorf("measuredNanos(0) = %d, want 1", got)
	}
	if got := measuredNanos(50 * time.Millisecond); got != int64(50*time.Millisecond) {
		t.Errorf("measuredNanos(50ms) = %d", got)
	}
}
