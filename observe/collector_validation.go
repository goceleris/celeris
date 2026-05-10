//go:build validation

package observe

import "github.com/goceleris/celeris/validation"

// validationFields is embedded into Snapshot under -tags=validation
// so external callers (probatorium's validator-checker) can read the
// validation counters off the same Snapshot value returned by
// Collector.Snapshot. In production builds the symmetric type in
// collector_default.go is an empty struct — the field name is then
// inaccessible, which is the explicit hardening contract.
type validationFields struct {
	// ValidationCounters carries the snapshot of all assertion
	// counters maintained under -tags=validation. JSON-serializable.
	ValidationCounters validation.Counters `json:"validation_counters"`
}

// fillValidation populates the validation half of a Snapshot from
// the live atomic counters. Called by Collector.Snapshot.
func (s *Snapshot) fillValidation() {
	s.ValidationCounters = validation.Snapshot()
}
