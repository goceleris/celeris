package postgres

import (
	"strings"
	"sync/atomic"
	"testing"

	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// A binary-only type with no codec never reaches *string / *[]byte as raw wire
// bytes: it is an error naming the type. *any keeps the raw bytes (v1.5.11), a
// type that is not listed (an enum's binary form is its text) is left alone,
// and the simple protocol's text is the value.
func TestScanRawBinaryOnlyTypeIntoString(t *testing.T) {
	raw := make([]byte, 16)
	binaryCol := func(oid uint32) protocol.ColumnDesc {
		return protocol.ColumnDesc{TypeOID: oid, FormatCode: protocol.FormatBinary}
	}
	for _, oid := range []uint32{600, 790, 1560, 3904, 3614, 1231, 1182} {
		var s string
		err := scanRaw(&s, raw, nil, binaryCol(oid), false)
		if err == nil || !strings.Contains(err.Error(), binaryOnlyTypes[oid]) || !strings.Contains(err.Error(), "::text") || s != "" {
			t.Errorf("oid %d into *string: got %q err %v, want an error naming %q and ::text", oid, s, err, binaryOnlyTypes[oid])
		}
		var b []byte
		if err := scanRaw(&b, raw, nil, binaryCol(oid), false); err == nil || b != nil {
			t.Errorf("oid %d into *[]byte: got %q err %v, want an error", oid, b, err)
		}
		var a any
		if err := scanRaw(&a, raw, nil, binaryCol(oid), false); err != nil {
			t.Errorf("oid %d into *any: %v", oid, err)
		} else if got, ok := a.([]byte); !ok || len(got) != 16 {
			t.Errorf("oid %d into *any: got %#v, want the raw bytes as in v1.5.11", oid, a)
		}
	}
	// An unlisted OID (an enum, for one) keeps v1.5.11: the binary form is the text.
	var s string
	if err := scanRaw(&s, []byte("happy"), nil, binaryCol(90001), false); err != nil || s != "happy" {
		t.Errorf("unlisted OID: got %q err %v, want happy", s, err)
	}
	// The simple protocol reads text: the text is the value.
	if err := scanRaw(&s, []byte("(1,2)"), nil, protocol.ColumnDesc{TypeOID: 600}, true); err != nil || s != "(1,2)" {
		t.Errorf("text format: got %q err %v", s, err)
	}
	// A NULL is a NULL.
	s = "STALE"
	if err := scanRaw(&s, nil, nil, binaryCol(600), false); err != nil || s != "" {
		t.Errorf("NULL: got %q err %v", s, err)
	}
}

// A streamed row (64 rows or more on the event loop) gets the same error.
func TestStreamedRowBinaryOnlyTypeIntoString(t *testing.T) {
	rowCh := make(chan [][]byte, 1)
	rowCh <- [][]byte{make([]byte, 16)}
	close(rowCh)
	doneCh := make(chan struct{})
	close(doneCh)
	var errVal atomic.Pointer[error]
	sr := &streamRows{
		columns: []protocol.ColumnDesc{{Name: "i", TypeOID: 600, FormatCode: protocol.FormatBinary}},
		codecs:  []*protocol.TypeCodec{nil},
		rowCh:   rowCh,
		doneCh:  doneCh,
		errVal:  &errVal,
	}
	rows := acquireRows(sr, nil, nil)
	if !rows.Next() {
		t.Fatal("no row")
	}
	var s string
	if err := rows.Scan(&s); err == nil || !strings.Contains(err.Error(), "point") || !strings.Contains(err.Error(), "::text") {
		t.Errorf("got %q err %v, want an error naming point and ::text", s, err)
	}
	var a any
	if err := rows.Scan(&a); err != nil {
		t.Errorf("*any: %v", err)
	}
}
