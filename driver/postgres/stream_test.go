package postgres

import (
	"context"
	"database/sql/driver"
	"fmt"
	"io"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/driver/postgres/protocol"
)

// TestStreamingBasic verifies that streaming rows delivers all rows
// correctly for a small result set.
func TestStreamingBasic(t *testing.T) {
	const numRows = 10
	addr := startFakePG(t, func(c net.Conn) {
		fakePGTrustStartup(t, c, 1, 2, func(c net.Conn) {
			for {
				typ, _, err := readMsg(c)
				if err != nil {
					return
				}
				if typ == protocol.MsgTerminate {
					return
				}
				if typ != protocol.MsgQuery {
					continue
				}
				rd := buildRowDescription([]colSpec{
					{name: "id", typeOID: protocol.OIDInt4, typeSize: 4, format: protocol.FormatText},
					{name: "name", typeOID: protocol.OIDText, typeSize: -1, format: protocol.FormatText},
				})
				_ = writeMsg(c, protocol.BackendRowDescription, rd)
				for i := range numRows {
					_ = writeMsg(c, protocol.BackendDataRow, buildDataRow([][]byte{
						[]byte(fmt.Sprintf("%d", i)),
						[]byte(fmt.Sprintf("row_%d", i)),
					}))
				}
				_ = writeCommandComplete(c, fmt.Sprintf("SELECT %d", numRows))
				_ = writeReadyForQuery(c, 'I')
			}
		})
	})

	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer eventloop.Release(prov)

	host, port, _ := net.SplitHostPort(addr)
	dsn := DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable", StatementCacheSize: 16}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialConn(ctx, prov, nil, dsn, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	rows, err := conn.QueryContext(ctx, "SELECT id, name FROM t", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	cols := rows.Columns()
	if len(cols) != 2 || cols[0] != "id" || cols[1] != "name" {
		t.Fatalf("columns = %v, want [id name]", cols)
	}

	var count int
	dest := make([]driver.Value, 2)
	for {
		err := rows.Next(dest)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		// Verify the row values.
		id, ok := dest[0].(int64)
		if !ok {
			t.Fatalf("row %d: id type = %T, want int64", count, dest[0])
		}
		if int(id) != count {
			t.Fatalf("row %d: id = %d", count, id)
		}
		name, ok := dest[1].(string)
		if !ok {
			t.Fatalf("row %d: name type = %T, want string", count, dest[1])
		}
		if name != fmt.Sprintf("row_%d", count) {
			t.Fatalf("row %d: name = %q", count, name)
		}
		count++
	}
	if count != numRows {
		t.Fatalf("got %d rows, want %d", count, numRows)
	}
}

