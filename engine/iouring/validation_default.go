//go:build linux && !validation

package iouring

// validateSQEWrite is the production no-op stub. The empty body
// inlines to nothing — GetSQE pays zero overhead in production
// builds. See validation_check.go for the assertion-bearing
// implementation compiled in under `-tags=validation`.
func validateSQEWrite(_ *Ring, _ uint32) {}
