//go:build postgres

package postgres_test

import (
	"context"
	"testing"
	"time"
)

// TestPreparedStatementReuse prepares a statement once and executes it many
// times, relying on the driver's per-conn LRU cache to avoid re-Parse.
func TestPreparedStatementReuse(t *testing.T) {
	db := openDB(t)
	tbl := uniqueTableName(t, "prep")
	createTable(t, db, tbl, "id int primary key, body text")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stmt, err := db.PrepareContext(ctx, "INSERT INTO "+tbl+" (id, body) VALUES ($1, $2)")
	if err != nil {
		t.Fatalf("Prepare INSERT: %v", err)
	}
	defer stmt.Close()

	for i := 0; i < 50; i++ {
		if _, err := stmt.ExecContext(ctx, i, "row"); err != nil {
			t.Fatalf("Exec[%d]: %v", i, err)
		}
	}

	q, err := db.PrepareContext(ctx, "SELECT COUNT(*) FROM "+tbl)
	if err != nil {
		t.Fatalf("Prepare SELECT: %v", err)
	}
	defer q.Close()

	var n int
	if err := q.QueryRowContext(ctx).Scan(&n); err != nil {
		t.Fatalf("QueryRow: %v", err)
	}
	if n != 50 {
		t.Fatalf("count = %d, want 50", n)
	}
}

// TestParameterBinding exercises many Go scalar types as $N parameters.
func TestParameterBinding(t *testing.T) {
	db := openDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var (
		i  int64
		f  float64
		s  string
		b  bool
		bb []byte
	)
	row := db.QueryRowContext(ctx,
		"SELECT $1::bigint, $2::float8, $3::text, $4::bool, $5::bytea",
		int64(42), float64(3.14), "hello", true, []byte{0x01, 0x02, 0x03},
	)
	if err := row.Scan(&i, &f, &s, &b, &bb); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if i != 42 || f < 3.13 || f > 3.15 || s != "hello" || !b || len(bb) != 3 || bb[0] != 1 || bb[1] != 2 || bb[2] != 3 {
		t.Fatalf("got (%d,%f,%q,%v,%v)", i, f, s, b, bb)
	}
}

// TestLimitClause validates that LIMIT works end-to-end — the driver doesn't
// translate it, but parsing many-row results is worth an explicit test.
func TestLimitClause(t *testing.T) {
	db := openDB(t)
	tbl := uniqueTableName(t, "limit")
	createTable(t, db, tbl, "id int primary key")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for i := 0; i < 20; i++ {
		if _, err := db.ExecContext(ctx, "INSERT INTO "+tbl+" VALUES ($1)", i); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	rows, err := db.QueryContext(ctx, "SELECT id FROM "+tbl+" ORDER BY id LIMIT $1", 5)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		count++
	}
	if count != 5 {
		t.Fatalf("count = %d, want 5", count)
	}
}

// TestRepeatedPrepareSameQuery confirms that asking the driver for the same
// query twice hits the LRU cache and returns a functional Stmt either way.
func TestRepeatedPrepareSameQuery(t *testing.T) {
	db := openDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const q = "SELECT $1::int + $2::int"
	for i := 0; i < 10; i++ {
		stmt, err := db.PrepareContext(ctx, q)
		if err != nil {
			t.Fatalf("Prepare[%d]: %v", i, err)
		}
		var got int
		if err := stmt.QueryRowContext(ctx, 3, 4).Scan(&got); err != nil {
			t.Fatalf("Scan[%d]: %v", i, err)
		}
		if got != 7 {
			t.Fatalf("iter %d: got %d, want 7", i, got)
		}
		_ = stmt.Close()
	}
}
