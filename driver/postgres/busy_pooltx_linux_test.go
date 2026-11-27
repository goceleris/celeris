package postgres

// Linux only (the _linux_test.go suffix is the build constraint). These tests
// need a Pool on the event loop, where a result of 64 rows or more streams. On
// macOS and Windows a Pool opened WithEngine on a server that has not started
// dials direct conns (pool.go), which buffer the whole result: there is no
// streaming Rows, so nothing for ErrRowsOpen or the discard to act on. A skip is
// not coverage, so the tests are not built there at all.

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// busyLoopPool opens a native Pool against the fake backend and puts it on the loop
// path, as a Pool opened WithEngine on a server that has not started is.
func busyLoopPool(t *testing.T, addr string, maxOpen int) *Pool {
	t.Helper()
	host, port, _ := net.SplitHostPort(addr)
	p, err := Open("postgres://u@"+host+":"+port+"/db?sslmode=disable", WithMaxOpen(maxOpen))
	if err != nil {
		t.Fatal(err)
	}
	p.hasEngine = true
	p.asyncEngine = false
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// TestBusyPoolTxCommitOverOpenRows: Commit over a streaming Rows ends the Tx
// (the reconciliation with a Commit that always ends its Tx): nothing is sent, the
// conn is discarded, Commit returns ErrRowsOpen, every later call returns
// ErrTxDone (Rollback included), the open Rows ends with an error that matches
// ErrClosed and io.ErrUnexpectedEOF, and with one slot and no Rollback the next
// BeginTx does not block (the slot is released exactly once).
func TestBusyPoolTxCommitOverOpenRows(t *testing.T) {
	p := busyLoopPool(t, busyFakeServer(t), 1)
	streamed := 0
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		tx, err := p.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("iteration %d: BeginTx: %v (the slot of the previous Tx was not released)", i, err)
		}
		rows, err := tx.QueryContext(ctx, "fast:2000")
		if err != nil {
			t.Fatal(err)
		}
		if !rows.Next() {
			t.Fatalf("iteration %d: no first row: %v", i, rows.Err())
		}
		if tx.conn.streaming.Load() == nil {
			// The sync poll took the whole reply: no stream, Commit commits.
			if err := tx.Commit(); err != nil {
				t.Fatalf("iteration %d: Commit of a buffered result: %v", i, err)
			}
			_ = rows.Close()
			cancel()
			continue
		}
		streamed++
		cerr, _ := busyWatch(t, "Commit", 15*time.Second, func() error { return tx.Commit() })
		if !errors.Is(cerr, ErrRowsOpen) {
			t.Fatalf("iteration %d: Commit over a streaming Rows: %v, want ErrRowsOpen", i, cerr)
		}
		if rerr := tx.Rollback(); !errors.Is(rerr, ErrTxDone) {
			t.Fatalf("iteration %d: Rollback after the Commit: %v, want ErrTxDone", i, rerr)
		}
		if cerr2 := tx.Commit(); !errors.Is(cerr2, ErrTxDone) {
			t.Fatalf("iteration %d: second Commit: %v, want ErrTxDone", i, cerr2)
		}
		if _, xerr := tx.ExecContext(ctx, "SELECT 1"); !errors.Is(xerr, ErrTxDone) {
			t.Fatalf("iteration %d: Exec after the Commit: %v, want ErrTxDone", i, xerr)
		}
		busyWatch(t, "drain", 15*time.Second, func() int {
			for rows.Next() {
			}
			return 0
		})
		if rerr := rows.Err(); rerr == nil || rerr == io.EOF || !errors.Is(rerr, ErrClosed) || !errors.Is(rerr, io.ErrUnexpectedEOF) {
			t.Fatalf("iteration %d: the Rows ended with Err = %v, want an error matching ErrClosed and io.ErrUnexpectedEOF", i, rerr)
		}
		_ = rows.Close()
		cancel()
	}
	if streamed < 5 {
		t.Fatalf("only %d of 20 first statements streamed: the test does not exercise the discard", streamed)
	}
	if st := p.Stats(); st.InUse != 0 || st.Open > 1 {
		t.Fatalf("pool after the loop: %+v, want nothing in use and at most one open conn", st)
	}
}

// TestBusyPoolTxRollbackOverOpenRowsEndsTx: Rollback over a streaming Rows discards
// the conn, returns nil, ends the Tx (a later Commit returns ErrTxDone), and the
// open Rows ends with an error.
func TestBusyPoolTxRollbackOverOpenRowsEndsTx(t *testing.T) {
	p := busyLoopPool(t, busyFakeServer(t), 1)
	streamed := 0
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		tx, err := p.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("iteration %d: BeginTx: %v", i, err)
		}
		rows, err := tx.QueryContext(ctx, "fast:2000")
		if err != nil {
			t.Fatal(err)
		}
		if !rows.Next() {
			t.Fatalf("iteration %d: no first row: %v", i, rows.Err())
		}
		isStream := tx.conn.streaming.Load() != nil
		if rerr, _ := busyWatch(t, "Rollback", 15*time.Second, func() error { return tx.Rollback() }); rerr != nil {
			t.Fatalf("iteration %d: Rollback: %v, want nil", i, rerr)
		}
		if cerr := tx.Commit(); !errors.Is(cerr, ErrTxDone) {
			t.Fatalf("iteration %d: Commit after the Rollback: %v, want ErrTxDone", i, cerr)
		}
		if isStream {
			streamed++
			busyWatch(t, "drain", 15*time.Second, func() int {
				for rows.Next() {
				}
				return 0
			})
			if rerr := rows.Err(); rerr == nil || rerr == io.EOF || !errors.Is(rerr, ErrClosed) {
				t.Fatalf("iteration %d: the Rows ended with Err = %v, want an error", i, rerr)
			}
		}
		_ = rows.Close()
		cancel()
	}
	if streamed < 5 {
		t.Fatalf("only %d of 20 first statements streamed", streamed)
	}
}
