package postgres

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"

	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// Pool-level benchmarks of the cached-statement path (the auto-cache hit: Bind,
// Execute, Sync of a statement named at prepare time), against a fake loopback
// backend that answers the extended protocol from prebuilt replies. They use
// only API that exists on main and on the fix, so the identical file runs on
// both: the name of a cached statement is built when it is prepared and stored
// in the cache entry, and a hit only reads it (celeris#1455).

func cachedBenchPool(b *testing.B) *Pool {
	b.Helper()
	// ParameterDescription (one int8), RowDescription (one int4 column, binary).
	params := rowsBenchMsg(protocol.BackendParameterDesc, []byte{0, 1, 0, 0, 0, byte(protocol.OIDInt8)})
	rd := []byte{0, 1, 'n', 0}
	var f [18]byte
	binary.BigEndian.PutUint32(f[6:10], protocol.OIDInt4)
	binary.BigEndian.PutUint16(f[10:12], 4)
	binary.BigEndian.PutUint32(f[12:16], 0xFFFFFFFF)
	binary.BigEndian.PutUint16(f[16:18], 1)
	rd = append(rd, f[:]...)
	rowDesc := rowsBenchMsg(protocol.BackendRowDescription, rd)
	parseOK := rowsBenchMsg(protocol.BackendParseComplete, nil)
	bindOK := rowsBenchMsg(protocol.BackendBindComplete, nil)
	dataRow := rowsBenchMsg(protocol.BackendDataRow, []byte{0, 1, 0, 0, 0, 4, 0, 0, 0, 7})
	cmdDone := rowsBenchMsg(protocol.BackendCommandComplete, append([]byte("SELECT 1"), 0))
	rfq := rowsBenchMsg(protocol.BackendReadyForQuery, []byte{'I'})

	addr := startFakePGBench(b, func(c net.Conn) {
		fakePGTrustStartupB(b, c, 1, 2, func(c net.Conn) {
			var hdr [5]byte
			body := make([]byte, 1024)
			var out []byte
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
				switch hdr[0] {
				case protocol.MsgTerminate:
					return
				case protocol.MsgParse:
					out = append(out, parseOK...)
				case protocol.MsgDescribe:
					out = append(out, params...)
					out = append(out, rowDesc...)
				case protocol.MsgBind:
					out = append(out, bindOK...)
				case protocol.MsgExecute:
					out = append(out, dataRow...)
					out = append(out, cmdDone...)
				case protocol.MsgSync:
					out = append(out, rfq...)
					if _, err := c.Write(out); err != nil {
						return
					}
					out = out[:0]
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

// BenchmarkPoolQueryCachedArgs: Pool.QueryContext with an argument, after the
// first call (a cache hit: Bind + Execute + Sync).
func BenchmarkPoolQueryCachedArgs(b *testing.B) {
	p := cachedBenchPool(b)
	ctx := context.Background()
	var v int32
	// The first call prepares and caches.
	rows, err := p.QueryContext(ctx, "SELECT $1::int4", int64(7))
	if err != nil {
		b.Fatal(err)
	}
	_ = rows.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := p.QueryContext(ctx, "SELECT $1::int4", int64(7))
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

// BenchmarkPoolQueryRowCachedArgs: Pool.QueryRow(...).Scan with an argument,
// after the first call.
func BenchmarkPoolQueryRowCachedArgs(b *testing.B) {
	p := cachedBenchPool(b)
	ctx := context.Background()
	var v int32
	if err := p.QueryRow(ctx, "SELECT $1::int4", int64(7)).Scan(&v); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := p.QueryRow(ctx, "SELECT $1::int4", int64(7)).Scan(&v); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPoolExecCachedArgs: Pool.ExecContext of an auto-cached write after
// its first call.
func BenchmarkPoolExecCachedArgs(b *testing.B) {
	p := cachedBenchPool(b)
	ctx := context.Background()
	if _, err := p.ExecContext(ctx, "INSERT INTO t(a) VALUES($1)", int64(7)); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := p.ExecContext(ctx, "INSERT INTO t(a) VALUES($1)", int64(7)); err != nil {
			b.Fatal(err)
		}
	}
}
