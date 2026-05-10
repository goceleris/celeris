//go:build !validation

package observe

// validationFields is an empty struct in production builds. Embedding
// an empty struct adds zero size to Snapshot, so callers reading the
// other fields pay no cost. Attempting to reference
// snapshot.ValidationCounters in a production build is a compile
// error — the explicit hardening contract from validation/doc.go.
type validationFields struct{}

// fillValidation is the production no-op. The validation-tagged
// override in collector_validation.go fills the embedded fields from
// the live atomic counters.
//
//go:inline
func (s *Snapshot) fillValidation() {}
