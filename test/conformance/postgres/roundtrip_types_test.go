//go:build postgres

package postgres_test

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/postgres"
)

// This file is the round-trip matrix for the common PostgreSQL column types:
// every type is written three ways (a SQL literal over the simple protocol,
// a bound parameter through database/sql, a bound parameter through the
// native Pool) and its NULL three ways, then read back through
//
//	API       database/sql and the native Pool (they decode differently:
//	          Pool.Rows.Scan has a raw-bytes fast path database/sql never takes)
//	protocol  simple  (no arguments, text-format result columns)
//	          ext     (arguments, auto-cached prepared statement, binary
//	                   result columns)
//	          nocache (arguments, auto_cache_statements=false: the unnamed
//	                   statement path)
//	          prep    (database/sql only: db.Prepare, the explicit statement)
//	dest      any (the driver's own value), the natural Go type, string,
//	          []byte, and a sql.Null* scanner for the NULL rows.
//
// Expectations are literals: the text form the server prints for each value
// (postgres:16, `SELECT <literal>::type`), cross-checked against pgx/v5 (native
// and its database/sql adapter) and lib/pq. The cross-check is kept alive by
// test/drivercmp/postgres/roundtrip_ref_test.go, a separate module so the
// reference drivers never reach consumers of celeris.
//
// Cells that are wrong today are classified in rtKnownBug and run only with
// `-tags 'postgres pgrtbugs'`; the default build leaves them out (no skip).

// rtWant checks a value read back as `any`.
type rtWant func(got any) error

func rtWantBool(want bool) rtWant {
	return func(got any) error {
		if b, ok := got.(bool); !ok || b != want {
			return fmt.Errorf("got %T(%v), want bool(%v)", got, got, want)
		}
		return nil
	}
}

func rtWantInt(want int64) rtWant {
	return func(got any) error {
		if n, ok := got.(int64); !ok || n != want {
			return fmt.Errorf("got %T(%v), want int64(%d)", got, got, want)
		}
		return nil
	}
}

// rtWantFloat compares a float. bits is 32 for float4 (the two protocols widen it
// differently: text parses the shortest decimal, binary widens the float32),
// so compare after rounding to float32; 64 for float8 (exact).
func rtWantFloat(want float64, bits int) rtWant {
	return func(got any) error {
		f, ok := got.(float64)
		if !ok {
			return fmt.Errorf("got %T(%v), want float64(%v)", got, got, want)
		}
		switch {
		case math.IsNaN(want):
			if !math.IsNaN(f) {
				return fmt.Errorf("got %v, want NaN", f)
			}
		case bits == 32:
			if float32(f) != float32(want) {
				return fmt.Errorf("got %v, want float32 %v", f, want)
			}
		default:
			if f != want {
				return fmt.Errorf("got %v, want %v", f, want)
			}
		}
		return nil
	}
}

// rtWantText accepts the PostgreSQL text form as a string or as []byte: the
// driver returns numeric, uuid and friends as string and the types it has no
// codec for as []byte, and both carry the same text.
func rtWantText(want string) rtWant {
	return func(got any) error {
		switch v := got.(type) {
		case string:
			if v != want {
				return fmt.Errorf("got string %q, want %q", v, want)
			}
			return nil
		case []byte:
			if string(v) != want {
				return fmt.Errorf("got []byte %q, want %q", v, want)
			}
			return nil
		}
		return fmt.Errorf("got %T(%v), want text %q", got, got, want)
	}
}

func rtWantBytes(want []byte) rtWant {
	return func(got any) error {
		b, ok := got.([]byte)
		if !ok || !bytes.Equal(b, want) {
			return fmt.Errorf("got %T(%q), want []byte %q", got, got, want)
		}
		return nil
	}
}

func rtWantTime(want time.Time) rtWant {
	return func(got any) error {
		if tm, ok := got.(time.Time); !ok || !tm.Equal(want) {
			return fmt.Errorf("got %T(%v), want time %v", got, got, want)
		}
		return nil
	}
}

// rtWantArr accepts either form the driver produces for an array column: the
// decoded []driver.Value (binary result columns) or the PostgreSQL text
// literal (text result columns).
func rtWantArr(vals []driver.Value, text string) rtWant {
	return func(got any) error {
		switch v := got.(type) {
		case []driver.Value:
			if !reflect.DeepEqual(v, vals) {
				return fmt.Errorf("got %#v, want %#v", v, vals)
			}
			return nil
		case []byte:
			if string(v) != text {
				return fmt.Errorf("got text %q, want %q", v, text)
			}
			return nil
		case string:
			if v != text {
				return fmt.Errorf("got text %q, want %q", v, text)
			}
			return nil
		}
		return fmt.Errorf("got %T(%v), want array %q", got, got, text)
	}
}

// rtNative is a typed destination: a fresh pointer plus a getter that turns
// what was scanned into the value rtWant expects.
type rtNative func() (dest any, get func() any)

