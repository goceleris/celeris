package postgres

import (
	"encoding/binary"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// A result of streamThreshold rows or more is promoted to a streamRows on the
// event loop. Its rows must scan exactly like the buffered ones: infinity into
// *string / *[]byte is the server's text (read from the wire value), a real
// value is itself (a real 9999-12-31 23:59:59 is in the conformance table), *time.Time and *any keep the exported
// sentinels.
func TestStreamedRowsInfinityIntoString(t *testing.T) {
	i64 := func(v int64) []byte { return binary.BigEndian.AppendUint64(nil, uint64(v)) }
	realMicros := int64(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).Sub(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) / time.Microsecond)
	cells := [][]byte{i64(math.MaxInt64), i64(math.MinInt64), i64(realMicros)}
	want := []string{"infinity", "-infinity", "2026-01-02T03:04:05Z"}

	rowCh := make(chan [][]byte, len(cells))
	for _, c := range cells {
		rowCh <- [][]byte{c}
	}
	close(rowCh)
	doneCh := make(chan struct{})
	close(doneCh)
	var errVal atomic.Pointer[error]
	sr := &streamRows{
		columns: []protocol.ColumnDesc{{Name: "t", TypeOID: protocol.OIDTimestamptz, FormatCode: protocol.FormatBinary}},
		codecs:  []*protocol.TypeCodec{protocol.LookupOID(protocol.OIDTimestamptz)},
		rowCh:   rowCh,
		doneCh:  doneCh,
		errVal:  &errVal,
	}
	rows := acquireRows(sr, nil, nil)
	for i := 0; rows.Next(); i++ {
		var s string
		if err := rows.Scan(&s); err != nil || s != want[i] {
			t.Errorf("row %d *string: got %q err %v, want %q", i, s, err, want[i])
		}
		var b []byte
		if err := rows.Scan(&b); err != nil || string(b) != want[i] {
			t.Errorf("row %d *[]byte: got %q err %v, want %q", i, b, err, want[i])
		}
		var tm time.Time
		if err := rows.Scan(&tm); err != nil {
			t.Fatalf("row %d *time.Time: %v", i, err)
		}
		if i == 0 && !tm.Equal(PGInfinity) || i == 1 && !tm.Equal(PGNegInfinity) {
			t.Errorf("row %d *time.Time: got %v, want the exported sentinel", i, tm)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
