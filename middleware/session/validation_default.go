//go:build !validation

package session

// validateAdmission is the production no-op stub. The empty body
// inlines to nothing — the session admission hot path pays zero
// overhead in production builds. The assertion-bearing implementation
// lives in validation_check.go and is compiled under -tags=validation.
func validateAdmission(_ *Session) {}