func rtNatBool() (any, func() any)    { v := new(bool); return v, func() any { return *v } }
func rtNatInt64() (any, func() any)   { v := new(int64); return v, func() any { return *v } }
func rtNatInt() (any, func() any)     { v := new(int); return v, func() any { return int64(*v) } }
func rtNatInt32() (any, func() any)   { v := new(int32); return v, func() any { return int64(*v) } }
func rtNatInt16() (any, func() any)   { v := new(int16); return v, func() any { return int64(*v) } }
func rtNatFloat64() (any, func() any) { v := new(float64); return v, func() any { return *v } }
func rtNatFloat32() (any, func() any) { v := new(float32); return v, func() any { return float64(*v) } }
func rtNatTime() (any, func() any)    { v := new(time.Time); return v, func() any { return *v } }

// rtNull builds the sql.Null* destination a NULL row is scanned into.
type rtNull func() (dest any, valid func() bool)

func rtNullString() (any, func() bool) {
	v := new(sql.NullString)
	return v, func() bool { return v.Valid }
}
func rtNullInt64() (any, func() bool) {
	v := new(sql.NullInt64)
	return v, func() bool { return v.Valid }
}
func rtNullFloat64() (any, func() bool) {
	v := new(sql.NullFloat64)
	return v, func() bool { return v.Valid }
}
func rtNullBool() (any, func() bool) {
	v := new(sql.NullBool)
	return v, func() bool { return v.Valid }
}
func rtNullTime() (any, func() bool) {
	v := new(sql.NullTime)
	return v, func() bool { return v.Valid }
}

type rtCase struct {
	name string
	typ  string // column type
	kind string // classification for rtKnownBug
	lit  string // SQL expression written over the simple protocol
	arg  any    // Go value bound as a parameter for the extended-protocol writes
	want rtWant
	text string // PostgreSQL text form: the expectation for string and []byte dests ("" = none)
	raw  []byte // bytea: the expectation for the []byte dest
	nat  []rtNative
	null rtNull
}

var (
	rtTS       = time.Date(2025, 1, 15, 12, 34, 56, 789123000, time.UTC)
	rtTSPlus   = time.Date(2025, 1, 15, 12, 34, 56, 0, time.FixedZone("p0530", 5*3600+1800))
	rtAllBytes = func() []byte {
		b := make([]byte, 256)
		for i := range b {
			b[i] = byte(i)
		}
		return b
	}()
	rtJSON = `{"a": [1, 2, {"b": null}], "c": "d"}`
)

func rtHex(b []byte) string { return fmt.Sprintf("\\x%x", b) }

