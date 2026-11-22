package postgres

import (
	"database/sql/driver"
	"encoding/binary"
	"encoding/json"
	"fmt"
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

// TestConvertAssignInfinityIntoString pins that the PGInfinity / PGNegInfinity
// sentinels read into *string and *[]byte as the server's own text.
func TestConvertAssignInfinityIntoString(t *testing.T) {
	for _, c := range []struct {
		in   time.Time
		want string
	}{
		{PGInfinity, "infinity"},
		{PGNegInfinity, "-infinity"},
		{time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), "2026-01-02T03:04:05Z"},
	} {
		var s string
		if err := convertAssign(&s, c.in); err != nil || s != c.want {
			t.Errorf("*string from %v: got %q err %v, want %q", c.in, s, err, c.want)
		}
		var b []byte
		if err := convertAssign(&b, c.in); err != nil || string(b) != c.want {
			t.Errorf("*[]byte from %v: got %q err %v, want %q", c.in, b, err, c.want)
		}
	}
}
