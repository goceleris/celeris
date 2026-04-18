//go:build postgres

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/postgres"
)

// TestSyntaxError confirms a plain syntax error surfaces as *postgres.PGError
// (aliased to *protocol.PGError) with SQLSTATE 42601.
func TestSyntaxError(t *testing.T) {
	db := openDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := db.ExecContext(ctx, "SELECT FROM WHERE -- nonsense")
	if err == nil {
		t.Fatalf("expected syntax error, got nil")
	}
	var pge *postgres.PGError
	if !errors.As(err, &pge) {
		t.Fatalf("err type = %T, want *postgres.PGError", err)
	}
	if pge.Code != "42601" {
		t.Fatalf("SQLSTATE = %q, want 42601", pge.Code)
	}
}

// TestUniqueViolation exercises a constraint violation path.
func TestUniqueViolation(t *testing.T) {
	db := openDB(t)
	tbl := uniqueTableName(t, "unique")
	createTable(t, db, tbl, "id int primary key")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := db.ExecContext(ctx, "INSERT INTO "+tbl+" VALUES (1)"); err != nil {
		t.Fatalf("first INSERT: %v", err)
	}
	_, err := db.ExecContext(ctx, "INSERT INTO "+tbl+" VALUES (1)")
	if err == nil {
		t.Fatalf("expected unique_violation, got nil")
	}
	var pge *postgres.PGError
	if !errors.As(err, &pge) {
		t.Fatalf("err type = %T, want *postgres.PGError", err)
	}
	if pge.Code != "23505" {
		t.Fatalf("SQLSTATE = %q, want 23505 (unique_violation)", pge.Code)
	}
}

// TestWrongTypeOp triggers 42883 (operator does not exist) or 22P02 (invalid
// text representation) depending on how pg parses the expression.
func TestWrongTypeOp(t *testing.T) {
	db := openDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := db.ExecContext(ctx, "SELECT 1 + 'not a number'::int")
	if err == nil {
		t.Fatalf("expected type error, got nil")
	}
	var pge *postgres.PGError
	if !errors.As(err, &pge) {
		t.Fatalf("err type = %T, want *postgres.PGError", err)
	}
	// 22P02 = invalid_text_representation. Accept anything in the 22xxx data
	// exception class to be server-version-tolerant.
	if len(pge.Code) != 5 || pge.Code[:2] != "22" {
		t.Fatalf("SQLSTATE = %q, want 22xxx data exception", pge.Code)
	}
}

// TestConstraintCheck exercises a CHECK violation (SQLSTATE 23514).
func TestConstraintCheck(t *testing.T) {
	db := openDB(t)
	tbl := uniqueTableName(t, "chk")
	createTable(t, db, tbl, "id int primary key, n int CHECK (n > 0)")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := db.ExecContext(ctx, "INSERT INTO "+tbl+" VALUES (1, -1)")
	if err == nil {
		t.Fatalf("expected CHECK violation, got nil")
	}
	var pge *postgres.PGError
	if !errors.As(err, &pge) {
		t.Fatalf("err type = %T, want *postgres.PGError", err)
	}
	if pge.Code != "23514" {
		t.Fatalf("SQLSTATE = %q, want 23514 (check_violation)", pge.Code)
	}
}
