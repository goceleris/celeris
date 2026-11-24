package postgres

import (
	"database/sql/driver"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// Regression tests for the native Rows.Scan fast path. The extended protocol
// asks for binary results; jsonb and jsonpath binary values carry a leading
// version byte (0x01) before the text, and the fast path used to copy the
// wire bytes into *string / *[]byte for every registered codec, so
// "SELECT tags FROM t WHERE id = $1" returned "\x01[\"popular\"]". The same
// shortcut returned raw big-endian bytes for int4, uuid, bool, float8,
// timestamp and friends. Each case here drives scanRaw (the function behind
// Rows.Scan) with the exact bytes the server sends, in both result formats.

const (
	testOIDJSONArr     uint32 = 199
	testOIDJSONBArr    uint32 = 3807
	testOIDJSONPath    uint32 = 4072
	testOIDJSONPathArr uint32 = 4073
)

// recScanner is a sql.Scanner that records the driver.Value it is handed.
type recScanner struct{ got driver.Value }

func (s *recScanner) Scan(v any) error {
	if b, ok := v.([]byte); ok {
		v = append([]byte(nil), b...)
	}
	s.got = v
	return nil
}

// vtext renders a driver.Value that carries text ([]byte or string).
func vtext(v any) string {
	switch x := v.(type) {
	case []byte:
		return string(x)
	case string:
		return x
	}
	return fmt.Sprintf("<%T>", v)
}

func binaryArray(elemOID uint32, elems ...[]byte) []byte {
	b := make([]byte, 0, 32)
	b = binary.BigEndian.AppendUint32(b, 1) // ndim
	b = binary.BigEndian.AppendUint32(b, 0) // hasNulls
	b = binary.BigEndian.AppendUint32(b, elemOID)
	b = binary.BigEndian.AppendUint32(b, uint32(len(elems))) // dim length
	b = binary.BigEndian.AppendUint32(b, 1)                  // lower bound
	for _, e := range elems {
		b = binary.BigEndian.AppendUint32(b, uint32(len(e)))
		b = append(b, e...)
	}
	return b
}

func TestScanRawJSONFamilyTextForm(t *testing.T) {
	cases := []struct {
		name   string
		oid    uint32
		raw    []byte
		format int16
		want   string
	}{
		{"jsonb-binary", protocol.OIDJSONB, []byte("\x01[\"popular\"]"), protocol.FormatBinary, `["popular"]`},
		{"jsonb-text", protocol.OIDJSONB, []byte(`["popular"]`), protocol.FormatText, `["popular"]`},
		{"json-binary", protocol.OIDJSON, []byte(`{"a":1}`), protocol.FormatBinary, `{"a":1}`},
		{"json-text", protocol.OIDJSON, []byte(`{"a":1}`), protocol.FormatText, `{"a":1}`},
		{"jsonpath-binary", testOIDJSONPath, []byte("\x01$.\"a\""), protocol.FormatBinary, `$."a"`},
		{"jsonpath-text", testOIDJSONPath, []byte(`$."a"`), protocol.FormatText, `$."a"`},
		{"text-binary", protocol.OIDText, []byte("plain"), protocol.FormatBinary, "plain"},
		{"varchar-binary", protocol.OIDVarchar, []byte("plain"), protocol.FormatBinary, "plain"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			codec := protocol.LookupOID(tc.oid)
			if codec == nil {
				t.Fatalf("no codec registered for OID %d", tc.oid)
			}
			col := protocol.ColumnDesc{TypeOID: tc.oid, FormatCode: tc.format}
			text := tc.format == protocol.FormatText

			var b []byte
			if err := scanRaw(&b, tc.raw, codec, col, text); err != nil || string(b) != tc.want {
				t.Errorf("*[]byte: got %q err %v, want %q", b, err, tc.want)
			}
			var s string
			if err := scanRaw(&s, tc.raw, codec, col, text); err != nil || s != tc.want {
				t.Errorf("*string: got %q err %v, want %q", s, err, tc.want)
			}
			var rm json.RawMessage
			if err := scanRaw(&rm, tc.raw, codec, col, text); err != nil || string(rm) != tc.want {
				t.Errorf("*json.RawMessage: got %q err %v, want %q", rm, err, tc.want)
			}
			var a any
			if err := scanRaw(&a, tc.raw, codec, col, text); err != nil {
				t.Errorf("*any: %v", err)
			} else if got := vtext(a); got != tc.want {
				t.Errorf("*any: got %#v, want %q", a, tc.want)
			}
			var sc recScanner
			if err := scanRaw(&sc, tc.raw, codec, col, text); err != nil {
				t.Errorf("sql.Scanner: %v", err)
			} else if got := vtext(sc.got); got != tc.want {
				t.Errorf("sql.Scanner: got %#v, want %q", sc.got, tc.want)
			}
			// The destination must not alias the wire buffer.
			if len(tc.raw) > 0 {
				before := string(b)
				tc.raw[len(tc.raw)-1] ^= 0xff
				if string(b) != before {
					t.Errorf("*[]byte aliases the wire buffer")
				}
				tc.raw[len(tc.raw)-1] ^= 0xff
			}
		})
	}
}

