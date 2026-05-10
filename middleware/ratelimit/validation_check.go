//go:build validation

package ratelimit

import "github.com/goceleris/celeris/validation"

// validateBucket asserts the token-bucket invariant
// 0 <= tokens <= burst after every allow/undo update.
//
// A negative count means the limiter handed out more tokens than it
// owned (concurrency bug in the shard locking). A count above burst
// means undo over-refilled (also a concurrency bug, or a wrong cap
// computation). Either case increments
// validation.RatelimitTokenViolations so the property-test harness
// flags the build.
func validateBucket(tokens float64, burst int) {
	if tokens < 0 || tokens > float64(burst) {
		validation.RatelimitTokenViolations.Add(1)
	}
}
