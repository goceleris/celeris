//go:build unix

package redis

import (
	"testing"
	"time"
)

// On unix the stored latency is the measured duration, exactly as before the
// windows fallback: a zero stays 0 ("not measured"), nothing is clamped.
func TestMeasuredNanosIsTheMeasuredDurationOnUnix(t *testing.T) {
	for _, d := range []time.Duration{0, 1, 50 * time.Millisecond} {
		if got := measuredNanos(d); got != d.Nanoseconds() {
			t.Errorf("measuredNanos(%v) = %d, want %d", d, got, d.Nanoseconds())
		}
	}
}
