package protocol

import (
	"bytes"
	"database/sql/driver"
	"encoding/binary"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"
)

// roundTrip encodes v with the codec's binary path then decodes back and
// returns the decoded value. It fails the test on any encoder/decoder error.
func roundTripBinary(t *testing.T, oid uint32, v any) driver.Value {
	t.Helper()
	c := LookupOID(oid)
	if c == nil {
		t.Fatalf("no codec for OID %d", oid)
	}
	if c.EncodeBinary == nil {
		t.Fatalf("no binary encoder for OID %d", oid)
	}
	buf, err := c.EncodeBinary(nil, v)
	if err != nil {
		t.Fatalf("encode(%v): %v", v, err)
	}
	got, err := c.DecodeBinary(buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

func TestBoolRoundTrip(t *testing.T) {
	for _, v := range []bool{true, false} {
		got := roundTripBinary(t, OIDBool, v)
		if got != v {
			t.Fatalf("bool %v -> %v", v, got)
		}
	}
}

func TestInt2RoundTrip(t *testing.T) {
	cases := []int64{0, 1, -1, math.MinInt16, math.MaxInt16}
	for _, v := range cases {
		got := roundTripBinary(t, OIDInt2, v)
		if got.(int64) != v {
			t.Fatalf("int2 %d -> %v", v, got)
		}
	}
	// Overflow detection.
	c := LookupOID(OIDInt2)
	if _, err := c.EncodeBinary(nil, int64(math.MaxInt32)); err == nil {
		t.Fatal("expected overflow error")
	}
}

func TestInt4RoundTrip(t *testing.T) {
	cases := []int64{0, 1, -1, math.MinInt32, math.MaxInt32}
	for _, v := range cases {
		got := roundTripBinary(t, OIDInt4, v)
		if got.(int64) != v {
			t.Fatalf("int4 %d -> %v", v, got)
		}
	}
}

func TestInt8RoundTrip(t *testing.T) {
	cases := []int64{0, 1, -1, math.MinInt64, math.MaxInt64}
	for _, v := range cases {
		got := roundTripBinary(t, OIDInt8, v)
		if got.(int64) != v {
			t.Fatalf("int8 %d -> %v", v, got)
		}
	}
}

func TestFloat4RoundTrip(t *testing.T) {
	cases := []float64{0, 1.5, -3.25, float64(float32(math.Pi))}
	for _, v := range cases {
		got := roundTripBinary(t, OIDFloat4, v).(float64)
		if float32(got) != float32(v) {
			t.Fatalf("float4 %v -> %v", v, got)
		}
	}
}

func TestFloat8RoundTrip(t *testing.T) {
	got := roundTripBinary(t, OIDFloat8, math.Pi).(float64)
	if got != math.Pi {
		t.Fatalf("float8 pi -> %v", got)
	}
	// NaN round-trip.
	got = roundTripBinary(t, OIDFloat8, math.NaN()).(float64)
	if !math.IsNaN(got) {
		t.Fatalf("NaN round trip: %v", got)
	}
	// Inf
	got = roundTripBinary(t, OIDFloat8, math.Inf(1)).(float64)
	if !math.IsInf(got, 1) {
		t.Fatalf("+Inf round trip: %v", got)
	}
}

func TestTextRoundTrip(t *testing.T) {
	cases := []string{"", "hello", "üñíçødé", "with\nnewline", "\x00embedded"}
	for _, v := range cases {
		got := roundTripBinary(t, OIDText, v)
		if got.(string) != v {
			t.Fatalf("text %q -> %q", v, got)
		}
	}
}

func TestByteaRoundTrip(t *testing.T) {
	cases := [][]byte{
		{},
		{0},
		{0, 1, 255},
		{'h', 'i'},
	}
	for _, v := range cases {
		got := roundTripBinary(t, OIDBytea, v).([]byte)
		if !bytes.Equal(got, v) {
			t.Fatalf("bytea %v -> %v", v, got)
		}
	}
	// nil encodes to empty, decodes back as empty []byte.
	got := roundTripBinary(t, OIDBytea, []byte(nil)).([]byte)
	if len(got) != 0 {
		t.Fatalf("nil bytea -> %v", got)
	}
}

func TestUUIDRoundTrip(t *testing.T) {
	// Binary roundtrip: encode 16 raw bytes, decode yields the canonical
	// RFC 4122 hyphenated string form.
	u := []byte{0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc, 0xde, 0xf0,
		0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}
	const want = "12345678-9abc-def0-1122-334455667788"
	got := roundTripBinary(t, OIDUUID, u).(string)
	if got != want {
		t.Fatalf("uuid bytes -> %q want %q", got, want)
	}
	// String roundtrip: encode the canonical form, decode yields the same.
	gotStr := roundTripBinary(t, OIDUUID, want).(string)
	if gotStr != want {
		t.Fatalf("uuid string -> %q want %q", gotStr, want)
	}
}

func TestDateRoundTrip(t *testing.T) {
	cases := []time.Time{
		time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2099, 12, 31, 0, 0, 0, 0, time.UTC),
	}
	for _, v := range cases {
		got := roundTripBinary(t, OIDDate, v).(time.Time)
		if !got.Equal(v) {
			t.Fatalf("date %v -> %v", v, got)
		}
	}
}

func TestTimestampRoundTrip(t *testing.T) {
	cases := []time.Time{
		time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Unix(0, 0).UTC(),
		time.Date(2030, 6, 15, 12, 34, 56, 789000, time.UTC), // microsecond precision
	}
	for _, v := range cases {
		got := roundTripBinary(t, OIDTimestamp, v).(time.Time)
		if !got.Equal(v.Truncate(time.Microsecond)) {
			t.Fatalf("timestamp %v -> %v", v, got)
		}
	}
}

func TestTimestamptzUTC(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")
	in := time.Date(2024, 3, 15, 12, 0, 0, 0, loc)
	c := LookupOID(OIDTimestamptz)
	buf, err := c.EncodeBinary(nil, in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.DecodeBinary(buf)
	if err != nil {
		t.Fatal(err)
	}
	got := out.(time.Time)
	if !got.Equal(in) {
		t.Fatalf("instant mismatch: got %v want %v", got, in)
	}
	if got.Location() != time.UTC {
		t.Fatalf("timestamptz decoded in %v, want UTC", got.Location())
	}
}

func TestTimestampSubMicrosecondTruncation(t *testing.T) {
	// The wire format is microseconds, so nanos are silently truncated.
	in := time.Date(2024, 1, 1, 0, 0, 0, 500, time.UTC) // 500 ns
	c := LookupOID(OIDTimestamp)
	buf, err := c.EncodeBinary(nil, in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.DecodeBinary(buf)
	if err != nil {
		t.Fatal(err)
	}
	got := out.(time.Time)
	if got.Nanosecond() != 0 {
		t.Fatalf("expected truncated nanos, got %d", got.Nanosecond())
	}
}

func TestTimestampInfinityBinary(t *testing.T) {
	c := LookupOID(OIDTimestamp)
	// +infinity: 0x7FFFFFFFFFFFFFFF
	posInf := []byte{0x7F, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
	got, err := c.DecodeBinary(posInf)
	if err != nil {
		t.Fatal(err)
	}
	if !got.(time.Time).Equal(PGInfinity) {
		t.Fatalf("+infinity: got %v want %v", got, PGInfinity)
	}
	// -infinity: 0x8000000000000000
	negInf := []byte{0x80, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	got, err = c.DecodeBinary(negInf)
	if err != nil {
		t.Fatal(err)
	}
	if !got.(time.Time).Equal(PGNegInfinity) {
		t.Fatalf("-infinity: got %v want %v", got, PGNegInfinity)
	}
}

func TestDateInfinityBinary(t *testing.T) {
	c := LookupOID(OIDDate)
	// +infinity: 0x7FFFFFFF
	posInf := []byte{0x7F, 0xFF, 0xFF, 0xFF}
	got, err := c.DecodeBinary(posInf)
	if err != nil {
		t.Fatal(err)
	}
	if !got.(time.Time).Equal(PGInfinity) {
		t.Fatalf("+infinity: got %v want %v", got, PGInfinity)
	}
	// -infinity: 0x80000000
	negInf := []byte{0x80, 0x00, 0x00, 0x00}
	got, err = c.DecodeBinary(negInf)
	if err != nil {
		t.Fatal(err)
	}
	if !got.(time.Time).Equal(PGNegInfinity) {
		t.Fatalf("-infinity: got %v want %v", got, PGNegInfinity)
	}
}

func TestTimestampInfinityText(t *testing.T) {
	for _, oid := range []uint32{OIDTimestamp, OIDTimestamptz} {
		c := LookupOID(oid)
		got, err := c.DecodeText([]byte("infinity"))
		if err != nil {
			t.Fatalf("oid %d: %v", oid, err)
		}
		if !got.(time.Time).Equal(PGInfinity) {
			t.Fatalf("oid %d: +infinity text: got %v", oid, got)
		}
		got, err = c.DecodeText([]byte("-infinity"))
		if err != nil {
			t.Fatalf("oid %d: %v", oid, err)
		}
		if !got.(time.Time).Equal(PGNegInfinity) {
			t.Fatalf("oid %d: -infinity text: got %v", oid, got)
		}
	}
}

func TestDateInfinityText(t *testing.T) {
	c := LookupOID(OIDDate)
	got, err := c.DecodeText([]byte("infinity"))
	if err != nil {
		t.Fatal(err)
	}
	if !got.(time.Time).Equal(PGInfinity) {
		t.Fatalf("+infinity: got %v", got)
	}
	got, err = c.DecodeText([]byte("-infinity"))
	if err != nil {
		t.Fatal(err)
	}
	if !got.(time.Time).Equal(PGNegInfinity) {
		t.Fatalf("-infinity: got %v", got)
	}
}

func TestJSONBRoundTrip(t *testing.T) {
	in := []byte(`{"a":1}`)
	got := roundTripBinary(t, OIDJSONB, in).([]byte)
	if !bytes.Equal(got, in) {
		t.Fatalf("jsonb -> %s", got)
	}
}

func TestJSONBVersionByteRejected(t *testing.T) {
	c := LookupOID(OIDJSONB)
	// Build an invalid jsonb payload with version 2.
	bad := append([]byte{2}, []byte(`{}`)...)
	if _, err := c.DecodeBinary(bad); err == nil {
		t.Fatal("expected version error")
	}
}

func TestJSONRoundTrip(t *testing.T) {
	in := []byte(`{"b":2}`)
	got := roundTripBinary(t, OIDJSON, in).([]byte)
	if !bytes.Equal(got, in) {
		t.Fatalf("json -> %s", got)
	}
}

func TestNumericDecode(t *testing.T) {
	// Build binary forms by hand and verify decode.
	cases := []struct {
		bytes []byte
		want  string
	}{
		// 0: ndigits=0, weight=0, sign=0, dscale=0
		{[]byte{0, 0, 0, 0, 0, 0, 0, 0}, "0"},
		// NaN
		{[]byte{0, 0, 0, 0, 0xC0, 0, 0, 0}, "NaN"},
		// 123: ndigits=1, weight=0, sign=0, dscale=0, digit=0x007B (123)
		{[]byte{0, 1, 0, 0, 0, 0, 0, 0, 0, 123}, "123"},
		// -1: ndigits=1, weight=0, sign=0x4000, dscale=0, digit=1
		{[]byte{0, 1, 0, 0, 0x40, 0, 0, 0, 0, 1}, "-1"},
	}
	c := LookupOID(OIDNumeric)
	for i, tc := range cases {
		got, err := c.DecodeBinary(tc.bytes)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if got.(string) != tc.want {
			t.Fatalf("case %d: %q want %q", i, got, tc.want)
		}
	}
}

func TestNumericEncodeText(t *testing.T) {
	c := LookupOID(OIDNumeric)
	buf, err := c.EncodeText(nil, "123.456")
	if err != nil {
		t.Fatal(err)
	}
	if string(buf) != "123.456" {
		t.Fatalf("numeric text = %q", buf)
	}
}

func TestTextArrayRoundTrip(t *testing.T) {
	c := LookupOID(OIDTextArr)
	in := []any{"a", "b", "c"}
	buf, err := c.EncodeBinary(nil, in)
	if err != nil {
		t.Fatal(err)
	}
	outV, err := c.DecodeBinary(buf)
	if err != nil {
		t.Fatal(err)
	}
	out := outV.([]driver.Value)
	if len(out) != 3 {
		t.Fatalf("len=%d", len(out))
	}
	for i, want := range []string{"a", "b", "c"} {
		if out[i].(string) != want {
			t.Fatalf("[%d] = %v want %v", i, out[i], want)
		}
	}
}

func TestEmptyArrayRoundTrip(t *testing.T) {
	c := LookupOID(OIDTextArr)
	buf, err := c.EncodeBinary(nil, []any{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.DecodeBinary(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.([]driver.Value); len(got) != 0 {
		t.Fatalf("len=%d", len(got))
	}
}

func TestNullableArrayRoundTrip(t *testing.T) {
	c := LookupOID(OIDTextArr)
	in := []any{"x", nil, "y"}
	buf, err := c.EncodeBinary(nil, in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.DecodeBinary(buf)
	if err != nil {
		t.Fatal(err)
	}
	els := out.([]driver.Value)
	if len(els) != 3 {
		t.Fatalf("len=%d", len(els))
	}
	if els[0].(string) != "x" || els[1] != nil || els[2].(string) != "y" {
		t.Fatalf("got %v", els)
	}
}

type stubValuer struct{ v any }

func (s stubValuer) Value() (driver.Value, error) { return s.v, nil }

func TestValuerDispatch(t *testing.T) {
	c := LookupOID(OIDInt8)
	buf, err := c.EncodeBinary(nil, stubValuer{v: int64(42)})
	if err != nil {
		t.Fatal(err)
	}
	v, err := c.DecodeBinary(buf)
	if err != nil {
		t.Fatal(err)
	}
	if v.(int64) != 42 {
		t.Fatalf("got %v", v)
	}
}

func TestRegisterTypeOverride(t *testing.T) {
	// Save and restore the text codec so we don't pollute other tests.
	prev := LookupOID(OIDText)
	t.Cleanup(func() { RegisterType(prev) })

	sentinel := fmt.Errorf("custom-decoder-invoked")
	custom := &TypeCodec{
		OID:  OIDText,
		Name: "custom-text",
		DecodeBinary: func(src []byte) (driver.Value, error) {
			return nil, sentinel
		},
	}
	RegisterType(custom)
	got := LookupOID(OIDText)
	if got != custom {
		t.Fatal("override did not replace existing codec")
	}
	if _, err := got.DecodeBinary([]byte("x")); err != sentinel {
		t.Fatalf("custom decoder not invoked: %v", err)
	}
}

func TestScanTypes(t *testing.T) {
	cases := []struct {
		oid  uint32
		want reflect.Type
	}{
		{OIDBool, reflect.TypeOf(false)},
		{OIDInt8, reflect.TypeOf(int64(0))},
		{OIDText, reflect.TypeOf("")},
		{OIDTimestamp, reflect.TypeOf(time.Time{})},
		{OIDBytea, reflect.TypeOf([]byte(nil))},
	}
	for _, tc := range cases {
		if got := LookupOID(tc.oid).ScanType; got != tc.want {
			t.Fatalf("oid %d: scan %v want %v", tc.oid, got, tc.want)
		}
	}
}

func TestDecodeNumericSmallFraction(t *testing.T) {
	// Binary encoding for 0.00001234:
	//   ndigits=1, weight=-2, sign=positive, dscale=8, digits=[1234]
	// Old code panicked because the loop started at i = weight+1 = -1,
	// causing an out-of-bounds access on the digits slice.
	src := make([]byte, 10)
	binary.BigEndian.PutUint16(src[0:2], 1)      // ndigits
	binary.BigEndian.PutUint16(src[2:4], 0xFFFE) // weight = -2 (int16)
	binary.BigEndian.PutUint16(src[4:6], 0)      // sign positive
	binary.BigEndian.PutUint16(src[6:8], 8)      // dscale
	binary.BigEndian.PutUint16(src[8:10], 1234)  // digits[0]

	v, err := decodeNumericBinary(src)
	if err != nil {
		t.Fatalf("decodeNumericBinary: %v", err)
	}
	want := "0.00001234"
	if v != want {
		t.Fatalf("got %q, want %q", v, want)
	}
}

func TestDecodeNumericVerySmallFraction(t *testing.T) {
	// 0.000000001234: weight=-3, ndigits=1, dscale=12, digits=[1234]
	src := make([]byte, 10)
	binary.BigEndian.PutUint16(src[0:2], 1)      // ndigits
	binary.BigEndian.PutUint16(src[2:4], 0xFFFD) // weight = -3 (int16)
	binary.BigEndian.PutUint16(src[4:6], 0)      // sign positive
	binary.BigEndian.PutUint16(src[6:8], 12)     // dscale
	binary.BigEndian.PutUint16(src[8:10], 1234)  // digits[0]

	v, err := decodeNumericBinary(src)
	if err != nil {
		t.Fatalf("decodeNumericBinary: %v", err)
	}
	want := "0.000000001234"
	if v != want {
		t.Fatalf("got %q, want %q", v, want)
	}
}

func TestLookupUnknownOID(t *testing.T) {
	if c := LookupOID(999999); c != nil {
		t.Fatalf("unknown oid returned %v", c)
	}
}