func TestScanRawJSONArraysBinary(t *testing.T) {
	cases := []struct {
		name  string
		oid   uint32
		elem  uint32
		elems [][]byte
		want  []string
	}{
		{"jsonb[]", testOIDJSONBArr, protocol.OIDJSONB, [][]byte{[]byte("\x01{\"a\": 1}"), []byte("\x01[]")}, []string{`{"a": 1}`, `[]`}},
		{"json[]", testOIDJSONArr, protocol.OIDJSON, [][]byte{[]byte(`{"a":1}`)}, []string{`{"a":1}`}},
		{"jsonpath[]", testOIDJSONPathArr, testOIDJSONPath, [][]byte{[]byte("\x01$.\"a\"")}, []string{`$."a"`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			codec := protocol.LookupOID(tc.oid)
			if codec == nil {
				t.Fatalf("no codec registered for OID %d", tc.oid)
			}
			raw := binaryArray(tc.elem, tc.elems...)
			col := protocol.ColumnDesc{TypeOID: tc.oid, FormatCode: protocol.FormatBinary}
			var a any
			if err := scanRaw(&a, raw, codec, col, false); err != nil {
				t.Fatalf("*any: %v", err)
			}
			vals, ok := a.([]driver.Value)
			if !ok || len(vals) != len(tc.want) {
				t.Fatalf("*any: got %#v, want %d elements", a, len(tc.want))
			}
			for i, w := range tc.want {
				if got := vtext(vals[i]); got != w {
					t.Errorf("element %d: got %#v, want %q", i, vals[i], w)
				}
			}
		})
	}
}

// Any registered codec whose binary form is not its text form must not be
// copied raw into *string / *[]byte.
func TestScanRawBinaryNotLeakedIntoStringDest(t *testing.T) {
	cases := []struct {
		name string
		oid  uint32
		raw  []byte
		want string
	}{
		{"int4", protocol.OIDInt4, []byte{0, 0, 0, 123}, "123"},
		{"bool", protocol.OIDBool, []byte{1}, "true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			codec := protocol.LookupOID(tc.oid)
			col := protocol.ColumnDesc{TypeOID: tc.oid, FormatCode: protocol.FormatBinary}
			var s string
			if err := scanRaw(&s, tc.raw, codec, col, false); err != nil || s != tc.want {
				t.Errorf("*string: got %q err %v, want %q", s, err, tc.want)
			}
		})
	}
}

// bytea in text format is the hex escape form ("\x616263"); scanning it into
// *[]byte / *string must yield the decoded bytes, as database/sql and lib/pq do.
func TestScanRawByteaText(t *testing.T) {
	codec := protocol.LookupOID(protocol.OIDBytea)
	col := protocol.ColumnDesc{TypeOID: protocol.OIDBytea, FormatCode: protocol.FormatText}
	raw := []byte(`\x616263`)
	var b []byte
	if err := scanRaw(&b, raw, codec, col, true); err != nil || string(b) != "abc" {
		t.Errorf("*[]byte: got %q err %v, want %q", b, err, "abc")
	}
	var s string
	if err := scanRaw(&s, raw, codec, col, true); err != nil || s != "abc" {
		t.Errorf("*string: got %q err %v, want %q", s, err, "abc")
	}
	// Binary format is the raw bytes already.
	bcol := protocol.ColumnDesc{TypeOID: protocol.OIDBytea, FormatCode: protocol.FormatBinary}
	var bb []byte
	if err := scanRaw(&bb, []byte("abc"), codec, bcol, false); err != nil || !reflect.DeepEqual(bb, []byte("abc")) {
		t.Errorf("binary *[]byte: got %q err %v", bb, err)
	}
}