func rtCases() []rtCase {
	big := strings.Repeat("0123456789abcdef", 6400) // 100 KiB: several reads, past any 8 KiB buffer
	bigBytes := bytes.Repeat(rtAllBytes, 400)       // 100 KiB
	i64 := func(n int64) string { return strconv.FormatInt(n, 10) }
	return []rtCase{
		// bool
		{name: "bool_true", typ: "bool", kind: "bool", lit: "true", arg: true, want: rtWantBool(true), nat: []rtNative{rtNatBool}, null: rtNullBool},
		{name: "bool_false", typ: "bool", kind: "bool", lit: "false", arg: false, want: rtWantBool(false), nat: []rtNative{rtNatBool}, null: rtNullBool},
		// integers
		{name: "int2_min", typ: "int2", kind: "int", lit: "(-32768)", arg: int64(-32768), want: rtWantInt(-32768), text: "-32768", nat: []rtNative{rtNatInt16, rtNatInt64, rtNatInt}, null: rtNullInt64},
		{name: "int2_max", typ: "int2", kind: "int", lit: "32767", arg: int64(32767), want: rtWantInt(32767), text: "32767", nat: []rtNative{rtNatInt16, rtNatInt64}, null: rtNullInt64},
		{name: "int4_min", typ: "int4", kind: "int", lit: "(-2147483648)", arg: int64(-2147483648), want: rtWantInt(-2147483648), text: "-2147483648", nat: []rtNative{rtNatInt32, rtNatInt64, rtNatInt}, null: rtNullInt64},
		{name: "int4_max", typ: "int4", kind: "int", lit: "2147483647", arg: int64(2147483647), want: rtWantInt(2147483647), text: "2147483647", nat: []rtNative{rtNatInt32, rtNatInt64}, null: rtNullInt64},
		{name: "int8_min", typ: "int8", kind: "int", lit: "(-9223372036854775808)", arg: int64(math.MinInt64), want: rtWantInt(math.MinInt64), text: i64(math.MinInt64), nat: []rtNative{rtNatInt64, rtNatInt}, null: rtNullInt64},
		{name: "int8_max", typ: "int8", kind: "int", lit: "9223372036854775807", arg: int64(math.MaxInt64), want: rtWantInt(math.MaxInt64), text: i64(math.MaxInt64), nat: []rtNative{rtNatInt64}, null: rtNullInt64},
		{name: "int8_zero", typ: "int8", kind: "int", lit: "0", arg: int64(0), want: rtWantInt(0), text: "0", nat: []rtNative{rtNatInt64}, null: rtNullInt64},
		// floats
		{name: "float4_1_5", typ: "float4", kind: "float", lit: "1.5", arg: float64(1.5), want: rtWantFloat(1.5, 32), nat: []rtNative{rtNatFloat32, rtNatFloat64}, null: rtNullFloat64},
		{name: "float4_0_1", typ: "float4", kind: "float", lit: "0.1", arg: float32(0.1), want: rtWantFloat(0.1, 32), nat: []rtNative{rtNatFloat32}, null: rtNullFloat64},
		{name: "float4_max", typ: "float4", kind: "float", lit: "3.4028235e38", arg: float64(math.MaxFloat32), want: rtWantFloat(math.MaxFloat32, 32), nat: []rtNative{rtNatFloat32}, null: rtNullFloat64},
		{name: "float8_pi", typ: "float8", kind: "float", lit: "3.141592653589793", arg: math.Pi, want: rtWantFloat(math.Pi, 64), nat: []rtNative{rtNatFloat64}, null: rtNullFloat64},
		{name: "float8_1e300", typ: "float8", kind: "float", lit: "1e300", arg: 1e300, want: rtWantFloat(1e300, 64), nat: []rtNative{rtNatFloat64}, null: rtNullFloat64},
		{name: "float8_small", typ: "float8", kind: "float", lit: "5e-324", arg: 5e-324, want: rtWantFloat(5e-324, 64), nat: []rtNative{rtNatFloat64}, null: rtNullFloat64},
		{name: "float8_nan", typ: "float8", kind: "float", lit: "'NaN'", arg: math.NaN(), want: rtWantFloat(math.NaN(), 64), nat: []rtNative{rtNatFloat64}, null: rtNullFloat64},
		{name: "float8_inf", typ: "float8", kind: "float", lit: "'Infinity'", arg: math.Inf(1), want: rtWantFloat(math.Inf(1), 64), nat: []rtNative{rtNatFloat64}, null: rtNullFloat64},
		{name: "float8_neginf", typ: "float8", kind: "float", lit: "'-Infinity'", arg: math.Inf(-1), want: rtWantFloat(math.Inf(-1), 64), nat: []rtNative{rtNatFloat64}, null: rtNullFloat64},
		// numeric: the driver's contract is the decimal text as a string
		{name: "numeric_zero", typ: "numeric", kind: "numeric", lit: "0", arg: "0", want: rtWantText("0"), text: "0", null: rtNullString},
		{name: "numeric_scale", typ: "numeric", kind: "numeric", lit: "12.50", arg: "12.50", want: rtWantText("12.50"), text: "12.50", null: rtNullString},
		{name: "numeric_neg", typ: "numeric", kind: "numeric", lit: "(-123456789.123456789)", arg: "-123456789.123456789", want: rtWantText("-123456789.123456789"), text: "-123456789.123456789", null: rtNullString},
		{name: "numeric_1e20", typ: "numeric", kind: "numeric", lit: "100000000000000000000", arg: "100000000000000000000", want: rtWantText("100000000000000000000"), text: "100000000000000000000", null: rtNullString},
		{name: "numeric_tiny", typ: "numeric", kind: "numeric", lit: "0.00001234", arg: "0.00001234", want: rtWantText("0.00001234"), text: "0.00001234", null: rtNullString},
		{name: "numeric_30digits", typ: "numeric", kind: "numeric", lit: "123456789012345678901234567890.123456789", arg: "123456789012345678901234567890.123456789", want: rtWantText("123456789012345678901234567890.123456789"), text: "123456789012345678901234567890.123456789", null: rtNullString},
		{name: "numeric_nan", typ: "numeric", kind: "numeric", lit: "'NaN'", arg: "NaN", want: rtWantText("NaN"), text: "NaN", null: rtNullString},
		// character types
		{name: "text_unicode", typ: "text", kind: "text", lit: "'héllo 世界 🚀'", arg: "héllo 世界 🚀", want: rtWantText("héllo 世界 🚀"), text: "héllo 世界 🚀", null: rtNullString},
		{name: "text_empty", typ: "text", kind: "text", lit: "''", arg: "", want: rtWantText(""), text: "", null: rtNullString},
		{name: "text_quotes", typ: "text", kind: "text", lit: "'it''s a \"test\" \\ {x},'", arg: `it's a "test" \ {x},`, want: rtWantText(`it's a "test" \ {x},`), text: `it's a "test" \ {x},`, null: rtNullString},
		{name: "text_100KiB", typ: "text", kind: "text", lit: "'" + big + "'", arg: big, want: rtWantText(big), text: big, null: rtNullString},
		{name: "varchar", typ: "varchar(8)", kind: "text", lit: "'varchar'", arg: "varchar", want: rtWantText("varchar"), text: "varchar", null: rtNullString},
		{name: "char_padded", typ: "char(4)", kind: "text", lit: "'ab'", arg: "ab", want: rtWantText("ab  "), text: "ab  ", null: rtNullString},
		{name: "name", typ: "name", kind: "text", lit: "'nm'", arg: "nm", want: rtWantText("nm"), text: "nm", null: rtNullString},
		// bytea
		{name: "bytea_all_bytes", typ: "bytea", kind: "bytea", lit: "'" + rtHex(rtAllBytes) + "'", arg: rtAllBytes, want: rtWantBytes(rtAllBytes), raw: rtAllBytes, null: rtNullString},
		{name: "bytea_empty", typ: "bytea", kind: "bytea", lit: "'\\x'", arg: []byte{}, want: rtWantBytes([]byte{}), raw: []byte{}, null: rtNullString},
		{name: "bytea_100KiB", typ: "bytea", kind: "bytea", lit: "'" + rtHex(bigBytes) + "'", arg: bigBytes, want: rtWantBytes(bigBytes), raw: bigBytes, null: rtNullString},
		// json. jsonb (and jsonb[]) is deliberately not here yet: its version-byte
		// fix is in flight (branch fix/postgres-jsonb-version-byte), and its
		// round-trip cases are added after that fix lands.
		{name: "json_object", typ: "json", kind: "json", lit: "'" + rtJSON + "'", arg: rtJSON, want: rtWantText(rtJSON), text: rtJSON, null: rtNullString},
		{name: "json_unicode_escape", typ: "json", kind: "json", lit: `'{"k": "é 世界"}'`, arg: `{"k": "é 世界"}`, want: rtWantText(`{"k": "é 世界"}`), text: `{"k": "é 世界"}`, null: rtNullString},
		// uuid
		{name: "uuid", typ: "uuid", kind: "uuid", lit: "'550e8400-e29b-41d4-a716-446655440000'", arg: "550e8400-e29b-41d4-a716-446655440000", want: rtWantText("550e8400-e29b-41d4-a716-446655440000"), text: "550e8400-e29b-41d4-a716-446655440000", null: rtNullString},
		{name: "uuid_nil", typ: "uuid", kind: "uuid", lit: "'00000000-0000-0000-0000-000000000000'", arg: "00000000-0000-0000-0000-000000000000", want: rtWantText("00000000-0000-0000-0000-000000000000"), text: "00000000-0000-0000-0000-000000000000", null: rtNullString},
		// date / time / timestamp
		{name: "date", typ: "date", kind: "ts", lit: "'2025-01-15'", arg: time.Date(2025, 1, 15, 0, 0, 0, 0, time.UTC), want: rtWantTime(time.Date(2025, 1, 15, 0, 0, 0, 0, time.UTC)), nat: []rtNative{rtNatTime}, null: rtNullTime},
		{name: "date_epoch", typ: "date", kind: "ts", lit: "'1970-01-01'", arg: time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC), want: rtWantTime(time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)), nat: []rtNative{rtNatTime}, null: rtNullTime},
		{name: "date_y2k_leap", typ: "date", kind: "ts", lit: "'2000-02-29'", arg: time.Date(2000, 2, 29, 0, 0, 0, 0, time.UTC), want: rtWantTime(time.Date(2000, 2, 29, 0, 0, 0, 0, time.UTC)), nat: []rtNative{rtNatTime}, null: rtNullTime},
		{name: "date_bc", typ: "date", kind: "datebc", lit: "'0044-03-15 BC'", want: rtWantTime(time.Date(-43, 3, 15, 0, 0, 0, 0, time.UTC)), nat: []rtNative{rtNatTime}, null: rtNullTime},
		{name: "time", typ: "time", kind: "nocodec", lit: "'12:34:56.789'", arg: "12:34:56.789", want: rtWantText("12:34:56.789"), text: "12:34:56.789", null: rtNullString},
		{name: "timetz", typ: "timetz", kind: "nocodec", lit: "'12:34:56.789+05:30'", arg: "12:34:56.789+05:30", want: rtWantText("12:34:56.789+05:30"), text: "12:34:56.789+05:30", null: rtNullString},
		{name: "timestamp", typ: "timestamp", kind: "ts", lit: "'2025-01-15 12:34:56.789123'", arg: rtTS, want: rtWantTime(rtTS), nat: []rtNative{rtNatTime}, null: rtNullTime},
		{name: "timestamp_pre1970", typ: "timestamp", kind: "ts", lit: "'1969-12-31 23:59:59.999999'", arg: time.Date(1969, 12, 31, 23, 59, 59, 999999000, time.UTC), want: rtWantTime(time.Date(1969, 12, 31, 23, 59, 59, 999999000, time.UTC)), nat: []rtNative{rtNatTime}, null: rtNullTime},
		{name: "timestamptz", typ: "timestamptz", kind: "ts", lit: "'2025-01-15 12:34:56.789123+00'", arg: rtTS, want: rtWantTime(rtTS), nat: []rtNative{rtNatTime}, null: rtNullTime},
		{name: "timestamptz_offset", typ: "timestamptz", kind: "ts", lit: "'2025-01-15 12:34:56+05:30'", arg: rtTSPlus, want: rtWantTime(rtTSPlus), nat: []rtNative{rtNatTime}, null: rtNullTime},
		{name: "timestamptz_far", typ: "timestamptz", kind: "ts", lit: "'2262-04-12 00:00:00+00'", arg: time.Date(2262, 4, 12, 0, 0, 0, 0, time.UTC), want: rtWantTime(time.Date(2262, 4, 12, 0, 0, 0, 0, time.UTC)), nat: []rtNative{rtNatTime}, null: rtNullTime},
		{name: "timestamp_year1500", typ: "timestamp", kind: "tsfar", lit: "'1500-01-01 00:00:00'", arg: time.Date(1500, 1, 1, 0, 0, 0, 0, time.UTC), want: rtWantTime(time.Date(1500, 1, 1, 0, 0, 0, 0, time.UTC)), nat: []rtNative{rtNatTime}, null: rtNullTime},
		{name: "timestamptz_year3000", typ: "timestamptz", kind: "tsfar", lit: "'3000-01-01 00:00:00+00'", arg: time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC), want: rtWantTime(time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)), nat: []rtNative{rtNatTime}, null: rtNullTime},
		{name: "interval", typ: "interval", kind: "nocodec", lit: "'1 year 2 mons 3 days 04:05:06.789'", arg: "1 year 2 mons 3 days 04:05:06.789", want: rtWantText("1 year 2 mons 3 days 04:05:06.789"), text: "1 year 2 mons 3 days 04:05:06.789", null: rtNullString},
		{name: "interval_negative", typ: "interval", kind: "nocodec", lit: "'-1 day'", arg: "-1 days", want: rtWantText("-1 days"), text: "-1 days", null: rtNullString},
		// arrays
		{name: "int2_array", typ: "int2[]", kind: "array", lit: "'{-32768,32767}'", arg: "{-32768,32767}", want: rtWantArr([]driver.Value{int64(-32768), int64(32767)}, "{-32768,32767}"), text: "{-32768,32767}", null: rtNullString},
		{name: "int4_array", typ: "int4[]", kind: "array", lit: "'{1,2,3}'", arg: "{1,2,3}", want: rtWantArr([]driver.Value{int64(1), int64(2), int64(3)}, "{1,2,3}"), text: "{1,2,3}", null: rtNullString},
		{name: "int4_array_null_elem", typ: "int4[]", kind: "array", lit: "'{1,NULL,3}'", arg: "{1,NULL,3}", want: rtWantArr([]driver.Value{int64(1), nil, int64(3)}, "{1,NULL,3}"), text: "{1,NULL,3}", null: rtNullString},
		{name: "int4_array_empty", typ: "int4[]", kind: "array", lit: "'{}'", arg: "{}", want: rtWantArr([]driver.Value{}, "{}"), text: "{}", null: rtNullString},
		{name: "int8_array", typ: "int8[]", kind: "array", lit: "'{-9223372036854775808,9223372036854775807}'", arg: "{-9223372036854775808,9223372036854775807}", want: rtWantArr([]driver.Value{int64(math.MinInt64), int64(math.MaxInt64)}, "{-9223372036854775808,9223372036854775807}"), text: "{-9223372036854775808,9223372036854775807}", null: rtNullString},
		{name: "text_array", typ: "text[]", kind: "array", lit: `'{a,"b c",NULL,"d\"q","e,f","",NULL}'`, arg: `{a,"b c",NULL,"d\"q","e,f","",NULL}`, want: rtWantArr([]driver.Value{"a", "b c", nil, `d"q`, "e,f", "", nil}, `{a,"b c",NULL,"d\"q","e,f","",NULL}`), text: `{a,"b c",NULL,"d\"q","e,f","",NULL}`, null: rtNullString},
		{name: "varchar_array", typ: "varchar[]", kind: "array", lit: "'{x,y}'", arg: "{x,y}", want: rtWantArr([]driver.Value{"x", "y"}, "{x,y}"), text: "{x,y}", null: rtNullString},
		{name: "float8_array", typ: "float8[]", kind: "array", lit: "'{1.5,-2.25}'", arg: "{1.5,-2.25}", want: rtWantArr([]driver.Value{1.5, -2.25}, "{1.5,-2.25}"), text: "{1.5,-2.25}", null: rtNullString},
		{name: "bool_array", typ: "bool[]", kind: "array", lit: "'{t,f}'", arg: "{t,f}", want: rtWantArr([]driver.Value{true, false}, "{t,f}"), text: "{t,f}", null: rtNullString},
		{name: "uuid_array", typ: "uuid[]", kind: "array", lit: "'{550e8400-e29b-41d4-a716-446655440000}'", arg: "{550e8400-e29b-41d4-a716-446655440000}", want: rtWantArr([]driver.Value{"550e8400-e29b-41d4-a716-446655440000"}, "{550e8400-e29b-41d4-a716-446655440000}"), text: "{550e8400-e29b-41d4-a716-446655440000}", null: rtNullString},
		{name: "bytea_array", typ: "bytea[]", kind: "array", lit: `'{"\\x00ff"}'`, arg: `{"\\x00ff"}`, want: rtWantArr([]driver.Value{[]byte{0, 0xff}}, `{"\\x00ff"}`), text: `{"\\x00ff"}`, null: rtNullString},
		{name: "int4_array_2d", typ: "int4[]", kind: "arr2d", lit: "'{{1,2},{3,4}}'", arg: "{{1,2},{3,4}}", want: rtWantArr(nil, "{{1,2},{3,4}}"), text: "{{1,2},{3,4}}", null: rtNullString},
		{name: "numeric_array", typ: "numeric[]", kind: "nocodec", lit: "'{1.5,2.50}'", arg: "{1.5,2.50}", want: rtWantText("{1.5,2.50}"), text: "{1.5,2.50}", null: rtNullString},
		{name: "date_array", typ: "date[]", kind: "nocodec", lit: "'{2025-01-15}'", arg: "{2025-01-15}", want: rtWantText("{2025-01-15}"), text: "{2025-01-15}", null: rtNullString},
		{name: "timestamptz_array", typ: "timestamptz[]", kind: "nocodec", lit: "'{\"2025-01-15 12:00:00+00\"}'", arg: `{"2025-01-15 12:00:00+00"}`, want: rtWantText(`{"2025-01-15 12:00:00+00"}`), text: `{"2025-01-15 12:00:00+00"}`, null: rtNullString},
		// network, enum and other text-on-the-wire types
		{name: "inet_v4_mask", typ: "inet", kind: "nocodec", lit: "'192.168.1.5/24'", arg: "192.168.1.5/24", want: rtWantText("192.168.1.5/24"), text: "192.168.1.5/24", null: rtNullString},
		{name: "inet_v4", typ: "inet", kind: "nocodec", lit: "'192.168.1.5'", arg: "192.168.1.5", want: rtWantText("192.168.1.5"), text: "192.168.1.5", null: rtNullString},
		{name: "inet_v6", typ: "inet", kind: "nocodec", lit: "'::1'", arg: "::1", want: rtWantText("::1"), text: "::1", null: rtNullString},
		{name: "cidr_v4", typ: "cidr", kind: "nocodec", lit: "'10.0.0.0/8'", arg: "10.0.0.0/8", want: rtWantText("10.0.0.0/8"), text: "10.0.0.0/8", null: rtNullString},
		{name: "cidr_v6", typ: "cidr", kind: "nocodec", lit: "'2001:db8::/32'", arg: "2001:db8::/32", want: rtWantText("2001:db8::/32"), text: "2001:db8::/32", null: rtNullString},
		{name: "macaddr", typ: "macaddr", kind: "nocodec", lit: "'08:00:2b:01:02:03'", arg: "08:00:2b:01:02:03", want: rtWantText("08:00:2b:01:02:03"), text: "08:00:2b:01:02:03", null: rtNullString},
		{name: "oid", typ: "oid", kind: "nocodec", lit: "4294967295", arg: "4294967295", want: rtWantText("4294967295"), text: "4294967295", null: rtNullString},
		{name: "enum", typ: "%ENUM%", kind: "text", lit: "'happy'", arg: "happy", want: rtWantText("happy"), text: "happy", null: rtNullString},
		{name: "enum_array", typ: "%ENUM%[]", kind: "nocodec", lit: "'{happy,sad}'", arg: "{happy,sad}", want: rtWantText("{happy,sad}"), text: "{happy,sad}", null: rtNullString},
	}
}

