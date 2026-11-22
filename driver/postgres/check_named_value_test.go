package postgres

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"math"
	"testing"
	"time"
)

type (
	cnvMyInt    int
	cnvMyInt32  int32
	cnvMyString string
	cnvValuer   struct{ s string }
)

func (v cnvValuer) Value() (driver.Value, error) { return v.s, nil }

// TestCheckNamedValueAcceptsWhatDatabaseSQLWouldConvert pins the contract of
// pgConn.CheckNamedValue: because the conn implements driver.NamedValueChecker,
// database/sql no longer converts arguments itself, so the conn must unwrap
// driver.Valuer, sql.Null*, defined types and pointers and widen the integer
// kinds that encodeOne does not know. Before the fix CheckNamedValue returned
// nil for every value and encodeOne then rejected these with "unsupported
// argument type".
func TestCheckNamedValueAcceptsWhatDatabaseSQLWouldConvert(t *testing.T) {
	str := "p"
	ts := time.Date(2025, 1, 15, 12, 34, 56, 0, time.UTC)
	tests := []struct {
		name    string
		in      any
		want    string // text sent on the wire; "" with null means NULL
		null    bool
		keep    bool // the value must reach encodeOne as the time.Time `in` (or its sql.NullTime content); the wire form is encodeOne's business
		wantErr bool
	}{
		// Passed through unchanged (the types encodeOne handles itself).
		{name: "int", in: 7, want: "7"},
		{name: "int32", in: int32(-7), want: "-7"},
		{name: "int64", in: int64(math.MinInt64), want: "-9223372036854775808"},
		{name: "float32", in: float32(1.5), want: "1.5"},
		{name: "float64", in: 2.5, want: "2.5"},
		{name: "bool", in: true, want: "\x01"},
		{name: "string", in: "s", want: "s"},
		{name: "bytes", in: []byte("b"), want: "b"},
		{name: "time", in: ts, keep: true},
		{name: "nil", in: nil, null: true},

		// Converted by driver.DefaultParameterConverter.
		{name: "int16", in: int16(7), want: "7"},
		{name: "int8", in: int8(-3), want: "-3"},
		{name: "uint8", in: uint8(3), want: "3"},
		{name: "uint16", in: uint16(65535), want: "65535"},
		{name: "uint32", in: uint32(4000000000), want: "4000000000"},
		{name: "uint", in: uint(9), want: "9"},
		{name: "uint64", in: uint64(1 << 62), want: "4611686018427387904"},
		{name: "defined_int", in: cnvMyInt(8), want: "8"},
		{name: "defined_int32", in: cnvMyInt32(-8), want: "-8"},
		{name: "defined_string", in: cnvMyString("named"), want: "named"},
		{name: "valuer", in: cnvValuer{"v"}, want: "v"},
		{name: "json_RawMessage", in: json.RawMessage(`{"a":1}`), want: `{"a":1}`},
		{name: "NullString", in: sql.NullString{String: "x", Valid: true}, want: "x"},
		{name: "NullString_null", in: sql.NullString{}, null: true},
		{name: "NullInt64", in: sql.NullInt64{Int64: 5, Valid: true}, want: "5"},
		{name: "NullInt64_null", in: sql.NullInt64{}, null: true},
		{name: "NullInt32", in: sql.NullInt32{Int32: 5, Valid: true}, want: "5"},
		{name: "NullInt16", in: sql.NullInt16{Int16: 5, Valid: true}, want: "5"},
		{name: "NullByte", in: sql.NullByte{Byte: 5, Valid: true}, want: "5"},
		{name: "NullBool", in: sql.NullBool{Bool: true, Valid: true}, want: "\x01"},
		{name: "NullFloat64", in: sql.NullFloat64{Float64: 1.5, Valid: true}, want: "1.5"},
		{name: "NullTime", in: sql.NullTime{Time: ts, Valid: true}, keep: true}, // unwrapped to the time.Time itself
		{name: "Null_generic", in: sql.Null[int32]{V: 3, Valid: true}, want: "3"},
		{name: "pointer", in: &str, want: "p"},
		{name: "nil_pointer", in: (*string)(nil), null: true},

		// Rejected, with an error that names the type.
		{name: "uint64_high_bit", in: uint64(1 << 63), wantErr: true},
		{name: "struct", in: struct{ A int }{1}, wantErr: true},
		{name: "slice", in: []string{"a"}, wantErr: true},
	}
	c := &pgConn{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			nv := driver.NamedValue{Ordinal: 1, Value: tc.in}
			err := c.CheckNamedValue(&nv)
			if err == nil && tc.keep {
				if got, ok := nv.Value.(time.Time); !ok || !got.Equal(ts) {
					t.Fatalf("%T(%v): value after CheckNamedValue is %T(%v), want the time.Time %v", tc.in, tc.in, nv.Value, nv.Value, ts)
				}
				return
			}
			if err == nil {
				// What the extended path does next.
				var b []byte
				b, _, err = encodeOne(nv.Value)
				if err == nil {
					if tc.wantErr {
						t.Fatalf("%T(%v) accepted (sent %q), want an error", tc.in, tc.in, b)
					}
					if tc.null {
						if b != nil {
							t.Fatalf("%T(%v): got %q, want NULL", tc.in, tc.in, b)
						}
						return
					}
					if string(b) != tc.want {
						t.Fatalf("%T(%v): sent %q, want %q", tc.in, tc.in, b, tc.want)
					}
					return
				}
			}
			if !tc.wantErr {
				t.Fatalf("%T(%v): %v", tc.in, tc.in, err)
			}
		})
	}
}

// BenchmarkCheckNamedValueInt measures the per-argument cost on the hot path:
// database/sql calls CheckNamedValue once per bound argument, and an int is
// the most common one.
func BenchmarkCheckNamedValueInt(b *testing.B) {
	c := &pgConn{}
	nv := driver.NamedValue{Ordinal: 1, Value: 42}
	b.ReportAllocs()
	for b.Loop() {
		if err := c.CheckNamedValue(&nv); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCheckNamedValueMixed3 is the per-query cost for a typical three
// argument query (int, string, time.Time: all pass-through kinds).
func BenchmarkCheckNamedValueMixed3(b *testing.B) {
	c := &pgConn{}
	ts := time.Date(2025, 1, 15, 12, 34, 56, 0, time.UTC)
	nvs := []driver.NamedValue{{Ordinal: 1, Value: 42}, {Ordinal: 2, Value: "abc"}, {Ordinal: 3, Value: ts}}
	b.ReportAllocs()
	for b.Loop() {
		for i := range nvs {
			if err := c.CheckNamedValue(&nvs[i]); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// BenchmarkCheckNamedValueNullString is the converted path (one Valuer call).
func BenchmarkCheckNamedValueNullString(b *testing.B) {
	c := &pgConn{}
	v := sql.NullString{String: "x", Valid: true}
	b.ReportAllocs()
	for b.Loop() {
		nv := driver.NamedValue{Ordinal: 1, Value: v}
		if err := c.CheckNamedValue(&nv); err != nil {
			b.Fatal(err)
		}
	}
}
