//go:build postgres

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/postgres"
)

// TestCommitAndRollback verifies BEGIN + INSERT + COMMIT persists, and
// BEGIN + INSERT + ROLLBACK does not.
func TestCommitAndRollback(t *testing.T) {
	db := openDB(t)
	tbl := uniqueTableName(t, "tx")
	createTable(t, db, tbl, "id int primary key")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO "+tbl+" VALUES (1)"); err != nil {
		t.Fatalf("INSERT committed: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx 2: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO "+tbl+" VALUES (2)"); err != nil {
		t.Fatalf("INSERT to-rollback: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tbl).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("got %d rows, want 1 (only the committed one)", n)
	}
}

// TestIsolationLevels just walks through the driver.TxOptions isolation
// settings and checks that BEGIN succeeds — correctness of isolation itself is
// a server concern, not a driver one.
func TestIsolationLevels(t *testing.T) {
	db := openDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	levels := []sql.IsolationLevel{
		sql.LevelDefault,
		sql.LevelReadCommitted,
		sql.LevelRepeatableRead,
		sql.LevelSerializable,
	}
	for _, lvl := range levels {
		t.Run(lvl.String(), func(t *testing.T) {
			tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: lvl})
			if err != nil {
				t.Fatalf("BeginTx: %v", err)
			}
			var one int
			if err := tx.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
				t.Fatalf("SELECT 1: %v", err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("Commit: %v", err)
			}
		})
	}
}

// TestReadOnlyTx verifies the ReadOnly flag surfaces as a server error on
// writes.
func TestReadOnlyTx(t *testing.T) {
	db := openDB(t)
	tbl := uniqueTableName(t, "readonly")
	createTable(t, db, tbl, "id int primary key")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, "INSERT INTO "+tbl+" VALUES (1)")
	if err == nil {
		t.Fatalf("expected write-in-read-only-tx to fail, got nil")
	}
}

// TestSavepointsViaConnRaw reaches savepoint methods through the sql.Conn.Raw
// path documented in the driver — these are not in the standard database/sql
// API surface.
func TestSavepointsViaConnRaw(t *testing.T) {
	db := openDB(t)
	tbl := uniqueTableName(t, "sp")
	createTable(t, db, tbl, "id int primary key")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}

	if _, err := conn.ExecContext(ctx, "INSERT INTO "+tbl+" VALUES (1)"); err != nil {
		t.Fatalf("INSERT outer: %v", err)
	}

	// Savepoint via Raw.
	err = conn.Raw(func(raw any) error {
		pc, ok := raw.(interface {
			Savepoint(context.Context, string) error
			RollbackToSavepoint(context.Context, string) error
			ReleaseSavepoint(context.Context, string) error
		})
		if !ok {
			return errors.New("raw conn does not implement savepoint API")
		}
		if err := pc.Savepoint(ctx, "sp1"); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Savepoint: %v", err)
	}

	if _, err := conn.ExecContext(ctx, "INSERT INTO "+tbl+" VALUES (2)"); err != nil {
		t.Fatalf("INSERT inner: %v", err)
	}

	err = conn.Raw(func(raw any) error {
		pc := raw.(interface {
			RollbackToSavepoint(context.Context, string) error
		})
		return pc.RollbackToSavepoint(ctx, "sp1")
	})
	if err != nil {
		t.Fatalf("RollbackToSavepoint: %v", err)
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		t.Fatalf("COMMIT: %v", err)
	}

	var n int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tbl).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("got %d rows, want 1 (inner insert should be rolled back)", n)
	}
}

// TestPoolTx exercises the non-database/sql Pool.BeginTx path.
func TestPoolTx(t *testing.T) {
	dsn := dsnFromEnv(t)
	p, err := postgres.Open(dsn)
	if err != nil {
		t.Fatalf("postgres.Open: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tbl := uniqueTableName(t, "pooltx")
	if _, err := p.ExecContext(ctx, "CREATE TABLE "+tbl+" (id int primary key)"); err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = p.ExecContext(ctx, "DROP TABLE IF EXISTS "+tbl)
	})

	tx, err := p.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO "+tbl+" VALUES (1)"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("INSERT: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}