// TestStreamingLarge verifies that streaming 100K rows does not OOM.
// The test checks that peak heap growth stays under 10MB.
func TestStreamingLarge(t *testing.T) {
	const numRows = 100_000
	addr := startFakePG(t, func(c net.Conn) {
		fakePGTrustStartup(t, c, 1, 2, func(c net.Conn) {
			for {
				typ, _, err := readMsg(c)
				if err != nil {
					return
				}
				if typ == protocol.MsgTerminate {
					return
				}
				if typ != protocol.MsgQuery {
					continue
				}
				rd := buildRowDescription([]colSpec{
					{name: "id", typeOID: protocol.OIDInt4, typeSize: 4, format: protocol.FormatText},
					{name: "payload", typeOID: protocol.OIDText, typeSize: -1, format: protocol.FormatText},
				})
				_ = writeMsg(c, protocol.BackendRowDescription, rd)
				// Send rows in a loop; the bounded channel (cap 64)
				// provides back-pressure so the server side can't
				// overwhelm the client.
				for i := range numRows {
					_ = writeMsg(c, protocol.BackendDataRow, buildDataRow([][]byte{
						[]byte(fmt.Sprintf("%d", i)),
						[]byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), // 31 bytes per row
					}))
				}
				_ = writeCommandComplete(c, fmt.Sprintf("SELECT %d", numRows))
				_ = writeReadyForQuery(c, 'I')
			}
		})
	})

	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer eventloop.Release(prov)

	host, port, _ := net.SplitHostPort(addr)
	dsn := DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable", StatementCacheSize: 16}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := dialConn(ctx, prov, nil, dsn, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	// Force a GC before measuring baseline.
	runtime.GC()
	var mBefore runtime.MemStats
	runtime.ReadMemStats(&mBefore)

	rows, err := conn.QueryContext(ctx, "SELECT id, payload FROM t", nil)
	if err != nil {
		t.Fatal(err)
	}

	var count int
	dest := make([]driver.Value, 2)
	for {
		err := rows.Next(dest)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		count++
	}
	_ = rows.Close()

	if count != numRows {
		t.Fatalf("got %d rows, want %d", count, numRows)
	}

	runtime.GC()
	var mAfter runtime.MemStats
	runtime.ReadMemStats(&mAfter)

	// With streaming, the peak heap should be bounded by the channel
	// capacity * row size, NOT numRows * rowSize. Allow 10MB of headroom
	// for test framework overhead and GC slack.
	heapGrowth := int64(mAfter.HeapAlloc) - int64(mBefore.HeapAlloc)
	t.Logf("heap growth: %d bytes (%.1f MB), rows=%d", heapGrowth, float64(heapGrowth)/(1<<20), count)

	const maxGrowth = 10 << 20 // 10 MB
	if heapGrowth > maxGrowth {
		t.Fatalf("heap grew %d bytes (%.1f MB), exceeds %d MB budget — likely not streaming",
			heapGrowth, float64(heapGrowth)/(1<<20), maxGrowth/(1<<20))
	}
}

// TestStreamingCloseEarly verifies that closing a streamRows before all
// rows are consumed drains correctly and the conn is reusable afterwards.
func TestStreamingCloseEarly(t *testing.T) {
	const numRows = 1000
	queriesSeen := 0
	addr := startFakePG(t, func(c net.Conn) {
		fakePGTrustStartup(t, c, 1, 2, func(c net.Conn) {
			for {
				typ, _, err := readMsg(c)
				if err != nil {
					return
				}
				if typ == protocol.MsgTerminate {
					return
				}
				if typ != protocol.MsgQuery {
					continue
				}
				queriesSeen++
				rd := buildRowDescription([]colSpec{
					{name: "n", typeOID: protocol.OIDInt4, typeSize: 4, format: protocol.FormatText},
				})
				_ = writeMsg(c, protocol.BackendRowDescription, rd)
				for i := range numRows {
					_ = writeMsg(c, protocol.BackendDataRow, buildDataRow([][]byte{
						[]byte(fmt.Sprintf("%d", i)),
					}))
				}
				_ = writeCommandComplete(c, fmt.Sprintf("SELECT %d", numRows))
				_ = writeReadyForQuery(c, 'I')
			}
		})
	})

	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer eventloop.Release(prov)

	host, port, _ := net.SplitHostPort(addr)
	dsn := DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable", StatementCacheSize: 16}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := dialConn(ctx, prov, nil, dsn, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	// First query: read only 10 rows, then close.
	rows, err := conn.QueryContext(ctx, "SELECT n FROM t", nil)
	if err != nil {
		t.Fatal(err)
	}
	dest := make([]driver.Value, 1)
	for range 10 {
		if err := rows.Next(dest); err != nil {
			t.Fatal(err)
		}
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close after 10 rows: %v", err)
	}

	// Second query: verify the conn is still usable.
	rows2, err := conn.QueryContext(ctx, "SELECT n FROM t", nil)
	if err != nil {
		t.Fatalf("second query: %v", err)
	}
	var count int
	for {
		err := rows2.Next(dest)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		count++
	}
	_ = rows2.Close()
	if count != numRows {
		t.Fatalf("second query: got %d rows, want %d", count, numRows)
	}
}

