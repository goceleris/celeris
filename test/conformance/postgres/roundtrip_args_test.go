//go:build postgres

package postgres_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"
)

// rtNamedString and rtNamedInt are defined types, rtValuer a driver.Valuer:
// what an application passes when it uses its own types or sql.Null*.
type (
	rtNamedString string
	rtNamedInt    int32
	rtValuer      struct{ s string }
)

func (v rtValuer) Value() (driver.Value, error) { return v.s, nil }

type rtArgCase struct {
	name, typ string
	arg       any
	want      string // text read back; "" with null = NULL
	null      bool
	bug       string
	// qualifier names the part of the column type's range the case covers when
	// the same Go value works for the rest of it ("years before 1"); the support
	// table lists the column type with it.
	qualifier string
}

// rtArgCases is the list TestRoundTripArgs runs and TestSupportTable reads.
func rtArgCases() []rtArgCase {
	ts := time.Date(2025, 1, 15, 12, 34, 56, 789123000, time.UTC)
	tsPlus := time.Date(2025, 1, 15, 12, 34, 56, 0, time.FixedZone("p0530", 5*3600+1800))
	const (
		bValuer  = "B6 database/sql hands driver.Valuer, sql.Null*, defined types and the integer widths other than int/int32/int64 to the driver unconverted (CheckNamedValue returns nil) and encodeOne rejects them"
		bRawMsg  = "B7 json.RawMessage is a defined type that encodeOne does not unwrap"
		bTime    = "B9 time.Time is sent as RFC 3339 text, which `time` does not parse"
		bYear    = "B9 time.Time before year 1 is sent as RFC 3339 text with a negative year, which timestamptz rejects"
		bArrays  = "B8 slices ([]string, []int64, []any) are not accepted as array parameters"
		bBytes16 = "B11 [16]byte is not accepted as a uuid parameter"
	)
	return []rtArgCase{
		{name: "bool_true", typ: "bool", arg: true, want: "true"},
		{name: "bool_false", typ: "bool", arg: false, want: "false"},
		{name: "int_to_int2", typ: "int2", arg: 12, want: "12"},
		{name: "int_to_int4", typ: "int4", arg: 5, want: "5"},
		{name: "int32_to_int4", typ: "int4", arg: int32(-9), want: "-9"},
		{name: "int64_to_int8", typ: "int8", arg: int64(math.MinInt64), want: "-9223372036854775808"},
		{name: "int_to_numeric", typ: "numeric", arg: int64(5), want: "5"},
		{name: "float64_to_numeric", typ: "numeric", arg: 12.5, want: "12.5"},
		{name: "float64_big_to_numeric", typ: "numeric", arg: 1e21, want: "1000000000000000000000"},
		{name: "string_to_numeric", typ: "numeric", arg: "-123456789.123456789", want: "-123456789.123456789"},
		{name: "float32_to_float4", typ: "float4", arg: float32(1.5), want: "1.5"},
		{name: "float64_to_float8", typ: "float8", arg: math.Pi, want: "3.141592653589793"},
		{name: "float64_nan", typ: "float8", arg: math.NaN(), want: "NaN"},
		{name: "float64_inf", typ: "float8", arg: math.Inf(1), want: "Infinity"},
		{name: "string_to_text", typ: "text", arg: "héllo 世界", want: "héllo 世界"},
		{name: "bytes_to_text", typ: "text", arg: []byte("bytes-as-text"), want: "bytes-as-text"},
		{name: "string_to_char", typ: "char(4)", arg: "ab", want: "ab"},
		{name: "bytes_to_bytea", typ: "bytea", arg: []byte{0, 1, 255}, want: `\x0001ff`},
		{name: "empty_bytes_to_bytea", typ: "bytea", arg: []byte{}, want: `\x`},
		// jsonb parameters are not covered yet: see the note in roundtrip_types_test.go.
		{name: "string_to_json", typ: "json", arg: `{"a":1}`, want: `{"a":1}`},
		{name: "bytes_to_json", typ: "json", arg: []byte(`{"a":1}`), want: `{"a":1}`},
		{name: "string_to_uuid", typ: "uuid", arg: "550e8400-e29b-41d4-a716-446655440000", want: "550e8400-e29b-41d4-a716-446655440000"},
		{name: "bytes_to_uuid", typ: "uuid", arg: []byte{0x55, 0x0e, 0x84, 0, 0xe2, 0x9b, 0x41, 0xd4, 0xa7, 0x16, 0x44, 0x66, 0x55, 0x44, 0, 0}, want: "550e8400-e29b-41d4-a716-446655440000"},
		{name: "time_to_date", typ: "date", arg: ts, want: "2025-01-15"},
		{name: "time_to_timestamp", typ: "timestamp", arg: ts, want: "2025-01-15 12:34:56.789123"},
		{name: "time_to_timestamptz", typ: "timestamptz", arg: ts, want: "2025-01-15 12:34:56.789123+00"},
		{name: "time_offset_to_timestamptz", typ: "timestamptz", arg: tsPlus, want: "2025-01-15 07:04:56+00"},
		{name: "time_year1_to_timestamptz", typ: "timestamptz", arg: time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC), want: "0001-01-01 00:00:00+00"},
		{name: "time_year9999_to_timestamptz", typ: "timestamptz", arg: time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC), want: "9999-12-31 23:59:59+00"},
		{name: "string_to_time", typ: "time", arg: "12:34:56.789", want: "12:34:56.789"},
		{name: "string_to_interval", typ: "interval", arg: "1 year 2 mons 3 days 04:05:06.789", want: "1 year 2 mons 3 days 04:05:06.789"},
		{name: "string_to_int4_array", typ: "int4[]", arg: "{1,2,3}", want: "{1,2,3}"},
		{name: "string_to_text_array", typ: "text[]", arg: `{a,"b c"}`, want: `{a,"b c"}`},
		{name: "string_to_inet", typ: "inet", arg: "192.168.1.5/24", want: "192.168.1.5/24"},
		{name: "string_to_cidr", typ: "cidr", arg: "10.0.0.0/8", want: "10.0.0.0/8"},

		// NULL of each common type
		{name: "nil_bool", typ: "bool", null: true},
		{name: "nil_int2", typ: "int2", null: true},
		{name: "nil_int4", typ: "int4", null: true},
		{name: "nil_int8", typ: "int8", null: true},
		{name: "nil_float4", typ: "float4", null: true},
		{name: "nil_float8", typ: "float8", null: true},
		{name: "nil_numeric", typ: "numeric", null: true},
		{name: "nil_text", typ: "text", null: true},
		{name: "nil_bytea", typ: "bytea", null: true},
		{name: "nil_json", typ: "json", null: true},
		{name: "nil_uuid", typ: "uuid", null: true},
		{name: "nil_date", typ: "date", null: true},
		{name: "nil_time", typ: "time", null: true},
		{name: "nil_timestamp", typ: "timestamp", null: true},
		{name: "nil_timestamptz", typ: "timestamptz", null: true},
		{name: "nil_interval", typ: "interval", null: true},
		{name: "nil_int4_array", typ: "int4[]", null: true},
		{name: "nil_text_array", typ: "text[]", null: true},
		{name: "nil_inet", typ: "inet", null: true},
		{name: "nil_cidr", typ: "cidr", null: true},

		// Parameter kinds that fail today.
		{name: "int16", typ: "int2", arg: int16(7), want: "7", bug: bValuer},
		{name: "uint8", typ: "int4", arg: uint8(3), want: "3", bug: bValuer},
		{name: "uint32", typ: "int8", arg: uint32(4000000000), want: "4000000000", bug: bValuer},
		{name: "uint64", typ: "int8", arg: uint64(1 << 62), want: "4611686018427387904", bug: bValuer},
		{name: "defined_string", typ: "text", arg: rtNamedString("named"), want: "named", bug: bValuer},
		{name: "defined_int", typ: "int4", arg: rtNamedInt(8), want: "8", bug: bValuer},
		{name: "valuer", typ: "text", arg: rtValuer{"v"}, want: "v", bug: bValuer},
		{name: "sql_NullString", typ: "text", arg: sql.NullString{String: "x", Valid: true}, want: "x", bug: bValuer},
		{name: "sql_NullString_null", typ: "text", arg: sql.NullString{}, null: true, bug: bValuer},
		{name: "sql_NullInt64", typ: "int8", arg: sql.NullInt64{Int64: 5, Valid: true}, want: "5", bug: bValuer},
		{name: "sql_NullInt64_null", typ: "int8", arg: sql.NullInt64{}, null: true, bug: bValuer},
		{name: "sql_NullBool", typ: "bool", arg: sql.NullBool{Bool: true, Valid: true}, want: "true", bug: bValuer},
		{name: "sql_NullFloat64", typ: "float8", arg: sql.NullFloat64{Float64: 1.5, Valid: true}, want: "1.5", bug: bValuer},
		{name: "sql_NullTime", typ: "timestamptz", arg: sql.NullTime{Time: ts, Valid: true}, want: "2025-01-15 12:34:56.789123+00", bug: bValuer},
		{name: "sql_Null_int32", typ: "int4", arg: sql.Null[int32]{V: 3, Valid: true}, want: "3", bug: bValuer},
		{name: "pointer_string", typ: "text", arg: func() *string { s := "p"; return &s }(), want: "p", bug: bValuer},
		{name: "nil_pointer_string", typ: "text", arg: (*string)(nil), null: true, bug: bValuer},
		{name: "bytes16_to_uuid", typ: "uuid", arg: [16]byte{1}, want: "01000000-0000-0000-0000-000000000000", bug: bBytes16},
		{name: "rawmessage_to_json", typ: "json", arg: json.RawMessage(`{"a":1}`), want: `{"a":1}`, bug: bRawMsg},
		{name: "time_to_time", typ: "time", arg: ts, want: "12:34:56.789123", bug: bTime},
		{name: "time_year_minus43", typ: "timestamptz", arg: time.Date(-43, 3, 15, 0, 0, 0, 0, time.UTC), want: "0044-03-15 00:00:00+00 BC", bug: bYear, qualifier: "years before 1"},
		{name: "strings_to_text_array", typ: "text[]", arg: []string{"a", "b"}, want: "{a,b}", bug: bArrays},
		{name: "int64s_to_int8_array", typ: "int8[]", arg: []int64{1, 2}, want: "{1,2}", bug: bArrays},
	}
}

