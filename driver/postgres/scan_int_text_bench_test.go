package postgres

import (
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// The benchmarked cells of the probatorium driver-pg scenarios read only INT and
// TEXT columns (users(id INT, name TEXT, email TEXT, score INT), bench_writes
// (id BIGSERIAL, payload TEXT)). These benchmarks run that scan, 50 rows of
// (int4, text, text, int4) in the binary result format through Rows.Next and
// Rows.Scan, and the codec lookup for those OIDs, so a change that adds codecs for
// other types can be shown not to move them: run them on the commit before and
// after the codec commits.

func intTextRows(n int) ([]protocol.ColumnDesc, [][][]byte) {
	cols := []protocol.ColumnDesc{
		{Name: "id", TypeOID: protocol.OIDInt4, FormatCode: protocol.FormatBinary},
		{Name: "name", TypeOID: protocol.OIDText, FormatCode: protocol.FormatBinary},
		{Name: "email", TypeOID: protocol.OIDText, FormatCode: protocol.FormatBinary},
		{Name: "score", TypeOID: protocol.OIDInt4, FormatCode: protocol.FormatBinary},
	}
	rows := make([][][]byte, n)
	for i := range rows {
		id := make([]byte, 4)
		binary.BigEndian.PutUint32(id, uint32(i+1))
		score := make([]byte, 4)
		binary.BigEndian.PutUint32(score, uint32(i*7))
		rows[i] = [][]byte{id, []byte(fmt.Sprintf("User %d", i+1)), []byte(fmt.Sprintf("user%d@example.com", i+1)), score}
	}
	return cols, rows
}

func BenchmarkScanIntText(b *testing.B) {
	cols, data := intTextRows(50)
	var (
		id, score int
		name, em  string
	)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows := acquireRows(acquirePGRows(cols, data, false, nil, nil), nil, nil)
		for rows.Next() {
			if err := rows.Scan(&id, &name, &em, &score); err != nil {
				b.Fatal(err)
			}
		}
	}
	if id != 50 || score != 49*7 || name != "User 50" || em != "user50@example.com" {
		b.Fatalf("wrong values: %d %q %q %d", id, name, em, score)
	}
}

func BenchmarkLookupOIDIntText(b *testing.B) {
	oids := []uint32{protocol.OIDInt4, protocol.OIDText, protocol.OIDInt8}
	b.ReportAllocs()
	var c *protocol.TypeCodec
	for i := 0; i < b.N; i++ {
		c = protocol.LookupOID(oids[i%len(oids)])
	}
	if c == nil {
		b.Fatal("no codec")
	}
}
