//go:build !validation

package ratelimit

// validateBucket is the production no-op stub. The empty body inlines
// to nothing — the allow/undo hot paths pay zero overhead in
// production builds. The assertion-bearing implementation lives in
// validation_check.go and is compiled under -tags=validation.
func validateBucket(_ float64, _ int) {}