// rtKnownBug classifies a cell that fails today. The ids are the entries of
// the failures list in the PR description; the default build does not run
// them (rtKnownBugs is false). dest is one of any, native, str, bytes, null.
func rtKnownBug(c *rtCase, api, proto, dest string) string {
	bin := proto != "simple"
	switch c.kind {
	case "nocodec":
		// No codec is registered for the type, and the driver asks for binary
		// result columns on every extended query, so the wire bytes come back.
		if bin && dest != "null" {
			return "B3 no codec: binary wire bytes returned for " + c.typ
		}
	case "array":
		if bin && (dest == "str" || dest == "bytes") {
			if api == "pool" {
				return "B4 Pool.Scan raw fast path returns the binary array bytes (an error once the raw copy is limited to codecs whose binary form is the value, #1333)"
			}
			return "B4 binary arrays decode to []driver.Value, which database/sql cannot scan into string or []byte"
		}
	case "arr2d":
		if bin {
			return "B12 multi-dimensional arrays are flattened (dimensions lost)"
		}
	case "tsfar":
		if bin && dest != "null" {
			return "B13 binary timestamp decode overflows a time.Duration outside about 1708..2292"
		}
	case "int", "numeric", "uuid", "bool", "float":
		if api == "pool" && bin && (dest == "str" || dest == "bytes") {
			return "B1 Pool.Scan raw fast path returns the binary wire bytes for " + c.typ
		}
	case "bytea":
		if api == "pool" && !bin && dest == "bytes" {
			return "B2 Pool.Scan raw fast path returns the text hex form of bytea"
		}
	case "datebc":
		if !bin && dest != "null" {
			return "B5 simple protocol cannot parse a BC date"
		}
	}
	return ""
}

