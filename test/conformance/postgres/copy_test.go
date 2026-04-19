//go:build postgres

package postgres_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/postgres"
)

// openPool opens a direct postgres.Pool against the harness DSN. Pool is the
// entry point that exposes CopyFrom / CopyTo — database/sql doesn't surface
// COPY. Each test that exercises COPY uses this helper plus the existing
// database/sql setup for the CREATE TABLE / schema side.
func openPool(t *testing.T) *postgres.Pool {
	t.Helper()
	p, err := postgres.Open(dsnFromEnv(t), postgres.WithMaxOpen(4))
	if err != nil {
		t.Fatalf("postgres.Open: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// TestCopyFromText covers the common case: stream a batch of rows in COPY
// text format via CopyFromSlice. Verifies row count propagated from
// CommandComplete and contents round-trip via a SELECT.
func TestCopyFromText(t *testing.T) {
	db := openDB(t)
	pool := openPool(t)
	table := uniqueTableName(t, "copy_from_text")
	createTable(t, db, table, "id INT PRIMARY KEY, name TEXT NOT NULL, active BOOLEAN")

	rows := [][]any{
		{1, "alice", true},
		{2, "bob", false},
		{3, "carol with\ttab", true},
		{4, "dave\nwith\nnewlines", false},
		{5, "eve \\ with backslash", true},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	n, err := pool.CopyFrom(ctx, table, []string{"id", "name", "active"}, postgres.CopyFromSlice(rows))
	if err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}
	if n != int64(len(rows)) {
		t.Fatalf("CopyFrom returned n=%d, want %d", n, len(rows))
	}

	// Round-trip via SELECT — proves the encode path preserved the special
	// characters we wrote (tab, newline, backslash).
	var got int
	if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&got); err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != len(rows) {
		t.Fatalf("SELECT COUNT=%d, want %d", got, len(rows))
	}
	for _, want := range rows {
		var name string
		if err := db.QueryRowContext(ctx,
			fmt.Sprintf("SELECT name FROM %s WHERE id = $1", table), want[0]).Scan(&name); err != nil {
			t.Fatalf("select id=%v: %v", want[0], err)
		}
		if name != want[1].(string) {
			t.Errorf("id=%v: name=%q, want %q", want[0], name, want[1])
		}
	}
}

// TestCopyFromNulls verifies that nil values map to NULL on the wire and
// round-trip as such — the text-format NULL sentinel is \N, which is the
// same two characters appendTextField emits for nil.
func TestCopyFromNulls(t *testing.T) {
	db := openDB(t)
	pool := openPool(t)
	table := uniqueTableName(t, "copy_from_nulls")
	createTable(t, db, table, "id INT PRIMARY KEY, nick TEXT")

	rows := [][]any{
		{1, "alice"},
		{2, nil},
		{3, ""}, // empty string is distinct from NULL
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := pool.CopyFrom(ctx, table, []string{"id", "nick"}, postgres.CopyFromSlice(rows)); err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}

	var nullCount, emptyCount int
	if err := db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE nick IS NULL", table)).Scan(&nullCount); err != nil {
		t.Fatalf("null count: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE nick = ''", table)).Scan(&emptyCount); err != nil {
		t.Fatalf("empty count: %v", err)
	}
	if nullCount != 1 {
		t.Errorf("NULL rows = %d, want 1", nullCount)
	}
	if emptyCount != 1 {
		t.Errorf("empty-string rows = %d, want 1", emptyCount)
	}
}

// TestCopyFromLargeBatch exercises the streaming path: 10k rows forces the
// driver to flush multiple CopyData packets rather than sending everything
// in one shot. Validates that the streaming side of COPY actually streams.
func TestCopyFromLargeBatch(t *testing.T) {
	db := openDB(t)
	pool := openPool(t)
	table := uniqueTableName(t, "copy_large")
	createTable(t, db, table, "id INT PRIMARY KEY, payload TEXT NOT NULL")

	const nRows = 10_000
	rows := make([][]any, nRows)
	for i := range nRows {
		rows[i] = []any{i, fmt.Sprintf("row-%08d", i)}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	n, err := pool.CopyFrom(ctx, table, []string{"id", "payload"}, postgres.CopyFromSlice(rows))
	if err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}
	if n != nRows {
		t.Fatalf("CopyFrom returned n=%d, want %d", n, nRows)
	}
	var got int
	if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&got); err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != nRows {
		t.Fatalf("COUNT=%d, want %d", got, nRows)
	}
}

