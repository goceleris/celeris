//go:build postgres

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/driver/postgres"
)

// COMMIT of a transaction in the aborted state is answered with the command tag
// ROLLBACK and no error; Commit must say so (ErrTxCommitRollback), on every route
// (database/sql, the Pool in direct mode and with the driver's own loop), nothing
// may be committed, the conn must be free again, and a healthy transaction must
// still commit. Run against PostgreSQL directly and through PgBouncer.
func TestCommitOfAnAbortedTransactionReportsTheRollback(t *testing.T) {
	dsn := dsnFromEnv(t)
	table := fmt.Sprintf("tx_commit_rollback_%d", time.Now().UnixNano())

	db, err := sql.Open("celeris-postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "CREATE TABLE "+table+"(id int)"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.ExecContext(ctx, "DROP TABLE "+table) })
	count := func() int {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	type txn struct {
		exec   func(q string) error
		commit func() error
		next   func() error
	}
	arms := map[string]func(t *testing.T) txn{
		"database-sql": func(t *testing.T) txn {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			return txn{
				func(q string) error { _, err := tx.ExecContext(ctx, q); return err },
				tx.Commit,
				func() error { _, err := db.ExecContext(ctx, "SELECT 1"); return err },
			}
		},
	}
	for _, mode := range []string{"pool-direct", "pool-loop"} {
		opts := []postgres.Option{postgres.WithMaxOpen(1)}
		if mode == "pool-loop" {
			opts = append(opts, postgres.WithEngine(celeris.New(celeris.Config{Addr: "127.0.0.1:0"})))
		}
		p, err := postgres.Open(dsn, opts...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = p.Close() })
		arms[mode] = func(t *testing.T) txn {
			tx, err := p.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			return txn{
				func(q string) error { _, err := tx.ExecContext(ctx, q); return err },
				tx.Commit,
				func() error { _, err := p.ExecContext(ctx, "SELECT 1"); return err },
			}
		}
	}

	for name, begin := range arms {
		t.Run(name+"/aborted", func(t *testing.T) {
			before := count()
			tx := begin(t)
			if err := tx.exec("INSERT INTO " + table + " VALUES (1)"); err != nil {
				t.Fatal(err)
			}
			if err := tx.exec("SELECT 1/0"); err == nil {
				t.Fatal("division by zero returned no error")
			}
			if err := tx.commit(); !errors.Is(err, postgres.ErrTxCommitRollback) {
				t.Fatalf("Commit() = %v, want postgres.ErrTxCommitRollback", err)
			}
			done := make(chan error, 1)
			go func() { done <- tx.next() }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("the statement after Commit failed: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the conn was not released by Commit")
			}
			if after := count(); after != before {
				t.Fatalf("rows %d -> %d: the aborted transaction committed something", before, after)
			}
		})
		t.Run(name+"/healthy", func(t *testing.T) {
			before := count()
			tx := begin(t)
			if err := tx.exec("INSERT INTO " + table + " VALUES (2)"); err != nil {
				t.Fatal(err)
			}
			if err := tx.commit(); err != nil {
				t.Fatalf("Commit() = %v, want nil", err)
			}
			if after := count(); after != before+1 {
				t.Fatalf("rows %d -> %d, want +1", before, after)
			}
		})
	}
}
