//go:build postgres

package postgres_test

import (
	"testing"
)

// TestCOPYFromStdin is a placeholder.
//
// COPY FROM STDIN / COPY TO STDOUT require CopyData protocol messages that
// sit outside the database/sql surface and the v1.4.0 Pool API. The underlying
// protocol primitives (CopyInState, CopyOutState, WriteCopyData, ...) exist
// in driver/postgres/protocol and are covered by unit tests there — but there
// is no public driver method that lets an application stream a COPY.
//
// TODO(#131): once the driver exposes Pool.CopyFrom / Pool.CopyTo (tracked in a
// follow-up issue), fill this in with:
//   - text COPY FROM STDIN with a few thousand rows
//   - binary COPY FROM STDIN round-trip
//   - COPY TO STDOUT reading back what was just inserted
//   - COPY FAIL propagation.
func TestCOPYFromStdin(t *testing.T) {
	t.Skip("COPY FROM/TO not exposed on the public driver API yet (v1.4.0); tracked in TODO(#131)")
}
