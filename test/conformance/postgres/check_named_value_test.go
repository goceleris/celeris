//go:build postgres

package postgres_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

type (
	cnvNamedString string
	cnvNamedInt    int32
	cnvValuer      struct{ s string }
)

func (v cnvValuer) Value() (driver.Value, error) { return v.s, nil }

// TestCheckNamedValueArgs binds the Go argument kinds database/sql normally
// converts for a driver (sql.Null*, driver.Valuer, defined types, pointers and
// the integer widths other than int/int32/int64) through database/sql against
// a real server and reads each one back as text. The conn implements
// driver.NamedValueChecker, so database/sql leaves the conversion to the
// driver: before the fix every cell here failed with "unsupported argument
// type".
func TestCheckNamedValueArgs(t *testing.T) {
	db := openDB(t)
	ts := time.Date(2025, 1, 15, 12, 34, 56, 789123000, time.UTC)
	str := "p"
	cases := []struct {
		name, typ string
		arg       any
		want      string
		null      bool
	}{
		{name: "int16", typ: "int2", arg: int16(7), want: "7"},
		{name: "int8", typ: "int2", arg: int8(-3), want: "-3"},
		{name: "uint8", typ: "int4", arg: uint8(3), want: "3"},
		{name: "uint16", typ: "int4", arg: uint16(65535), want: "65535"},
		{name: "uint32", typ: "int8", arg: uint32(4000000000), want: "4000000000"},
		{name: "uint64", typ: "int8", arg: uint64(1 << 62), want: "4611686018427387904"},
		{name: "defined_string", typ: "text", arg: cnvNamedString("named"), want: "named"},
		{name: "defined_int", typ: "int4", arg: cnvNamedInt(8), want: "8"},
		{name: "valuer", typ: "text", arg: cnvValuer{"v"}, want: "v"},
		{name: "json_RawMessage", typ: "json", arg: json.RawMessage(`{"a":1}`), want: `{"a":1}`},
		{name: "NullString", typ: "text", arg: sql.NullString{String: "x", Valid: true}, want: "x"},
		{name: "NullString_null", typ: "text", arg: sql.NullString{}, null: true},
		{name: "NullInt64", typ: "int8", arg: sql.NullInt64{Int64: 5, Valid: true}, want: "5"},
		{name: "NullInt64_null", typ: "int8", arg: sql.NullInt64{}, null: true},
		{name: "NullInt32", typ: "int4", arg: sql.NullInt32{Int32: 5, Valid: true}, want: "5"},
		{name: "NullBool", typ: "bool", arg: sql.NullBool{Bool: true, Valid: true}, want: "true"},
		{name: "NullFloat64", typ: "float8", arg: sql.NullFloat64{Float64: 1.5, Valid: true}, want: "1.5"},
		{name: "NullTime", typ: "timestamptz", arg: sql.NullTime{Time: ts, Valid: true}, want: "2025-01-15 12:34:56.789123+00"},
		{name: "NullTime_null", typ: "timestamptz", arg: sql.NullTime{}, null: true},
		{name: "Null_generic", typ: "int4", arg: sql.Null[int32]{V: 3, Valid: true}, want: "3"},
		{name: "pointer_string", typ: "text", arg: &str, want: "p"},
		{name: "nil_pointer_string", typ: "text", arg: (*string)(nil), null: true},
		// Unchanged: the kinds encodeOne handles itself still work.
		{name: "int", typ: "int4", arg: 5, want: "5"},
		{name: "string", typ: "text", arg: "s", want: "s"},
		{name: "nil", typ: "text", arg: nil, null: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var got sql.NullString
			q := fmt.Sprintf("SELECT ($1::%s)::text", c.typ)
			if err := db.QueryRowContext(ctx, q, c.arg).Scan(&got); err != nil {
				t.Fatalf("%s with %T(%v): %v", q, c.arg, c.arg, err)
			}
			if got.Valid == c.null || got.String != c.want {
				t.Fatalf("%T(%v) as %s: got %q (valid=%v), want %q (NULL=%v)", c.arg, c.arg, c.typ, got.String, got.Valid, c.want, c.null)
			}
		})
	}

	// A uint64 with the high bit set is not representable as int64; database/sql
	// rejects it and so must the driver, with an error rather than a wrapped value.
	t.Run("uint64_high_bit_rejected", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var got sql.NullString
		if err := db.QueryRowContext(ctx, "SELECT $1::text", uint64(1<<63)).Scan(&got); err == nil {
			t.Fatalf("uint64 with the high bit set was accepted (got %q)", got.String)
		}
	})
}