// rtRow is anything with Scan(dest...): *sql.Row and *postgres.Row.
type rtRow interface{ Scan(dest ...any) error }

type rtConn struct {
	api, proto string
	query      func(ctx context.Context, q string, args ...any) rtRow
}

func rtOpenVariants(t *testing.T) []rtConn {
	t.Helper()
	dsn := dsnFromEnv(t)
	nocache := dsn
	if strings.Contains(nocache, "?") {
		nocache += "&auto_cache_statements=false"
	} else {
		nocache += "?auto_cache_statements=false"
	}
	db := openDB(t)
	dbNC, err := sql.Open(driverName, nocache)
	if err != nil {
		t.Fatalf("sql.Open (nocache): %v", err)
	}
	t.Cleanup(func() { _ = dbNC.Close() })
	pool := openPool(t)
	poolNC, err := postgres.Open(nocache, postgres.WithMaxOpen(4))
	if err != nil {
		t.Fatalf("postgres.Open (nocache): %v", err)
	}
	t.Cleanup(func() { _ = poolNC.Close() })

	sqlQ := func(d *sql.DB) func(context.Context, string, ...any) rtRow {
		return func(ctx context.Context, q string, args ...any) rtRow { return d.QueryRowContext(ctx, q, args...) }
	}
	poolQ := func(p *postgres.Pool) func(context.Context, string, ...any) rtRow {
		return func(ctx context.Context, q string, args ...any) rtRow { return p.QueryRow(ctx, q, args...) }
	}
	prep := func(ctx context.Context, q string, args ...any) rtRow {
		stmt, err := db.PrepareContext(ctx, q)
		if err != nil {
			return rtErrRow{err}
		}
		rows, err := stmt.QueryContext(ctx, args...)
		if err != nil {
			_ = stmt.Close()
			return rtErrRow{err}
		}
		return &rtRowsRow{rows: rows, stmt: stmt}
	}
	return []rtConn{
		{"sql", "simple", sqlQ(db)},
		{"sql", "ext", sqlQ(db)},
		{"sql", "nocache", sqlQ(dbNC)},
		{"sql", "prep", prep},
		{"pool", "simple", poolQ(pool)},
		{"pool", "ext", poolQ(pool)},
		{"pool", "nocache", poolQ(poolNC)},
	}
}

