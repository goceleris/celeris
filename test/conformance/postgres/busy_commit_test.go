//go:build postgres

package postgres_test

// Commit over a streaming Rows that is still open ends the native Tx (every Commit
// does): nothing is sent, the connection is discarded and the transaction is rolled
// back, Commit returns ErrRowsOpen so that the caller sees why, every later call on
// the Tx returns ErrTxDone (Rollback included), the open Rows ends with an error,
// and the pool slot is released exactly once. In direct mode (no stream) Commit
// commits normally.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/postgres"
)

func TestBusyConnNativeCommitOverOpenRows(t *testing.T) {
	dsn := dsnFromEnv(t)
	p, err := openPoolEngine(dsn, postgres.WithMaxOpen(3))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	ctx := context.Background()
	tbl := uniqueTableName(t, "busycommit")
	if _, err := p.ExecContext(ctx, "CREATE TABLE "+tbl+" (id int)"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = p.ExecContext(ctx, "DROP TABLE IF EXISTS "+tbl) })
	loop := busyPoolOnLoop()

	tx, err := p.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO "+tbl+" VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.QueryContext(ctx, busyStreamQ)
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatalf("no first row: %v", rows.Err())
	}
	cerr, _ := busyWatch(t, "Commit over an open Rows", 30*time.Second, func() error { return tx.Commit() })

	count := func() int {
		var n int
		if err := p.QueryRow(ctx, "SELECT count(*) FROM "+tbl).Scan(&n); err != nil {
			t.Fatalf("count from a second session: %v", err)
		}
		return n
	}
	if !loop {
		// Direct mode buffered the result: no stream, so Commit commits normally.
		if cerr != nil {
			t.Fatalf("direct mode: Commit with the first Rows open: %v, want nil", cerr)
		}
		_ = rows.Close()
		if n := count(); n != 1 {
			t.Fatalf("direct mode: a second session sees %d rows, want the committed 1", n)
		}
		return
	}
	if !errors.Is(cerr, postgres.ErrRowsOpen) {
		t.Fatalf("Commit over a streaming Rows: err = %v, want ErrRowsOpen", cerr)
	}
	// The Tx is done: every later call returns ErrTxDone, Rollback included.
	if rerr := tx.Rollback(); !errors.Is(rerr, postgres.ErrTxDone) || !errors.Is(rerr, sql.ErrTxDone) {
		t.Fatalf("Rollback after the Commit: %v, want ErrTxDone", rerr)
	}
	if cerr2 := tx.Commit(); !errors.Is(cerr2, postgres.ErrTxDone) {
		t.Fatalf("second Commit: %v, want ErrTxDone", cerr2)
	}
	if _, xerr := tx.ExecContext(ctx, "SELECT 1"); !errors.Is(xerr, postgres.ErrTxDone) {
		t.Fatalf("ExecContext after the Commit: %v, want ErrTxDone", xerr)
	}
	// The open Rows ends with an error, never with a clean end.
	n := 1
	_, _ = busyWatch(t, "drain after the Commit", 30*time.Second, func() int {
		for rows.Next() {
			n++
		}
		return n
	})
	if rerr := rows.Err(); rerr == nil || rerr == io.EOF || !errors.Is(rerr, postgres.ErrClosed) || !errors.Is(rerr, io.ErrUnexpectedEOF) {
		t.Fatalf("the Rows ended after %d rows with Err = %v, want an error matching ErrClosed and io.ErrUnexpectedEOF", n, rerr)
	}
	_ = rows.Close()
	// The transaction was rolled back: a second session sees none of its writes.
	if got := count(); got != 0 {
		t.Fatalf("a second session sees %d rows of the transaction that Commit ended over an open Rows, want 0", got)
	}
}

// With one slot and no Rollback call, Commit over an open Rows frees the slot
// exactly once: the next BeginTx does not block, and the pool does not grow.
func TestBusyConnNativeCommitOverOpenRowsSlot(t *testing.T) {
	dsn := dsnFromEnv(t)
	p, err := openPoolEngine(dsn, postgres.WithMaxOpen(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	loop := busyPoolOnLoop()
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err, _ := busyWatch(t, fmt.Sprint("iteration ", i), 30*time.Second, func() error {
			tx, err := p.BeginTx(ctx, nil)
			if err != nil {
				return fmt.Errorf("BeginTx: %w (the slot of the previous Tx was not released)", err)
			}
			rows, err := tx.QueryContext(ctx, busyStreamQ)
			if err != nil {
				return err
			}
			if !rows.Next() {
				return fmt.Errorf("no first row: %v", rows.Err())
			}
			cerr := tx.Commit() // and no Rollback
			if loop && !errors.Is(cerr, postgres.ErrRowsOpen) {
				return fmt.Errorf("Commit over a streaming Rows: %v, want ErrRowsOpen", cerr)
			}
			if !loop && cerr != nil {
				return fmt.Errorf("direct mode Commit: %v", cerr)
			}
			_ = rows.Close()
			return nil
		})
		cancel()
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
	}
	if st := p.Stats(); st.InUse != 0 || st.Open > 1 {
		t.Fatalf("pool after the loop: %+v, want nothing in use and at most one open conn", st)
	}
}

// Rollback over an open stream still discards the conn and ends the Tx.
func TestBusyConnNativeRollbackOverOpenRowsEndsTx(t *testing.T) {
	dsn := dsnFromEnv(t)
	p, err := openPoolEngine(dsn, postgres.WithMaxOpen(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	ctx := context.Background()
	tx, err := p.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := tx.QueryContext(ctx, busyStreamQ)
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatalf("no first row: %v", rows.Err())
	}
	if rerr, _ := busyWatch(t, "Rollback", 30*time.Second, func() error { return tx.Rollback() }); rerr != nil {
		t.Fatalf("Rollback over an open Rows: %v, want nil", rerr)
	}
	if cerr := tx.Commit(); !errors.Is(cerr, postgres.ErrTxDone) {
		t.Fatalf("Commit after the Rollback: %v, want ErrTxDone", cerr)
	}
	_ = rows.Close()
	bctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx2, err := p.BeginTx(bctx, nil)
	if err != nil {
		t.Fatalf("BeginTx after the Rollback: %v (slot not released)", err)
	}
	_ = tx2.Rollback()
}
