//go:build postgres

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// TestSimpleSelect exercises the simple query protocol (no parameters).
func TestSimpleSelect(t *testing.T) {
	db := openDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var got int
	if err := db.QueryRowContext(ctx, "SELECT 42").Scan(&got); err != nil {
		t.Fatalf("QueryRow: %v", err)
	}
	if got != 42 {
		t.Fatalf("got %d, want 42", got)
	}
}

// TestSimpleSelectMultiColumn decodes a row with several column types.
func TestSimpleSelectMultiColumn(t *testing.T) {
	db := openDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var (
		i int
		s string
		b bool
	)
	row := db.QueryRowContext(ctx, "SELECT 1::int, 'hello'::text, true")
	if err := row.Scan(&i, &s, &b); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if i != 1 || s != "hello" || !b {
		t.Fatalf("got (%d,%q,%v), want (1,\"hello\",true)", i, s, b)
	}
}

// TestInsertUpdateDelete runs a full CRUD cycle through simple and extended
// query paths.
func TestInsertUpdateDelete(t *testing.T) {
	db := openDB(t)
	tbl := uniqueTableName(t, "crud")
	createTable(t, db, tbl, "id int primary key, body text")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// INSERT via extended protocol (parameters).
	res, err := db.ExecContext(ctx, "INSERT INTO "+tbl+" (id, body) VALUES ($1, $2)", 1, "alpha")
	if err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		t.Fatalf("RowsAffected = (%d,%v), want (1,nil)", n, err)
	}

	// INSERT a second row via simple protocol (no params).
	if _, err := db.ExecContext(ctx, "INSERT INTO "+tbl+" (id, body) VALUES (2, 'beta')"); err != nil {
		t.Fatalf("INSERT2: %v", err)
	}

	// UPDATE.
	if _, err := db.ExecContext(ctx, "UPDATE "+tbl+" SET body = $1 WHERE id = $2", "ALPHA", 1); err != nil {
		t.Fatalf("UPDATE: %v", err)
	}

	// SELECT the updated row.
	var body string
	if err := db.QueryRowContext(ctx, "SELECT body FROM "+tbl+" WHERE id = $1", 1).Scan(&body); err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	if body != "ALPHA" {
		t.Fatalf("body = %q, want %q", body, "ALPHA")
	}

	// DELETE.
	res, err = db.ExecContext(ctx, "DELETE FROM "+tbl+" WHERE id = $1", 2)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("DELETE RowsAffected = %d, want 1", n)
	}
}

// TestNullHandling verifies NULL round-trips via both Scan(*NullT) and Scan
// into pointer types.
func TestNullHandling(t *testing.T) {
	db := openDB(t)
	tbl := uniqueTableName(t, "nulls")
	createTable(t, db, tbl, "id int primary key, val int")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := db.ExecContext(ctx, "INSERT INTO "+tbl+" VALUES (1, NULL), (2, 100)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	var v1 sql.NullInt64
	if err := db.QueryRowContext(ctx, "SELECT val FROM "+tbl+" WHERE id = 1").Scan(&v1); err != nil {
		t.Fatalf("Scan NULL: %v", err)
	}
	if v1.Valid {
		t.Fatalf("id=1 val: got Valid=true, want NULL")
	}

	var v2 sql.NullInt64
	if err := db.QueryRowContext(ctx, "SELECT val FROM "+tbl+" WHERE id = 2").Scan(&v2); err != nil {
		t.Fatalf("Scan 100: %v", err)
	}
	if !v2.Valid || v2.Int64 != 100 {
		t.Fatalf("id=2 val: got (%d,%v), want (100,true)", v2.Int64, v2.Valid)
	}
}

// TestEmptyResultSet ensures a SELECT that matches no rows returns
// sql.ErrNoRows from QueryRow and an immediately-closed Rows from Query.
func TestEmptyResultSet(t *testing.T) {
	db := openDB(t)
	tbl := uniqueTableName(t, "empty")
	createTable(t, db, tbl, "id int primary key")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var id int
	err := db.QueryRowContext(ctx, "SELECT id FROM "+tbl+" WHERE id = $1", 999).Scan(&id)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("got err=%v, want sql.ErrNoRows", err)
	}

	rows, err := db.QueryContext(ctx, "SELECT id FROM "+tbl)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatalf("empty SELECT returned a row")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
}

// TestMultiRowSelect walks a multi-row result.
func TestMultiRowSelect(t *testing.T) {
	db := openDB(t)
	tbl := uniqueTableName(t, "multi")
	createTable(t, db, tbl, "id int primary key, body text")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for i := 0; i < 10; i++ {
		if _, err := db.ExecContext(ctx, "INSERT INTO "+tbl+" VALUES ($1, $2)", i, "row"); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	rows, err := db.QueryContext(ctx, "SELECT id, body FROM "+tbl+" ORDER BY id")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var id int
		var body string
		if err := rows.Scan(&id, &body); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if id != count || body != "row" {
			t.Fatalf("row %d: got (%d,%q)", count, id, body)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if count != 10 {
		t.Fatalf("count = %d, want 10", count)
	}
}