type rtErrRow struct{ err error }

func (r rtErrRow) Scan(...any) error { return r.err }

// rtRowsRow is a one-row rtRow over *sql.Rows that closes the rows and the
// statement after Scan, so the explicit prepared-statement path can be read
// the same way as the others.
type rtRowsRow struct {
	rows *sql.Rows
	stmt *sql.Stmt
}

func (r *rtRowsRow) Scan(dest ...any) error {
	defer func() {
		_ = r.rows.Close()
		_ = r.stmt.Close()
	}()
	if !r.rows.Next() {
		if err := r.rows.Err(); err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	return r.rows.Scan(dest...)
}

// rtEnumType creates the enum the "enum" cases use and drops it afterwards.
// The name is unique per run so parallel invocations on one server do not clash.
func rtEnumType(t *testing.T, db *sql.DB) string {
	t.Helper()
	name := uniqueTableName(t, "mood")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, "CREATE TYPE "+name+" AS ENUM ('sad','ok','happy')"); err != nil {
		t.Fatalf("CREATE TYPE: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = db.ExecContext(ctx, "DROP TYPE IF EXISTS "+name+" CASCADE")
	})
	return name
}

// TestRoundTripTypes is the matrix described at the top of this file.
func TestRoundTripTypes(t *testing.T) {
	db := openDB(t)
	pool := openPool(t)
	variants := rtOpenVariants(t)
	enum := rtEnumType(t, db)

	for _, c := range rtCases() {
		t.Run(c.name, func(t *testing.T) {
			typ := strings.ReplaceAll(c.typ, "%ENUM%", enum)
			tbl := uniqueTableName(t, c.name)
			createTable(t, db, tbl, "id int primary key, c "+typ)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			// Writes. Rows 1-3 hold the value, rows 4-6 hold NULL; within each
			// triple: SQL literal (simple protocol), database/sql parameter,
			// native Pool parameter.
			ins := "INSERT INTO " + tbl + " (id, c) VALUES "
			mustExec := func(name string, f func() error) {
				t.Helper()
				if err := f(); err != nil {
					t.Fatalf("write %s: %v", name, err)
				}
			}
			mustExec("literal", func() error {
				_, err := db.ExecContext(ctx, ins+"(1, "+c.lit+")")
				return err
			})
			mustExec("literal NULL", func() error {
				_, err := db.ExecContext(ctx, ins+"(4, NULL)")
				return err
			})
			valueRows := []int{1}
			if c.arg != nil {
				mustExec("database/sql arg", func() error {
					_, err := db.ExecContext(ctx, ins+"($1, $2)", 2, c.arg)
					return err
				})
				mustExec("pool arg", func() error {
					_, err := pool.ExecContext(ctx, ins+"($1, $2)", 3, c.arg)
					return err
				})
				valueRows = append(valueRows, 2, 3)
			}
			mustExec("database/sql NULL arg", func() error {
				_, err := db.ExecContext(ctx, ins+"($1, $2)", 5, nil)
				return err
			})
			mustExec("pool NULL arg", func() error {
				_, err := pool.ExecContext(ctx, ins+"($1, $2)", 6, nil)
				return err
			})
			// Rows 2, 3, 5 and 6 are read back as `any` (the value the write
			// paths must have stored); row 1 and the NULL row 4 get every dest.

			sel := "SELECT c FROM " + tbl + " WHERE id = "
			for _, v := range variants {
				for _, id := range append(append([]int{}, valueRows...), 4, 5, 6) {
					isNull := id >= 4
					q := sel + strconv.Itoa(id)
					var args []any
					if v.proto != "simple" {
						q = sel + "$1"
						args = []any{id}
					}
					cell := func(dest string, run func(t *testing.T)) {
						bug := ""
						if !isNull { // NULL is a length of -1 in either format: never affected
							bug = rtKnownBug(&c, v.api, v.proto, dest)
						}
						if bug != "" && !rtKnownBugs {
							return
						}
						name := fmt.Sprintf("%s/%s/row%d/%s", v.api, v.proto, id, dest)
						t.Run(name, func(t *testing.T) {
							if bug != "" {
								t.Logf("known bug: %s", bug)
							}
							run(t)
						})
					}

					cell("any", func(t *testing.T) {
						var got any
						if err := v.query(ctx, q, args...).Scan(&got); err != nil {
							t.Fatalf("Scan(any): %v", err)
						}
						if isNull {
							if got != nil {
								t.Fatalf("NULL read back as %T(%v)", got, got)
							}
							return
						}
						if err := c.want(got); err != nil {
							t.Fatal(err)
						}
					})
					if isNull {
						if c.null != nil && id == 4 {
							cell("null", func(t *testing.T) {
								dest, valid := c.null()
								if err := v.query(ctx, q, args...).Scan(dest); err != nil {
									t.Fatalf("Scan(sql.Null*): %v", err)
								}
								if valid() {
									t.Fatalf("NULL scanned as valid")
								}
							})
						}
						continue
					}
					if id != 1 {
						continue
					}
					for i, mk := range c.nat {
						cell(fmt.Sprintf("native%d", i), func(t *testing.T) {
							dest, get := mk()
							if err := v.query(ctx, q, args...).Scan(dest); err != nil {
								t.Fatalf("Scan(%T): %v", dest, err)
							}
							if err := c.want(get()); err != nil {
								t.Fatalf("Scan(%T): %v", dest, err)
							}
						})
					}
					if c.text != "" || c.kind == "text" {
						cell("str", func(t *testing.T) {
							var got string
							if err := v.query(ctx, q, args...).Scan(&got); err != nil {
								t.Fatalf("Scan(string): %v", err)
							}
							if got != c.text {
								t.Fatalf("string: got %q, want %q", rtClip(got), rtClip(c.text))
							}
						})
					}
					if c.text != "" || c.raw != nil {
						cell("bytes", func(t *testing.T) {
							var got []byte
							if err := v.query(ctx, q, args...).Scan(&got); err != nil {
								t.Fatalf("Scan([]byte): %v", err)
							}
							want := []byte(c.text)
							if c.raw != nil {
								want = c.raw
							}
							if !bytes.Equal(got, want) {
								t.Fatalf("[]byte: got %q, want %q", rtClip(string(got)), rtClip(string(want)))
							}
						})
					}
				}
			}
		})
	}
}

func rtClip(s string) string {
	if len(s) > 80 {
		return s[:80] + "...(" + strconv.Itoa(len(s)) + " bytes)"
	}
	return s
}
