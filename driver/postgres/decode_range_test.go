package postgres

import (
	"encoding/binary"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/goceleris/celeris/driver/postgres/protocol"
)

// Regression tests for celeris#502: decodeTextInto / decodeBinaryInto
// narrowed the parsed int64 into int / int32 / int16 / uint32 without a
// range check, so an out-of-range value wrapped silently (2147483648 scanned
// into *int32 became -2147483648) instead of failing the scan the way the
// encode side ("int4 overflow") and database/sql do.

func int8Codec() *protocol.TypeCodec { return &protocol.TypeCodec{OID: protocol.OIDInt8} }

func beInt8(n int64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(n))
	return b[:]
}

func wantRangeErr(t *testing.T, handled bool, err error, want string) {
	t.Helper()
	if !handled {
		t.Fatalf("destination type not handled by the fast path")
	}
	if err == nil {
		t.Fatalf("expected out-of-range error for %s, got nil", want)
	}
	if !strings.Contains(err.Error(), "out of range") || !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not mention out of range for %s", err, want)
	}
}

func TestDecodeTextInto_Int32Overflow(t *testing.T) {
	var d int32 = 7
	handled, err := decodeTextInto(&d, []byte("2147483648"), nil)
	wantRangeErr(t, handled, err, "int32")
	if d != 7 {
		t.Fatalf("destination was written on error: %d", d)
	}

	d = 7
	handled, err = decodeTextInto(&d, []byte("-2147483649"), nil)
	wantRangeErr(t, handled, err, "int32")
	if d != 7 {
		t.Fatalf("destination was written on error: %d", d)
	}
}

func TestDecodeTextInto_Int32Boundaries(t *testing.T) {
	for _, want := range []int64{math.MinInt32, -1, 0, 1, math.MaxInt32} {
		var d int32
		handled, err := decodeTextInto(&d, []byte(strconv.FormatInt(want, 10)), nil)
		if !handled || err != nil {
			t.Fatalf("%d: handled=%v err=%v", want, handled, err)
		}
		if int64(d) != want {
			t.Fatalf("%d: got %d", want, d)
		}
	}
}

func TestDecodeTextInto_IntAndInt64Boundaries(t *testing.T) {
	for _, want := range []int64{math.MinInt64, math.MinInt, -1, 0, 1, math.MaxInt, math.MaxInt64} {
		var d64 int64
		handled, err := decodeTextInto(&d64, []byte(strconv.FormatInt(want, 10)), nil)
		if !handled || err != nil || d64 != want {
			t.Fatalf("int64 %d: handled=%v err=%v got=%d", want, handled, err, d64)
		}
		if want < math.MinInt || want > math.MaxInt {
			continue // only reachable on 32-bit builds
		}
		var d int
		handled, err = decodeTextInto(&d, []byte(strconv.FormatInt(want, 10)), nil)
		if !handled || err != nil {
			t.Fatalf("int %d: handled=%v err=%v", want, handled, err)
		}
		if int64(d) != want {
			t.Fatalf("int %d: got %d", want, d)
		}
	}
}

func TestDecodeBinaryInto_Int32Overflow(t *testing.T) {
	var d int32 = 7
	handled, err := decodeBinaryInto(&d, beInt8(2147483648), int8Codec())
	wantRangeErr(t, handled, err, "int32")
	if d != 7 {
		t.Fatalf("destination was written on error: %d", d)
	}
	handled, err = decodeBinaryInto(&d, beInt8(math.MinInt32-1), int8Codec())
	wantRangeErr(t, handled, err, "int32")
}

func TestDecodeBinaryInto_Int16Overflow(t *testing.T) {
	var d int16
	handled, err := decodeBinaryInto(&d, beInt8(math.MaxInt16+1), int8Codec())
	wantRangeErr(t, handled, err, "int16")
	handled, err = decodeBinaryInto(&d, beInt8(math.MinInt16-1), int8Codec())
	wantRangeErr(t, handled, err, "int16")
}

func TestDecodeBinaryInto_Uint32Overflow(t *testing.T) {
	var d uint32
	handled, err := decodeBinaryInto(&d, beInt8(-1), int8Codec())
	wantRangeErr(t, handled, err, "uint32")
	handled, err = decodeBinaryInto(&d, beInt8(math.MaxUint32+1), int8Codec())
	wantRangeErr(t, handled, err, "uint32")
}

// scanValue is the driver.Value fallback path (codec-less / non-fast-path
// destinations); it narrowed int64 the same way.
func TestScanValue_IntNarrowingRange(t *testing.T) {
	var d32 int32 = 7
	if err := scanValue(&d32, int64(2147483648)); err == nil || !strings.Contains(err.Error(), "out of range for int32") {
		t.Fatalf("int32: err=%v", err)
	}
	if d32 != 7 {
		t.Fatalf("int32 destination written on error: %d", d32)
	}
	var d16 int16
	if err := scanValue(&d16, int64(math.MinInt16-1)); err == nil || !strings.Contains(err.Error(), "out of range for int16") {
		t.Fatalf("int16: err=%v", err)
	}
	if err := scanValue(&d32, int64(math.MaxInt32)); err != nil || d32 != math.MaxInt32 {
		t.Fatalf("int32 boundary: err=%v got=%d", err, d32)
	}
	if err := scanValue(&d16, int64(math.MinInt16)); err != nil || d16 != math.MinInt16 {
		t.Fatalf("int16 boundary: err=%v got=%d", err, d16)
	}
	var di int
	if err := scanValue(&di, int64(math.MaxInt)); err != nil || di != math.MaxInt {
		t.Fatalf("int boundary: err=%v got=%d", err, di)
	}
}

func TestDecodeBinaryInto_Boundaries(t *testing.T) {
	codec := int8Codec()
	for _, want := range []int64{math.MinInt32, -1, 0, 1, math.MaxInt32} {
		var d int32
		if handled, err := decodeBinaryInto(&d, beInt8(want), codec); !handled || err != nil || int64(d) != want {
			t.Fatalf("int32 %d: handled=%v err=%v got=%d", want, handled, err, d)
		}
	}
	for _, want := range []int64{math.MinInt16, -1, 0, 1, math.MaxInt16} {
		var d int16
		if handled, err := decodeBinaryInto(&d, beInt8(want), codec); !handled || err != nil || int64(d) != want {
			t.Fatalf("int16 %d: handled=%v err=%v got=%d", want, handled, err, d)
		}
	}
	for _, want := range []int64{0, 1, math.MaxUint32} {
		var d uint32
		if handled, err := decodeBinaryInto(&d, beInt8(want), codec); !handled || err != nil || int64(d) != want {
			t.Fatalf("uint32 %d: handled=%v err=%v got=%d", want, handled, err, d)
		}
	}
	for _, want := range []int64{math.MinInt64, -1, 0, 1, math.MaxInt64} {
		var d int64
		if handled, err := decodeBinaryInto(&d, beInt8(want), codec); !handled || err != nil || d != want {
			t.Fatalf("int64 %d: handled=%v err=%v got=%d", want, handled, err, d)
		}
		if want < math.MinInt || want > math.MaxInt {
			continue
		}
		var di int
		if handled, err := decodeBinaryInto(&di, beInt8(want), codec); !handled || err != nil || int64(di) != want {
			t.Fatalf("int %d: handled=%v err=%v got=%d", want, handled, err, di)
		}
	}
}