// TestRoundTripArgs binds one Go value of each kind an application passes
// (plain scalars, []byte, time.Time, nil, and the sql.Null* / driver.Valuer /
// defined types database/sql normally unwraps) as a parameter of each column
// type, and reads it back as text: `SELECT ($1::type)::text`. Parameters go
// through the extended protocol in text or binary format as the driver
// chooses per Go type (encodeOne), so this is the write half of the matrix in
// roundtrip_types_test.go.
//
// Cells that fail today carry a bug id and run only with -tags pgrtbugs.
func TestRoundTripArgs(t *testing.T) {
	variants := rtOpenVariants(t)
	cases := rtArgCases()

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			q := fmt.Sprintf("SELECT ($1::%s)::text", c.typ)
			for _, v := range variants {
				if v.proto == "simple" {
					continue // a parameter needs the extended protocol
				}
				if c.bug != "" && !rtKnownBugs {
					continue
				}
				t.Run(v.api+"/"+v.proto, func(t *testing.T) {
					if c.bug != "" {
						t.Logf("known bug: %s", c.bug)
					}
					var got sql.NullString
					if err := v.query(ctx, q, c.arg).Scan(&got); err != nil {
						t.Fatalf("%s with %T(%v): %v", q, c.arg, c.arg, err)
					}
					if got.Valid == c.null || got.String != c.want {
						t.Fatalf("%T(%v) as %s: got %q (valid=%v), want %q (NULL=%v)", c.arg, c.arg, c.typ, got.String, got.Valid, c.want, c.null)
					}
				})
			}
		})
	}
}
