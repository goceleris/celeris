package protocol

import (
	"math"
	"testing"
)

// TestParseIntMinInt64 verifies that parseInt correctly handles
// math.MinInt64 (-9223372036854775808). Bug 2: parseUint rejects the
// magnitude 9223372036854775808 (overflows int64), so parseInt must
// special-case the negative path for this exact value.
func TestParseIntMinInt64(t *testing.T) {
	r := NewReader()
	r.Feed([]byte(":-9223372036854775808\r\n"))
	v, err := r.Next()
	if err != nil {
		t.Fatalf("parse MinInt64: %v", err)
	}
	if v.Type != TyInt {
		t.Fatalf("type = %s, want int", v.Type)
	}
	if v.Int != math.MinInt64 {
		t.Fatalf("got %d, want %d", v.Int, int64(math.MinInt64))
	}
}

// TestParseIntMinInt64PlusOne ensures -9223372036854775807 still works.
func TestParseIntMinInt64PlusOne(t *testing.T) {
	r := NewReader()
	r.Feed([]byte(":-9223372036854775807\r\n"))
	v, err := r.Next()
	if err != nil {
		t.Fatalf("parse MinInt64+1: %v", err)
	}
	if v.Int != math.MinInt64+1 {
		t.Fatalf("got %d, want %d", v.Int, int64(math.MinInt64+1))
	}
}

// TestParseIntOverflowNeg ensures that negative values beyond MinInt64
// are still rejected.
func TestParseIntOverflowNeg(t *testing.T) {
	r := NewReader()
	r.Feed([]byte(":-9223372036854775809\r\n"))
	_, err := r.Next()
	if err == nil {
		t.Fatal("expected error for value below MinInt64")
	}
}