// A NULL cell must clear a *json.RawMessage / *[]byte / *string rather than
// leave the previous row's value in place.
func TestScanRawNullClearsDest(t *testing.T) {
	codec := protocol.LookupOID(protocol.OIDJSONB)
	col := protocol.ColumnDesc{TypeOID: protocol.OIDJSONB, FormatCode: protocol.FormatBinary}
	rm := json.RawMessage(`["stale"]`)
	if err := scanRaw(&rm, nil, codec, col, false); err != nil || rm != nil {
		t.Errorf("*json.RawMessage: got %q err %v, want nil", rm, err)
	}
	b := []byte("stale")
	if err := scanRaw(&b, nil, codec, col, false); err != nil || b != nil {
		t.Errorf("*[]byte: got %q err %v, want nil", b, err)
	}
	s := "stale"
	if err := scanRaw(&s, nil, codec, col, false); err != nil || s != "" {
		t.Errorf("*string: got %q err %v, want empty", s, err)
	}
}

// Times scanned into string / []byte take the RFC 3339 form database/sql
// uses, not Go's "%v" form and not an error.
func TestScanRawTimesAsRFC3339(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)
	for _, tc := range []struct {
		name string
		oid  uint32
		in   time.Time
		want string
	}{
		{"date", protocol.OIDDate, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), "2026-01-02T00:00:00Z"},
		{"timestamp", protocol.OIDTimestamp, ts, "2026-01-02T03:04:05.123456Z"},
		{"timestamptz", protocol.OIDTimestamptz, ts, "2026-01-02T03:04:05.123456Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			codec := protocol.LookupOID(tc.oid)
			raw, err := codec.EncodeBinary(nil, tc.in)
			if err != nil {
				t.Fatal(err)
			}
			col := protocol.ColumnDesc{TypeOID: tc.oid, FormatCode: protocol.FormatBinary}
			var s string
			if err := scanRaw(&s, raw, codec, col, false); err != nil || s != tc.want {
				t.Errorf("*string: got %q err %v, want %q", s, err, tc.want)
			}
			var b []byte
			if err := scanRaw(&b, raw, codec, col, false); err != nil || string(b) != tc.want {
				t.Errorf("*[]byte: got %q err %v, want %q", b, err, tc.want)
			}
		})
	}
}

// Arrays decode to []driver.Value; scanning one into a string must fail
// loudly (as database/sql does) instead of formatting Go's "%v" of the
// element bytes ("[[123 34 97 ...]]").
func TestScanRawArrayIntoStringErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		oid   uint32
		elem  uint32
		elems [][]byte
	}{
		{"jsonb[]", testOIDJSONBArr, protocol.OIDJSONB, [][]byte{[]byte("\x01{\"a\": 1}")}},
		{"int4[]", protocol.OIDInt4Arr, protocol.OIDInt4, [][]byte{{0, 0, 0, 1}, {0, 0, 0, 2}}},
		{"text[]", protocol.OIDTextArr, protocol.OIDText, [][]byte{[]byte("a"), []byte("b c")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			codec := protocol.LookupOID(tc.oid)
			raw := binaryArray(tc.elem, tc.elems...)
			col := protocol.ColumnDesc{TypeOID: tc.oid, FormatCode: protocol.FormatBinary}
			var s string
			if err := scanRaw(&s, raw, codec, col, false); err == nil {
				t.Errorf("*string: got %q, want an error", s)
			}
			var b []byte
			if err := scanRaw(&b, raw, codec, col, false); err == nil {
				t.Errorf("*[]byte: got %q, want an error", b)
			}
			var a any
			if err := scanRaw(&a, raw, codec, col, false); err != nil {
				t.Errorf("*any: %v", err)
			}
		})
	}
}

// A time.Time never reads as the text "infinity": the PGInfinity date is also a
// real instant (9999-12-31 23:59:59). convertAssign formats it as v1.5.11 did
// (fmt's %v), which a streamed row still uses; the buffered raw path formats a
// date or timestamp into a string as RFC3339Nano (as database/sql does), see
// TestScanRawTimeIntoStringIsRFC3339Nano. The server's own text for infinity is
// read from the wire value in scanRaw, see TestScanRawInfinityIntoString.
func TestConvertAssignTimeIsNeverInfinityText(t *testing.T) {
	for _, c := range []struct {
		in   time.Time
		want string
	}{
		{PGInfinity, "9999-12-31 23:59:59 +0000 UTC"},
		{PGNegInfinity, "-4713-01-01 00:00:00 +0000 UTC"},
		{time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), "2026-01-02 03:04:05 +0000 UTC"},
	} {
		var s string
		if err := convertAssign(&s, c.in); err != nil || s != c.want {
			t.Errorf("*string from %v: got %q err %v, want %q", c.in, s, err, c.want)
		}
	}
}

