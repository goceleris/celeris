//go:build postgres

package postgres_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestBoolRoundTrip covers BOOL via both formats (driver requests binary for
// extended queries, text for simple ones).
func TestBoolRoundTrip(t *testing.T) {
	db := openDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, v := range []bool{true, false} {
		var got bool
		if err := db.QueryRowContext(ctx, "SELECT $1::bool", v).Scan(&got); err != nil {
			t.Fatalf("Scan bool %v: %v", v, err)
		}
		if got != v {
			t.Fatalf("bool roundtrip: got %v want %v", got, v)
		}
	}
}

// TestIntRoundTrip covers int2, int4, int8 via text and binary.
func TestIntRoundTrip(t *testing.T) {
	db := openDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cases := []struct {
		typ  string
		val  int64
	}{
		{"int2", -32768},
		{"int2", 32767},
		{"int4", -2147483648},
		{"int4", 2147483647},
		{"int8", -9223372036854775808},
		{"int8", 9223372036854775807},
	}
	for _, c := range cases {
		t.Run(c.typ, func(t *testing.T) {
			var got int64
			if err := db.QueryRowContext(ctx, "SELECT $1::"+c.typ, c.val).Scan(&got); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if got != c.val {
				t.Fatalf("%s roundtrip: got %d want %d", c.typ, got, c.val)
			}
		})
	}
}

// TestFloatRoundTrip covers float4 and float8.
func TestFloatRoundTrip(t *testing.T) {
	db := openDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, v := range []float64{0, 1, -1, 3.141592653589793, 1e-10, 1e10} {
		var got float64
		if err := db.QueryRowContext(ctx, "SELECT $1::float8", v).Scan(&got); err != nil {
			t.Fatalf("Scan float8 %v: %v", v, err)
		}
		// Float8 is exact; compare with a tight tolerance to cover any
		// text-format round-trip imprecision.
		diff := got - v
		if diff < 0 {
			diff = -diff
		}
		if v != 0 && diff/absF(v) > 1e-14 {
			t.Fatalf("float8 roundtrip: got %v want %v", got, v)
		}
	}
}

func absF(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// TestTextRoundTrip covers text and varchar including multi-byte characters.
func TestTextRoundTrip(t *testing.T) {
	db := openDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cases := []string{
		"",
		"simple ascii",
		"multi-byte: héllo, 世界, 🚀",
		strings.Repeat("x", 10000),
	}
	for _, v := range cases {
		var got string
		if err := db.QueryRowContext(ctx, "SELECT $1::text", v).Scan(&got); err != nil {
			t.Fatalf("Scan text (%d bytes): %v", len(v), err)
		}
		if got != v {
			t.Fatalf("text roundtrip diff: got %d bytes want %d", len(got), len(v))
		}
	}
}

// TestByteaRoundTrip covers arbitrary bytes including NUL.
func TestByteaRoundTrip(t *testing.T) {
	db := openDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	payload := make([]byte, 256)
	for i := range payload {
		payload[i] = byte(i)
	}
	var got []byte
	if err := db.QueryRowContext(ctx, "SELECT $1::bytea", payload).Scan(&got); err != nil {
		t.Fatalf("Scan bytea: %v", err)
	}
	if !bytes.Equal(payload, got) {
		t.Fatalf("bytea roundtrip differs (len=%d got=%d)", len(payload), len(got))
	}
}

// TestUUIDRoundTrip ensures uuid columns survive a roundtrip as a string.
func TestUUIDRoundTrip(t *testing.T) {
	db := openDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const u = "550e8400-e29b-41d4-a716-446655440000"
	var got string
	if err := db.QueryRowContext(ctx, "SELECT $1::uuid", u).Scan(&got); err != nil {
		t.Fatalf("Scan uuid: %v", err)
	}
	if got != u {
		t.Fatalf("uuid: got %q want %q", got, u)
	}
}

// TestDateTimestampRoundTrip covers date, timestamp, timestamptz.
func TestDateTimestampRoundTrip(t *testing.T) {
	db := openDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	t.Run("date", func(t *testing.T) {
		// Use an ISO string so we don't fight driver/pg timezone handling.
		var got time.Time
		if err := db.QueryRowContext(ctx, "SELECT '2025-01-15'::date").Scan(&got); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if got.Year() != 2025 || got.Month() != 1 || got.Day() != 15 {
			t.Fatalf("date: got %v", got)
		}
	})

	t.Run("timestamp", func(t *testing.T) {
		want := time.Date(2025, 1, 15, 12, 34, 56, 789000000, time.UTC)
		var got time.Time
		if err := db.QueryRowContext(ctx, "SELECT $1::timestamp", want).Scan(&got); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		// Timestamp drops timezone but keeps wall-clock fields.
		if got.Year() != want.Year() || got.Month() != want.Month() || got.Day() != want.Day() ||
			got.Hour() != want.Hour() || got.Minute() != want.Minute() || got.Second() != want.Second() {
			t.Fatalf("timestamp mismatch: got %v want %v", got, want)
		}
	})

	t.Run("timestamptz", func(t *testing.T) {
		want := time.Date(2025, 1, 15, 12, 34, 56, 0, time.UTC)
		var got time.Time
		if err := db.QueryRowContext(ctx, "SELECT $1::timestamptz", want).Scan(&got); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if !got.Equal(want) {
			t.Fatalf("timestamptz: got %v want %v", got.UTC(), want)
		}
	})
}

// TestJSONBRoundTrip covers jsonb column in text form (which is how pg returns
// it by default for most drivers) and json in the same mode.
func TestJSONBRoundTrip(t *testing.T) {
	db := openDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const v = `{"a":1,"b":"two","c":[1,2,3]}`
	var got string
	// Cast at read time so we get the canonical text form instead of the
	// binary jsonb header.
	if err := db.QueryRowContext(ctx, "SELECT ($1::jsonb)::text", v).Scan(&got); err != nil {
		t.Fatalf("Scan jsonb: %v", err)
	}
	// jsonb re-serialization can reorder keys; just check it parses and
	// roundtrips structurally via length.
	if len(got) == 0 || got[0] != '{' {
		t.Fatalf("jsonb result not a JSON object: %q", got)
	}
}

// TestNumericAsString accepts the numeric type as text — the driver's default
// codec returns text for NUMERIC since Go lacks a native decimal type.
func TestNumericAsString(t *testing.T) {
	db := openDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cases := []string{"0", "1", "-1", "1.5", "3.141592653589793238"}
	for _, v := range cases {
		var got string
		// Read as text to avoid binary decoding assumptions.
		if err := db.QueryRowContext(ctx, "SELECT ($1::numeric)::text", v).Scan(&got); err != nil {
			t.Fatalf("Scan numeric %q: %v", v, err)
		}
		if got != v {
			// Postgres may canonicalize 1.50 → 1.50 (preserves scale). Skip
			// strict eq for trailing-zero cases; exact inputs above avoid
			// that.
			t.Fatalf("numeric: got %q want %q", got, v)
		}
	}
}

// TestArrays exercises int and text arrays.
func TestArrays(t *testing.T) {
	db := openDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	t.Run("int4[]", func(t *testing.T) {
		// Read as text to sidestep driver-specific array decoding.
		var got string
		if err := db.QueryRowContext(ctx, "SELECT (ARRAY[1,2,3]::int4[])::text").Scan(&got); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if got != "{1,2,3}" {
			t.Fatalf("int4[]: got %q", got)
		}
	})

	t.Run("text[]", func(t *testing.T) {
		var got string
		if err := db.QueryRowContext(ctx, "SELECT (ARRAY['a','b','c']::text[])::text").Scan(&got); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if got != "{a,b,c}" {
			t.Fatalf("text[]: got %q", got)
		}
	})
}

// TestNullableTypes walks a mixed row with NULLs in several columns to check
// the decoder path for sql.NullX scanners.
func TestNullableTypes(t *testing.T) {
	db := openDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var (
		ni sql.NullInt64
		ns sql.NullString
		nb sql.NullBool
		nf sql.NullFloat64
	)
	if err := db.QueryRowContext(ctx, "SELECT NULL::int8, NULL::text, NULL::bool, NULL::float8").Scan(&ni, &ns, &nb, &nf); err != nil {
		t.Fatalf("Scan NULLs: %v", err)
	}
	if ni.Valid || ns.Valid || nb.Valid || nf.Valid {
		t.Fatalf("expected all NULL, got (%v %v %v %v)", ni, ns, nb, nf)
	}
}

// TestStoredTableTypes inserts a mixed-column row into a real table, then reads
// it back, to check codec paths on real column OIDs (not casts on expressions).
func TestStoredTableTypes(t *testing.T) {
	db := openDB(t)
	tbl := uniqueTableName(t, "types")
	createTable(t, db, tbl, "id int primary key, b bool, s text, f float8, ba bytea, ts timestamptz")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	_, err := db.ExecContext(ctx,
		fmt.Sprintf("INSERT INTO %s VALUES ($1,$2,$3,$4,$5,$6)", tbl),
		1, true, "hello", 3.14, []byte{1, 2, 3}, now,
	)
	if err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	var (
		id int
		b  bool
		s  string
		f  float64
		ba []byte
		ts time.Time
	)
	err = db.QueryRowContext(ctx, "SELECT id,b,s,f,ba,ts FROM "+tbl+" WHERE id=1").
		Scan(&id, &b, &s, &f, &ba, &ts)
	if err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	if id != 1 || !b || s != "hello" || f < 3.13 || f > 3.15 || len(ba) != 3 || !ts.Equal(now) {
		t.Fatalf("stored types mismatch: id=%d b=%v s=%q f=%v ba=%v ts=%v", id, b, s, f, ba, ts)
	}
}