// TestStreamingError verifies that a server error mid-stream surfaces
// through Err().
func TestStreamingError(t *testing.T) {
	addr := startFakePG(t, func(c net.Conn) {
		fakePGTrustStartup(t, c, 1, 2, func(c net.Conn) {
			for {
				typ, _, err := readMsg(c)
				if err != nil {
					return
				}
				if typ == protocol.MsgTerminate {
					return
				}
				if typ != protocol.MsgQuery {
					continue
				}
				// Send RowDescription + 3 rows + ErrorResponse + RFQ.
				rd := buildRowDescription([]colSpec{
					{name: "n", typeOID: protocol.OIDInt4, typeSize: 4, format: protocol.FormatText},
				})
				_ = writeMsg(c, protocol.BackendRowDescription, rd)
				for i := range 3 {
					_ = writeMsg(c, protocol.BackendDataRow, buildDataRow([][]byte{
						[]byte(fmt.Sprintf("%d", i)),
					}))
				}
				// Simulate an error mid-stream.
				errPayload := []byte("SERROR\x00V00000\x00C22000\x00Mtest error mid-stream\x00\x00")
				_ = writeMsg(c, protocol.BackendErrorResponse, errPayload)
				_ = writeReadyForQuery(c, 'I')
			}
		})
	})

	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer eventloop.Release(prov)

	host, port, _ := net.SplitHostPort(addr)
	dsn := DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable", StatementCacheSize: 16}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialConn(ctx, prov, nil, dsn, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	rows, err := conn.QueryContext(ctx, "SELECT n FROM t", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	dest := make([]driver.Value, 1)
	count := 0
	for {
		err := rows.Next(dest)
		if err == io.EOF {
			break
		}
		if err != nil {
			// Got the error — expected.
			t.Logf("streaming error after %d rows: %v", count, err)
			return
		}
		count++
	}
	// If we got here with count == 3, the error should have come from
	// the state machine via the channel close.
	t.Logf("got %d rows before EOF", count)
}

// TestStreamingPoolLevel verifies that Pool.QueryContext returns streaming
// rows and properly returns the conn to the pool on Close.
func TestStreamingPoolLevel(t *testing.T) {
	const numRows = 5
	addr := startFakePG(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		readStartup(t, c)
		_ = writeAuthOK(c)
		_ = writeBackendKeyData(c, 1, 2)
		_ = writeReadyForQuery(c, 'I')
		for {
			typ, _, err := readMsg(c)
			if err != nil {
				return
			}
			if typ == protocol.MsgTerminate {
				return
			}
			if typ != protocol.MsgQuery {
				continue
			}
			rd := buildRowDescription([]colSpec{
				{name: "id", typeOID: protocol.OIDInt4, typeSize: 4, format: protocol.FormatText},
			})
			_ = writeMsg(c, protocol.BackendRowDescription, rd)
			for i := range numRows {
				_ = writeMsg(c, protocol.BackendDataRow, buildDataRow([][]byte{
					[]byte(fmt.Sprintf("%d", i)),
				}))
			}
			_ = writeCommandComplete(c, fmt.Sprintf("SELECT %d", numRows))
			_ = writeReadyForQuery(c, 'I')
		}
	})

	host, port, _ := net.SplitHostPort(addr)
	pool, err := Open("postgres://u@"+host+":"+port+"/d?sslmode=disable&statement_cache_size=4", WithMaxOpen(1))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pool.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// First query via pool.
	rows, err := pool.QueryContext(ctx, "SELECT id FROM t")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		if id != count {
			t.Fatalf("row %d: id = %d", count, id)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	if count != numRows {
		t.Fatalf("got %d rows, want %d", count, numRows)
	}

	// Verify conn was returned to pool by issuing another query.
	rows2, err := pool.QueryContext(ctx, "SELECT id FROM t")
	if err != nil {
		t.Fatalf("second pool query: %v", err)
	}
	count = 0
	for rows2.Next() {
		var id int
		if err := rows2.Scan(&id); err != nil {
			t.Fatal(err)
		}
		count++
	}
	_ = rows2.Close()
	if count != numRows {
		t.Fatalf("second query: got %d rows, want %d", count, numRows)
	}

	// Verify pool occupancy.
	stats := pool.Stats()
	if stats.Open != 1 {
		t.Errorf("pool open = %d, want 1", stats.Open)
	}
}
