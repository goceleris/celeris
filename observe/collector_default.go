//go:build !validation

package observe

// validationFields is an empty struct in production builds. Embedding
// an empty struct adds zero size to Snapshot, so callers reading the
// other fields pay no cost. Attempting to reference
// snapshot.ValidationCounters in a production build is a compile
// error — the explicit hardening contract from validation/doc.go.
//
// The unused-linter does not track embedding as a use, so suppress its
// false positive: this type is referenced from collector.go's
// embedded field, which lives on Snapshot.
//
//nolint:unused // embedded into Snapshot in collector.go for build-tag symmetry
type validationFields struct{}

// fillValidation is the production no-op. The empty body inlines to
// nothing. The validation-tagged override in collector_validation.go
// fills the embedded fields from the live atomic counters.
func (s *Snapshot) fillValidation() {}
