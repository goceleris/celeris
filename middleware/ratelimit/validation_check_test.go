//go:build validation

package ratelimit

import (
	"testing"

	"github.com/goceleris/celeris/validation"
)

// TestValidateBucketNegativeBumps exercises the lower-bound breach:
// a negative token count means the limiter handed out more tokens
// than it owned, which must trip the assertion.
func TestValidateBucketNegativeBumps(t *testing.T) {
	before := validation.RatelimitTokenViolations.Load()
	validateBucket(-1, 10)
	after := validation.RatelimitTokenViolations.Load()
	if after < before+1 {
		t.Fatalf("RatelimitTokenViolations negative: got %d, want >=%d (before=%d)", after, before+1, before)
	}
}

// TestValidateBucketAboveBurstBumps exercises the upper-bound breach:
// an undo over-refill above burst must trip the assertion.
func TestValidateBucketAboveBurstBumps(t *testing.T) {
	before := validation.RatelimitTokenViolations.Load()
	validateBucket(11, 10)
	after := validation.RatelimitTokenViolations.Load()
	if after < before+1 {
		t.Fatalf("RatelimitTokenViolations above-burst: got %d, want >=%d (before=%d)", after, before+1, before)
	}
}

// TestValidateBucketWithinRangeQuiet covers the healthy interior and
// boundaries [0, burst] — none of these may bump the counter.
func TestValidateBucketWithinRangeQuiet(t *testing.T) {
	cases := []struct {
		name   string
		tokens float64
		burst  int
	}{
		{"interior", 5, 10},
		{"empty-bucket", 0, 10},
		{"full-bucket", 10, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := validation.RatelimitTokenViolations.Load()
			validateBucket(tc.tokens, tc.burst)
			after := validation.RatelimitTokenViolations.Load()
			if after != before {
				t.Fatalf("RatelimitTokenViolations %s: got %d, want %d (no bump)", tc.name, after, before)
			}
		})
	}
}
