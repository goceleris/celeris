//go:build validation

package session

import (
	"strings"
	"testing"

	"github.com/goceleris/celeris/validation"
)

// validID is a 64-char lowercase-hex placeholder that satisfies
// validSessionID. The exact bytes are irrelevant; the length and
// charset are what matter.
var validID = strings.Repeat("a", sessionIDHexLen)

// TestValidateAdmissionOwnerMismatchBumpsCounter exercises the
// owner-binding predicate: when the session carries _expected_owner
// but _owner doesn't match, the assertion bumps the counter.
func TestValidateAdmissionOwnerMismatchBumpsCounter(t *testing.T) {
	before := validation.SessionOwnerMismatches.Load()

	s := &Session{
		id: validID,
		data: map[string]any{
			expectedOwnerKey: "alice",
			ownerKey:         "bob",
		},
	}
	validateAdmission(s)

	after := validation.SessionOwnerMismatches.Load()
	if after < before+1 {
		t.Fatalf("SessionOwnerMismatches: got %d, want >=%d (before=%d)", after, before+1, before)
	}
}

// TestValidateAdmissionHealthySessionQuiet confirms a healthy session
// (id well-formed, _owner matches _expected_owner) does not bump.
func TestValidateAdmissionHealthySessionQuiet(t *testing.T) {
	before := validation.SessionOwnerMismatches.Load()

	s := &Session{
		id: validID,
		data: map[string]any{
			expectedOwnerKey: "alice",
			ownerKey:         "alice",
		},
	}
	validateAdmission(s)

	after := validation.SessionOwnerMismatches.Load()
	if after != before {
		t.Fatalf("SessionOwnerMismatches healthy: got %d, want %d (no bump)", after, before)
	}
}

// TestValidateAdmissionMalformedIDBumps confirms an ill-formed
// session id (wrong length) trips the assertion regardless of owner
// data.
func TestValidateAdmissionMalformedIDBumps(t *testing.T) {
	before := validation.SessionOwnerMismatches.Load()

	s := &Session{id: "too-short"}
	validateAdmission(s)

	after := validation.SessionOwnerMismatches.Load()
	if after < before+1 {
		t.Fatalf("SessionOwnerMismatches malformed-id: got %d, want >=%d (before=%d)", after, before+1, before)
	}
}

// TestValidateAdmissionMissingExpectedOwner confirms the assertion
// is a vacuous pass when the harness didn't write _expected_owner —
// only fixtures that explicitly opt in are subject to the check.
func TestValidateAdmissionMissingExpectedOwner(t *testing.T) {
	before := validation.SessionOwnerMismatches.Load()

	s := &Session{
		id: validID,
		data: map[string]any{
			ownerKey: "alice",
		},
	}
	validateAdmission(s)

	after := validation.SessionOwnerMismatches.Load()
	if after != before {
		t.Fatalf("SessionOwnerMismatches no-expected: got %d, want %d (no bump)", after, before)
	}
}
