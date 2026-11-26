package redis

import (
	"math"
	"strings"
	"testing"

	"github.com/goceleris/celeris/internal/driver/redis/protocol"
)

// asStringMap is reached through HGetAll, whose values are always bulk, so no
// server round trip exposes a non-bulk map value; this pins the decoder
// directly. A RESP3 map with integer, double or boolean values must read the
// same as the RESP2 flat array of their bulk or integer downgrade.
func TestAsStringMapNonBulkValues(t *testing.T) {
	bulk := func(s string) protocol.Value { return protocol.Value{Type: protocol.TyBulk, Str: []byte(s)} }
	m := protocol.Value{Type: protocol.TyMap, Map: []protocol.KV{
		{K: bulk("i"), V: protocol.Value{Type: protocol.TyInt, Int: 7}},
		{K: bulk("d"), V: protocol.Value{Type: protocol.TyDouble, Float: 3.5}},
		{K: bulk("b"), V: protocol.Value{Type: protocol.TyBool, Bool: true}},
		{K: bulk("s"), V: bulk("x")},
	}}
	got, err := asStringMap(m)
	if err != nil {
		t.Fatalf("asStringMap(map): %v", err)
	}
	want := map[string]string{"i": "7", "d": "3.5", "b": "1", "s": "x"}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("map[%q] = %q, want %q", k, got[k], w)
		}
	}
	flat := protocol.Value{Type: protocol.TyArray, Array: []protocol.Value{
		bulk("i"), {Type: protocol.TyInt, Int: 7}, bulk("s"), bulk("x"),
	}}
	got, err = asStringMap(flat)
	if err != nil || got["i"] != "7" || got["s"] != "x" {
		t.Errorf("asStringMap(flat array) = %v, %v; want i=7 s=x", got, err)
	}
}

// TestAsStringSliceElementKinds: one element of each kind, as DoSlice sees it
// on a RESP3 connection.
func TestAsStringSliceElementKinds(t *testing.T) {
	bulk := func(s string) protocol.Value { return protocol.Value{Type: protocol.TyBulk, Str: []byte(s)} }
	elems := []protocol.Value{
		bulk("b"),
		{Type: protocol.TyNull},
		{Type: protocol.TyInt, Int: -3},
		{Type: protocol.TyDouble, Float: 2.5},
		{Type: protocol.TyBool, Bool: false},
		{Type: protocol.TyBigInt, BigN: []byte("123456789012345678901234567890")},
		{Type: protocol.TySimple, Str: []byte("OK")},
		{Type: protocol.TyVerbatim, Str: []byte("txt:v")},
	}
	got, err := asStringSlice(protocol.Value{Type: protocol.TyArray, Array: elems})
	if err != nil {
		t.Fatalf("asStringSlice: %v", err)
	}
	want := []string{"b", "", "-3", "2.5", "0", "123456789012345678901234567890", "OK", "v"}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("element %d = %q, want %q", i, got[i], w)
		}
	}
	for name, bad := range map[string]protocol.Value{
		"nested array": {Type: protocol.TyArray, Array: []protocol.Value{bulk("x")}},
		"error":        {Type: protocol.TyError, Str: []byte("ERR boom")},
	} {
		if s, err := asStringSlice(protocol.Value{Type: protocol.TyArray, Array: []protocol.Value{bad}}); err == nil {
			t.Errorf("%s element: got %q and no error, want an error", name, s)
		}
	}
}

// TestFormatDouble: the text of a RESP3 double is the text the server sent in
// RESP2. Every pair below was read from a RESP2 reply of Redis 7.4.8, 8.0.6,
// 8.2.5, 8.4.2, 8.6.2 and Valkey 9.0.3 (the same on all six) for the score
// given on the left.
func TestFormatDouble(t *testing.T) {
	for _, c := range []struct {
		f    float64
		want string
	}{
		{1.5, "1.5"}, {3.5, "3.5"}, {100, "100"}, {0.1, "0.1"}, {-2.25, "-2.25"},
		{1000000, "1000000"}, {1700000000, "1700000000"}, {1e17, "100000000000000000"},
		{1e20, "1e+20"}, {1e21, "1e+21"},
		{0.0001, "0.0001"}, {0.00001, "0.00001"}, {1e-7, "1e-7"}, {1.5e-10, "1.5e-10"},
		{math.Inf(1), "inf"}, {math.Inf(-1), "-inf"}, {0, "0"},
	} {
		if got := formatDouble(c.f); got != c.want {
			t.Errorf("formatDouble(%v) = %q, want %q", c.f, got, c.want)
		}
	}
}

// TestIntBoolFloatOfRESP3Scalars: DoInt, DoBool and the float decoder read a
// RESP3 double, boolean or big number as they read the RESP2 text of it, and an
// error names the type.
func TestIntBoolFloatOfRESP3Scalars(t *testing.T) {
	dbl := func(f float64) protocol.Value { return protocol.Value{Type: protocol.TyDouble, Float: f} }
	big := func(s string) protocol.Value { return protocol.Value{Type: protocol.TyBigInt, BigN: []byte(s)} }
	boolean := func(b bool) protocol.Value { return protocol.Value{Type: protocol.TyBool, Bool: b} }
	for _, c := range []struct {
		name string
		v    protocol.Value
		n    int64
		errT string // "" or the type the error must name
	}{
		{"true", boolean(true), 1, ""}, {"false", boolean(false), 0, ""},
		{"integral double", dbl(100), 100, ""}, {"negative integral double", dbl(-7), -7, ""},
		{"fraction", dbl(3.5), 0, "double"}, {"infinity", dbl(math.Inf(1)), 0, "double"},
		{"big number that fits", big("42"), 42, ""}, {"big number too large", big("123456789012345678901234567890"), 0, "bigint"},
	} {
		n, err := asInt(c.v)
		switch {
		case c.errT == "" && (err != nil || n != c.n):
			t.Errorf("asInt(%s) = %d, %v; want %d", c.name, n, err, c.n)
		case c.errT != "" && (err == nil || !strings.Contains(err.Error(), c.errT)):
			t.Errorf("asInt(%s) = %d, %v; want an error naming %q", c.name, n, err, c.errT)
		}
	}
	if b, err := asBool(dbl(1)); err != nil || !b {
		t.Errorf("asBool(double 1) = %v, %v; want true", b, err)
	}
	if b, err := asBool(dbl(0)); err != nil || b {
		t.Errorf("asBool(double 0) = %v, %v; want false", b, err)
	}
	if _, err := asBool(dbl(1.5)); err == nil {
		t.Error("asBool(double 1.5): want an error, as for the bulk string 1.5")
	}
	if f, err := asFloat(big("12345678901234567890")); err != nil || f != 12345678901234567890 {
		t.Errorf("asFloat(big number) = %v, %v", f, err)
	}
	for name, v := range map[string]protocol.Value{
		"map":   {Type: protocol.TyMap, Map: []protocol.KV{{K: dbl(1), V: dbl(2)}}},
		"array": {Type: protocol.TyArray},
		"set":   {Type: protocol.TySet},
	} {
		if s, err := asString(v); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("asString(%s) = %q, %v; want an error naming %q", name, s, err, name)
		}
	}
}
