//go:build validation

package session

import "github.com/goceleris/celeris/validation"

// ownerKey is the well-known map key that the property-test harness
// (and probatorium-driven fixtures) write into session data when they
// want validateAdmission to verify session→user binding. If the key
// is absent, the assertion site degrades to an ID-shape check only.
const ownerKey = "_owner"

// expectedOwnerKey is the parallel "ground truth" key the test
// fixture writes into the session right before exercising the
// middleware. A mismatch between data[ownerKey] and
// data[expectedOwnerKey] means the session was admitted on behalf of
// a different user than the one the harness expected — exactly the
// scenario probatorium's session-owner-binding property checks.
const expectedOwnerKey = "_expected_owner"

// validateAdmission asserts the invariants every admitted session
// must satisfy:
//
//  1. id is non-empty and well-formed (matches validSessionID)
//  2. if both _owner and _expected_owner are present, they are equal
//
// Either failure increments validation.SessionOwnerMismatches so the
// harness's session-owner-binding predicate observes the discrepancy
// over the unix socket.
func validateAdmission(s *Session) {
	if s == nil {
		return
	}
	if s.id == "" || !validSessionID(s.id) {
		validation.SessionOwnerMismatches.Add(1)
		return
	}
	if s.data == nil {
		return
	}
	expected, hasExp := s.data[expectedOwnerKey]
	if !hasExp {
		return
	}
	actual, hasOwner := s.data[ownerKey]
	if !hasOwner || actual != expected {
		validation.SessionOwnerMismatches.Add(1)
	}
}
