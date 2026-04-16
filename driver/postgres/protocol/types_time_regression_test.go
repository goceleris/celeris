package protocol

import (
	"testing"
	"time"
)

// TestTimestampPreEpochRoundTrip verifies that timestamps before the PG epoch
// (2000-01-01) with sub-microsecond remainders are encoded correctly. Bug 3:
// Go's integer division truncates toward zero for negative durations, so a
// pre-epoch timestamp with a non-zero sub-microsecond remainder would be off
// by one microsecond (truncated toward zero instead of floored).
func TestTimestampPreEpochRoundTrip(t *testing.T) {
	// 1999-12-31 23:59:59.999999500 UTC — 500ns past the last microsecond
	// before the PG epoch. Without the floor correction, the encoder
	// truncates toward zero and encodes as -1 microsecond instead of -2.
	preEpoch := time.Date(1999, 12, 31, 23, 59, 59, 999999500, time.UTC)
	c := LookupOID(OIDTimestamp)
	buf, err := c.EncodeBinary(nil, preEpoch)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := c.DecodeBinary(buf)
	if err != nil {
		t.Fatal(err)
	}
	got := decoded.(time.Time)
	// The wire format is microseconds, so we expect the value to be floored
	// to 1999-12-31 23:59:59.999999 (not .000000 or truncated wrong).
	want := time.Date(1999, 12, 31, 23, 59, 59, 999999000, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("pre-epoch timestamp: got %v, want %v", got, want)
	}
}

// TestTimestampPreEpochExactMicro verifies that a pre-epoch timestamp
// exactly on a microsecond boundary is not off by one.
func TestTimestampPreEpochExactMicro(t *testing.T) {
	// Exactly 1 microsecond before PG epoch — no remainder.
	preEpoch := time.Date(1999, 12, 31, 23, 59, 59, 999999000, time.UTC)
	c := LookupOID(OIDTimestamp)
	buf, err := c.EncodeBinary(nil, preEpoch)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := c.DecodeBinary(buf)
	if err != nil {
		t.Fatal(err)
	}
	got := decoded.(time.Time)
	if !got.Equal(preEpoch) {
		t.Fatalf("pre-epoch exact micro: got %v, want %v", got, preEpoch)
	}
}

// TestTimestampDeepPreEpoch checks a date well before the epoch (1970).
func TestTimestampDeepPreEpoch(t *testing.T) {
	deepPast := time.Date(1970, 1, 1, 0, 0, 0, 500, time.UTC) // 500ns
	c := LookupOID(OIDTimestamp)
	buf, err := c.EncodeBinary(nil, deepPast)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := c.DecodeBinary(buf)
	if err != nil {
		t.Fatal(err)
	}
	got := decoded.(time.Time)
	// 500ns remainder with negative duration should floor to the previous microsecond.
	want := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("deep pre-epoch: got %v, want %v", got, want)
	}
}