// A buffered date, timestamp or timestamptz into *string is RFC3339Nano, a real
// 9999-12-31 23:59:59 included.
func TestScanRawTimeIntoStringIsRFC3339Nano(t *testing.T) {
	micros := func(tm time.Time) []byte {
		return binary.BigEndian.AppendUint64(nil, uint64(tm.Sub(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))/time.Microsecond))
	}
	codec := protocol.LookupOID(protocol.OIDTimestamptz)
	col := protocol.ColumnDesc{TypeOID: protocol.OIDTimestamptz, FormatCode: protocol.FormatBinary}
	tm := time.Date(2026, 1, 2, 3, 4, 5, 6000, time.UTC)
	var s string
	if err := scanRaw(&s, micros(tm), codec, col, false); err != nil || s != "2026-01-02T03:04:05.000006Z" {
		t.Errorf("*string: got %q err %v", s, err)
	}
}

// TestScanRawInfinityIntoString: date, timestamp and timestamptz infinity
// arrive as the binary sentinels (int64 MaxInt64/MinInt64 micros, int32
// MaxInt32/MinInt32 days) or the text "infinity" / "-infinity". A *string or
// *[]byte destination is filled from that raw value, before any time.Time
// exists, with the server's text. *time.Time and *any keep the exported
// sentinels (PGInfinity, PGNegInfinity), as in v1.5.11.
func TestScanRawInfinityIntoString(t *testing.T) {
	i64 := func(v int64) []byte { return binary.BigEndian.AppendUint64(nil, uint64(v)) }
	i32 := func(v int32) []byte { return binary.BigEndian.AppendUint32(nil, uint32(v)) }
	for _, c := range []struct {
		name   string
		oid    uint32
		binary []byte
		text   string
		want   string
		wantT  time.Time
	}{
		{"timestamp+inf", protocol.OIDTimestamp, i64(math.MaxInt64), "infinity", "infinity", PGInfinity},
		{"timestamp-inf", protocol.OIDTimestamp, i64(math.MinInt64), "-infinity", "-infinity", PGNegInfinity},
		{"timestamptz+inf", protocol.OIDTimestamptz, i64(math.MaxInt64), "infinity", "infinity", PGInfinity},
		{"timestamptz-inf", protocol.OIDTimestamptz, i64(math.MinInt64), "-infinity", "-infinity", PGNegInfinity},
		{"date+inf", protocol.OIDDate, i32(math.MaxInt32), "infinity", "infinity", PGInfinity},
		{"date-inf", protocol.OIDDate, i32(math.MinInt32), "-infinity", "-infinity", PGNegInfinity},
	} {
		codec := protocol.LookupOID(c.oid)
		for _, f := range []struct {
			name   string
			raw    []byte
			format int16
			text   bool
		}{
			{"binary", c.binary, protocol.FormatBinary, false},
			{"text", []byte(c.text), protocol.FormatText, true},
		} {
			t.Run(c.name+"/"+f.name, func(t *testing.T) {
				col := protocol.ColumnDesc{TypeOID: c.oid, FormatCode: f.format}
				var s string
				if err := scanRaw(&s, f.raw, codec, col, f.text); err != nil || s != c.want {
					t.Errorf("*string: got %q err %v, want %q", s, err, c.want)
				}
				var b []byte
				if err := scanRaw(&b, f.raw, codec, col, f.text); err != nil || string(b) != c.want {
					t.Errorf("*[]byte: got %q err %v, want %q", b, err, c.want)
				}
				var tm time.Time
				if err := scanRaw(&tm, f.raw, codec, col, f.text); err != nil || !tm.Equal(c.wantT) {
					t.Errorf("*time.Time: got %v err %v, want %v", tm, err, c.wantT)
				}
				var a any
				if err := scanRaw(&a, f.raw, codec, col, f.text); err != nil {
					t.Fatalf("*any: %v", err)
				}
				if at, ok := a.(time.Time); !ok || !at.Equal(c.wantT) {
					t.Errorf("*any: got %#v, want %v", a, c.wantT)
				}
			})
		}
	}

	// A timestamp one microsecond below the sentinel is a real value.
	codec := protocol.LookupOID(protocol.OIDTimestamp)
	var s string
	col := protocol.ColumnDesc{TypeOID: protocol.OIDTimestamp, FormatCode: protocol.FormatBinary}
	if err := scanRaw(&s, i64(math.MaxInt64-1), codec, col, false); err == nil && s == "infinity" {
		t.Errorf("MaxInt64-1 read as infinity")
	}
}