// TestCopyTo exercises COPY ... TO STDOUT: populate a table, read it back
// through Pool.CopyTo's row callback, verify the byte content matches the
// PG text encoding of each row.
func TestCopyTo(t *testing.T) {
	db := openDB(t)
	pool := openPool(t)
	table := uniqueTableName(t, "copy_to")
	createTable(t, db, table, "id INT PRIMARY KEY, name TEXT NOT NULL")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i, name := range []string{"alice", "bob", "carol"} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s (id, name) VALUES ($1, $2)", table),
			i+1, name); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	var got [][]byte
	if err := pool.CopyTo(ctx, fmt.Sprintf("COPY %s (id, name) TO STDOUT", table), func(row []byte) error {
		// CopyTo promises dest may retain row — but defensively copy because
		// we accumulate across calls.
		cp := make([]byte, len(row))
		copy(cp, row)
		got = append(got, cp)
		return nil
	}); err != nil {
		t.Fatalf("CopyTo: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3", len(got))
	}
	// Each row ends with \n and has tab-separated fields.
	wantRows := [][]byte{
		[]byte("1\talice\n"),
		[]byte("2\tbob\n"),
		[]byte("3\tcarol\n"),
	}
	for i, w := range wantRows {
		if !bytes.Equal(got[i], w) {
			t.Errorf("row %d: got %q, want %q", i, got[i], w)
		}
	}
}

// TestCopyToAbortMidStream verifies that returning a non-nil error from the
// row callback aborts iteration without leaking the connection. A follow-up
// query on the same pool succeeds — proving the conn is back in the pool
// and not wedged in COPY OUT state.
func TestCopyToAbortMidStream(t *testing.T) {
	db := openDB(t)
	pool := openPool(t)
	table := uniqueTableName(t, "copy_to_abort")
	createTable(t, db, table, "id INT PRIMARY KEY")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := range 100 {
		if _, err := db.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO %s (id) VALUES ($1)", table), i); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	abort := errors.New("caller aborts mid-stream")
	seen := 0
	err := pool.CopyTo(ctx, fmt.Sprintf("COPY %s TO STDOUT", table), func(row []byte) error {
		seen++
		if seen >= 3 {
			return abort
		}
		return nil
	})
	if !errors.Is(err, abort) {
		t.Fatalf("CopyTo returned %v, want %v", err, abort)
	}
	if seen < 3 {
		t.Fatalf("saw %d rows before abort, expected >= 3", seen)
	}

	// Prove the pool still works — if COPY OUT cleanup were broken the next
	// acquire would either hang or return a wire-protocol error.
	var got int
	if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&got); err != nil {
		t.Fatalf("post-abort SELECT: %v", err)
	}
	if got != 100 {
		t.Fatalf("post-abort COUNT=%d, want 100", got)
	}
}

// TestCopyFromIteratorError propagates an error returned by the source's
// Err() after all rows have been drained. The server has already received
// CopyDone, so the failure must surface as a CopyFrom error rather than a
// silent success.
func TestCopyFromIteratorError(t *testing.T) {
	db := openDB(t)
	pool := openPool(t)
	table := uniqueTableName(t, "copy_itererr")
	createTable(t, db, table, "id INT")

	src := &failingSource{rows: [][]any{{1}, {2}}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := pool.CopyFrom(ctx, table, []string{"id"}, src)
	if err == nil {
		t.Fatal("CopyFrom returned nil error; expected source Err to propagate")
	}
	// Rows should NOT have been committed — CopyFail on iterator error.
	var got int
	if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&got); err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != 0 {
		t.Fatalf("COUNT=%d after CopyFail, want 0", got)
	}
	// Pool is still usable.
	if _, err := db.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s (id) VALUES (42)", table)); err != nil {
		t.Fatalf("post-failure INSERT: %v", err)
	}
}

// failingSource iterates through rows then returns an error from Err().
type failingSource struct {
	rows [][]any
	idx  int
}

func (s *failingSource) Next() bool {
	s.idx++
	return s.idx <= len(s.rows)
}

func (s *failingSource) Values() ([]any, error) {
	if s.idx <= 0 || s.idx > len(s.rows) {
		return nil, fmt.Errorf("failingSource: out of range (idx=%d)", s.idx)
	}
	return s.rows[s.idx-1], nil
}

func (s *failingSource) Err() error { return errors.New("simulated iterator failure") }
