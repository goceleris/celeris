package postgres

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"unsafe"

	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// Self-contained Pool-level benchmark for the native Query hot path (fake
// loopback backend, one prebuilt reply segment per query). It uses only API that
// exists in v1.5.11, main and the fix, so the identical file is dropped into each.

func benchMsg(typ byte, payload []byte) []byte {
	b := make([]byte, 5+len(payload))
	b[0] = typ
	binary.BigEndian.PutUint32(b[1:5], uint32(4+len(payload)))
	copy(b[5:], payload)
	return b
}

func benchReply() []byte {
	rd := []byte{0, 1, 'n', 0}
	var f [18]byte
	binary.BigEndian.PutUint32(f[6:10], protocol.OIDInt4)
	binary.BigEndian.PutUint16(f[10:12], 4)
	binary.BigEndian.PutUint32(f[12:16], 0xFFFFFFFF)
	rd = append(rd, f[:]...)
	dr := []byte{0, 1, 0, 0, 0, 1, '7'}
	var out []byte
	out = append(out, benchMsg(protocol.BackendRowDescription, rd)...)
	out = append(out, benchMsg(protocol.BackendDataRow, dr)...)
	out = append(out, benchMsg(protocol.BackendCommandComplete, append([]byte("SELECT 1"), 0))...)
	out = append(out, benchMsg(protocol.BackendReadyForQuery, []byte{'I'})...)
	return out
}

func benchPool(b *testing.B) *Pool {
	reply := benchReply()
	addr := startFakePGBench(b, func(c net.Conn) {
		fakePGTrustStartupB(b, c, 1, 2, func(c net.Conn) {
			var hdr [5]byte
			body := make([]byte, 256)
			for {
				if _, err := io.ReadFull(c, hdr[:]); err != nil {
					return
				}
				n := int(binary.BigEndian.Uint32(hdr[1:5])) - 4
				if n < 0 {
					return
				}
				if n > len(body) {
					body = make([]byte, n)
				}
				if _, err := io.ReadFull(c, body[:n]); err != nil {
					return
				}
				if hdr[0] == protocol.MsgTerminate {
					return
				}
				if hdr[0] == protocol.MsgQuery {
					if _, err := c.Write(reply); err != nil {
						return
					}
				}
			}
		})
	})
	host, port, _ := net.SplitHostPort(addr)
	p, err := Open("postgres://u@"+host+":"+port+"/?sslmode=disable", WithMaxOpen(1))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = p.Close() })
	return p
}

// BenchmarkPoolQuery: Pool.QueryContext + Next + Scan + Close (the native hot path).
func BenchmarkPoolQuery(b *testing.B) {
	p := benchPool(b)
	ctx := context.Background()
	b.Logf("sizeof(Rows)=%d sizeof(Row)=%d", unsafe.Sizeof(Rows{}), unsafe.Sizeof(Row{}))
	var v int64
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := p.QueryContext(ctx, "SELECT 7")
		if err != nil {
			b.Fatal(err)
		}
		if rows.Next() {
			if err := rows.Scan(&v); err != nil {
				b.Fatal(err)
			}
		}
		_ = rows.Close()
	}
}

// BenchmarkPoolQueryRow: Pool.QueryRow(...).Scan.
func BenchmarkPoolQueryRow(b *testing.B) {
	p := benchPool(b)
	ctx := context.Background()
	var v int64
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := p.QueryRow(ctx, "SELECT 7").Scan(&v); err != nil {
			b.Fatal(err)
		}
	}
}
