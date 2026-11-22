package redis

import (
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
