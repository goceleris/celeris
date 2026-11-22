package protocol

import (
	"database/sql/driver"
	"encoding/binary"
	"reflect"
	"strings"
	"testing"
)

// binArray builds a PostgreSQL binary array of int4 with the given dimension
// lengths and lower bounds and the elements in row-major order.
func binArray(dims, lbs []int32, elems []int32) []byte {
	b := make([]byte, 0, 64)
	b = binary.BigEndian.AppendUint32(b, uint32(len(dims)))
	b = binary.BigEndian.AppendUint32(b, 0)
	b = binary.BigEndian.AppendUint32(b, OIDInt4)
	for i, d := range dims {
		b = binary.BigEndian.AppendUint32(b, uint32(d))
		b = binary.BigEndian.AppendUint32(b, uint32(lbs[i]))
	}
	for _, e := range elems {
		b = binary.BigEndian.AppendUint32(b, 4)
		b = binary.BigEndian.AppendUint32(b, uint32(e))
	}
	return b
}

func TestDecodeArrayBinaryValue_OneDimension(t *testing.T) {
	got, err := decodeArrayBinaryValue(binArray([]int32{3}, []int32{1}, []int32{1, 2, 3}))
	if err != nil {
		t.Fatal(err)
	}
	want := []driver.Value{int64(1), int64(2), int64(3)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

// A multi-dimensional array must not come back flattened: the dimensions would
// be lost without a trace. It is an error that says so.
func TestDecodeArrayBinaryValue_MultiDimensionalIsAnError(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  []byte
	}{
		{"2d", binArray([]int32{2, 2}, []int32{1, 1}, []int32{1, 2, 3, 4})},
		{"3d", binArray([]int32{2, 1, 2}, []int32{1, 1, 1}, []int32{1, 2, 3, 4})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeArrayBinaryValue(tc.src)
			if err == nil {
				t.Fatalf("got %#v and no error", got)
			}
			if !strings.Contains(err.Error(), "multi-dimensional") {
				t.Fatalf("error does not say why: %v", err)
			}
		})
	}
}

// A lower bound other than 1 ('[0:2]={1,2,3}') is lost the same way.
func TestDecodeArrayBinaryValue_LowerBoundIsAnError(t *testing.T) {
	got, err := decodeArrayBinaryValue(binArray([]int32{3}, []int32{0}, []int32{1, 2, 3}))
	if err == nil {
		t.Fatalf("got %#v and no error", got)
	}
	if !strings.Contains(err.Error(), "lower bound") {
		t.Fatalf("error does not say why: %v", err)
	}
}

// DecodeArrayBinary keeps decoding any shape for callers that want the Dims.
func TestDecodeArrayBinary_KeepsDims(t *testing.T) {
	a, err := DecodeArrayBinary(binArray([]int32{2, 2}, []int32{1, 1}, []int32{1, 2, 3, 4}))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a.Dims, []int32{2, 2}) || len(a.Elements) != 4 {
		t.Fatalf("dims=%v elements=%v", a.Dims, a.Elements)
	}
}

// An element with no binary codec is an error, not a copy of the wire bytes.
func TestDecodeArrayBinary_ElementWithoutCodecIsAnError(t *testing.T) {
	src := binArray([]int32{1}, []int32{1}, []int32{1})
	binary.BigEndian.PutUint32(src[8:12], 1186) // interval: no codec
	if a, err := DecodeArrayBinary(src); err == nil {
		t.Fatalf("got %#v and no error", a.Elements)
	}
}
