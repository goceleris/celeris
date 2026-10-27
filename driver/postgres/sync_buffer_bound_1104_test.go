package postgres

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// The sync fast path and direct mode buffer a whole reply (syncMode: the
// caller's goroutine is the dispatcher, so promoting to streaming would block
// it). maxDirectResultBytes is the bound on that buffer, and appendBuffered
// used to test it AFTER it appended the row and cannot stop the poll, so a reply
// above the bound was still buffered whole (about 5 times its wire size in heap:
// 1.1 to 1.2 GB for a 240 MB reply, measured) and ErrResultTooBig came at the
// end (celeris#1104). These tests pin that the buffer stops at the bound.

// TestAppendBufferedStopsAtTheCap1104 drives the request the way dispatch does,
// row after row, until well past the cap.
func TestAppendBufferedStopsAtTheCap1104(t *testing.T) {
	r := &pgRequest{}
	r.syncMode.Store(true)
	payload := make([]byte, 1<<10)
	row := [][]byte{payload}
	const rows = maxDirectResultBytes/(1<<10) + 20_000 // the cap, and 20 MiB more
	for range rows {
		r.OnRow(row)
	}
	if !errors.Is(r.err, ErrResultTooBig) {
		t.Fatalf("err = %v, want ErrResultTooBig", r.err)
	}
	if got := len(r.rowSlab); got > maxDirectResultBytes {
		t.Errorf("the buffer holds %d bytes of rows, over the %d cap: the cap is checked after the append", got, maxDirectResultBytes)
	}
	if got, max := len(r.rows), maxDirectResultBytes/(1<<10); got > max {
		t.Errorf("%d rows buffered, want at most %d (the cap)", got, max)
	}
}

// TestBufferedRowsLiveHeapIsTheData1104: the rows of a buffered reply hold about
// the data, not several generations of it. The slab and the row fields grew by
// copy, and the row views already handed out kept every earlier array alive:
// with 1.25x growth, 5 times the data (celeris#1104).
func TestBufferedRowsLiveHeapIsTheData1104(t *testing.T) {
	live := func() uint64 {
		runtime.GC()
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	const rows, size = 400_000, 64 // 25.6 MB of payload
	row := [][]byte{make([]byte, size)}
	before := live()
	r := &pgRequest{}
	r.syncMode.Store(true)
	for range rows {
		r.OnRow(row)
	}
	after := live()
	runtime.KeepAlive(r)
	data := uint64(rows * size)
	// The data, 24 bytes of field view and 24 of row view per row: 112 bytes a
	// row, 1.75 times the payload; allow 2.5.
	if grown := after - before; grown > data*5/2 {
		t.Errorf("%d rows of %d bytes (%d MB of data) hold %.1f MB of live heap, want under 2.5 times the data", rows, size, data>>20, float64(grown)/(1<<20))
	}
}

// TestAppendBufferedKeepsReplyOrderAfterTheCap1104: a row that does not fit fails
// the request, and a smaller row after it that would fit is not buffered either:
// the result would have a hole where the big one was.
func TestAppendBufferedKeepsReplyOrderAfterTheCap1104(t *testing.T) {
	r := &pgRequest{}
	r.syncMode.Store(true)
	for range maxDirectResultBytes/(1<<10) - 1 {
		r.OnRow([][]byte{make([]byte, 1<<10)})
	}
	if r.err != nil {
		t.Fatalf("err = %v before the cap", r.err)
	}
	r.OnRow([][]byte{make([]byte, 2<<10)}) // does not fit
	if !errors.Is(r.err, ErrResultTooBig) {
		t.Fatalf("err = %v, want ErrResultTooBig", r.err)
	}
	before := len(r.rows)
	r.OnRow([][]byte{make([]byte, 512)}) // would fit
	if len(r.rows) != before {
		t.Errorf("a row that fits after the failed one was buffered (%d rows, was %d)", len(r.rows), before)
	}
}

// TestAppendBufferedKeepsAResultUnderTheCap1104: a reply that fits is buffered
// whole, as before.
func TestAppendBufferedKeepsAResultUnderTheCap1104(t *testing.T) {
	r := &pgRequest{}
	r.syncMode.Store(true)
	row := [][]byte{make([]byte, 1<<10)}
	const rows = maxDirectResultBytes / (1 << 10) // exactly the cap
	for range rows {
		r.OnRow(row)
	}
	if r.err != nil || len(r.rows) != rows {
		t.Fatalf("a reply of exactly the cap: err = %v, %d of %d rows", r.err, len(r.rows), rows)
	}
}

// TestDirectModeResultAboveTheCapIsBounded1104 runs it end to end in direct mode,
// where the whole reply is read on the caller's goroutine whatever the timing:
// the fake backend sends a reply over the cap in one go, and what the caller can
// read is at most the cap's worth of rows, then ErrResultTooBig.
func TestDirectModeResultAboveTheCapIsBounded1104(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rows    int
		wantErr error
	}{
		{"under the cap", maxDirectResultBytes/(1<<10) - 4096, nil},
		{"over the cap", maxDirectResultBytes/(1<<10) + 20_000, ErrResultTooBig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := make([]byte, 1<<10)
			for i := range payload {
				payload[i] = 'x'
			}
			addr := startFakePG(t, func(c net.Conn) {
				fakePGTrustStartup(t, c, 1, 2, func(c net.Conn) {
					for {
						typ, _, err := readMsg(c)
						if err != nil || typ == protocol.MsgTerminate {
							return
						}
						if typ != protocol.MsgQuery {
							continue
						}
						_ = writeMsg(c, protocol.BackendRowDescription, buildRowDescription([]colSpec{
							{name: "payload", typeOID: protocol.OIDText, typeSize: -1, format: protocol.FormatText},
						}))
						dr := buildDataRow([][]byte{payload})
						for range tc.rows {
							if writeMsg(c, protocol.BackendDataRow, dr) != nil {
								return
							}
						}
						_ = writeCommandComplete(c, fmt.Sprintf("SELECT %d", tc.rows))
						_ = writeReadyForQuery(c, 'I')
					}
				})
			})
			host, port, _ := net.SplitHostPort(addr)
			dsn := DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable", StatementCacheSize: 16}}
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			conn, err := dialDirectConn(ctx, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			rows, err := conn.QueryContext(ctx, "SELECT payload FROM t", nil)
			if err != nil && !errors.Is(err, ErrResultTooBig) {
				t.Fatal(err)
			}
			got := 0
			var rerr error
			if rows != nil {
				dest := make([]driver.Value, 1)
				for {
					if rerr = rows.Next(dest); rerr != nil {
						break
					}
					got++
				}
				_ = rows.Close()
			} else {
				rerr = err
			}
			if rerr == io.EOF {
				rerr = nil
			}
			if !errors.Is(rerr, tc.wantErr) && !(tc.wantErr == nil && rerr == nil) {
				t.Fatalf("after %d rows: err = %v, want %v", got, rerr, tc.wantErr)
			}
			if tc.wantErr == nil {
				if got != tc.rows {
					t.Fatalf("got %d rows, want %d", got, tc.rows)
				}
				return
			}
			if max := maxDirectResultBytes / (1 << 10); got > max {
				t.Errorf("read %d rows of a reply over the cap, want at most %d: the reply was buffered whole", got, max)
			}
		})
	}
}
