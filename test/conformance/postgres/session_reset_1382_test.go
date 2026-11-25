//go:build postgres

package postgres_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/postgres"
)

// A connection returned to a pool inside a transaction, or whose session reset
// fails, is not handed to the next user (#1382). Every test runs the previous
// user and the next one on a pool of one connection, so a reused connection is
// the same backend process.

func resetDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := openSQLEngine(dsnFromEnv(t))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func resetPool(t *testing.T) *postgres.Pool {
	t.Helper()
	p, err := openPoolEngine(dsnFromEnv(t), postgres.WithMaxOpen(1))
	if err != nil {
		t.Fatalf("postgres.Open: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

type queryRower interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

func pgPID(ctx context.Context, t *testing.T, q queryRower) int {
	t.Helper()
	var pid int
	if err := q.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("pg_backend_pid: %v", err)
	}
	return pid
}

func nativePID(ctx context.Context, t *testing.T, p *postgres.Pool) int {
	t.Helper()
	var pid int
	if err := p.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("pg_backend_pid: %v", err)
	}
	return pid
}

func resetCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// BEGIN and an INSERT as plain SQL on a pinned conn, the conn returned: the next
// user gets a clean connection that does not see the uncommitted row. A failed
// transaction (an error after BEGIN) is the same, and the next user does not get
// SQLSTATE 25P02.
func TestReturnedMidTransactionDatabaseSQL(t *testing.T) {
	for _, failed := range []bool{false, true} {
		name := "open"
		if failed {
			name = "failed"
		}
		t.Run(name, func(t *testing.T) {
			db := resetDB(t)
			ctx := resetCtx(t)
			tbl := uniqueTableName(t, "mid_tx")
			createTable(t, db, tbl, "v text")

			a, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var pidA int
			if err := a.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&pidA); err != nil {
				t.Fatal(err)
			}
			for _, s := range []string{"BEGIN", "INSERT INTO " + tbl + " VALUES ('uncommitted')"} {
				if _, err := a.ExecContext(ctx, s); err != nil {
					t.Fatal(err)
				}
			}
			if failed {
				if _, err := a.ExecContext(ctx, "SELECT 1/0"); err == nil {
					t.Fatal("SELECT 1/0 succeeded")
				}
			}
			if err := a.Close(); err != nil {
				t.Fatal(err)
			}

			// The next user, three statements: each must work.
			if pid := pgPID(ctx, t, db); pid == pidA {
				t.Fatalf("the next user got the previous user's backend %d, still in a transaction", pid)
			}
			var n int
			if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+tbl).Scan(&n); err != nil {
				t.Fatalf("next user: %v", err)
			}
			if n != 0 {
				t.Fatalf("next user sees %d uncommitted rows, want 0", n)
			}
		})
	}
}

// The same on the native Pool.
func TestReturnedMidTransactionNativePool(t *testing.T) {
	for _, failed := range []bool{false, true} {
		name := "open"
		if failed {
			name = "failed"
		}
		t.Run(name, func(t *testing.T) {
			p := resetPool(t)
			ctx := resetCtx(t)
			tbl := uniqueTableName(t, "mid_tx_native")
			if _, err := p.ExecContext(ctx, "CREATE TABLE "+tbl+" (v text)"); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = p.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+tbl) })

			pidA := nativePID(ctx, t, p)
			// The native Pool has no pinned connection: one multi-statement
			// query leaves the transaction open on the connection it ran on.
			stmts := "BEGIN; INSERT INTO " + tbl + " VALUES ('uncommitted')"
			if failed {
				// The failing statement ends the query with an error: the
				// transaction is left in the failed state.
				stmts += "; SELECT 1/0"
			}
			_, err := p.ExecContext(ctx, stmts)
			if failed && err == nil {
				t.Fatal("SELECT 1/0 succeeded")
			}
			if !failed && err != nil {
				t.Fatal(err)
			}
			if pid := nativePID(ctx, t, p); pid == pidA {
				t.Fatalf("the next caller got the previous caller's backend %d, still in a transaction", pid)
			}
			var n int
			if err := p.QueryRow(ctx, "SELECT count(*) FROM "+tbl).Scan(&n); err != nil {
				t.Fatalf("next caller: %v", err)
			}
			if n != 0 {
				t.Fatalf("next caller sees %d uncommitted rows, want 0", n)
			}
		})
	}
}

// Forced reset failure, server error 25001: the session is dirty (the driver's
// own BeginTx sets the flag) and a transaction is open when the conn is
// returned. Before the fix, DISCARD ALL failed, the package ErrBadConn was ignored by
// database/sql, and every following statement got SQLSTATE 25P02 on a poisoned
// connection.
func TestForcedResetFailurePoisonedConnDiscarded(t *testing.T) {
	db := resetDB(t)
	ctx := resetCtx(t)
	a, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var pidA int
	if err := a.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&pidA); err != nil {
		t.Fatal(err)
	}
	if err := a.Raw(func(dc any) error {
		_, err := dc.(driver.ConnBeginTx).BeginTx(ctx, driver.TxOptions{})
		return err
	}); err != nil {
		t.Skipf("the driver's BeginTx is not reachable through Raw: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		var one int
		if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
			t.Fatalf("innocent user %d: %v", i, err)
		}
	}
	if pid := pgPID(ctx, t, db); pid == pidA {
		t.Fatalf("the next user got the poisoned backend %d", pid)
	}
}

// Forced reset failure, server error 57014: the session is dirty, no transaction
// is open, and DISCARD ALL runs into statement_timeout (2000 temporary tables,
// 1 ms). The reset fails, the connection must not be handed out with the
// previous user's settings. Whether the timeout fires or the reset wins the
// race, the next user must see a reset session.
func TestForcedResetFailureTimedOutDiscardDiscarded(t *testing.T) {
	db := resetDB(t)
	ctx := resetCtx(t)
	a, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Raw(func(dc any) error {
		_, err := dc.(driver.ConnBeginTx).BeginTx(ctx, driver.TxOptions{})
		return err
	}); err != nil {
		t.Skipf("the driver's BeginTx is not reachable through Raw: %v", err)
	}
	// COMMIT as plain SQL ends the transaction and leaves the dirty flag set.
	for _, s := range []string{
		"COMMIT",
		"DO $$ BEGIN FOR i IN 1..2000 LOOP EXECUTE format('CREATE TEMP TABLE tt_%s(x int)', i); END LOOP; END $$",
		"SET application_name = 'previous_user'",
		"SET statement_timeout = '1ms'",
	} {
		if _, err := a.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	var app, st string
	var tmp int
	if err := db.QueryRowContext(ctx, "SELECT current_setting('application_name'), current_setting('statement_timeout'), "+
		"(SELECT count(*) FROM pg_class WHERE relname LIKE 'tt%' AND relnamespace = pg_my_temp_schema())").Scan(&app, &st, &tmp); err != nil {
		t.Fatalf("next user: %v", err)
	}
	if strings.Contains(app, "previous_user") || st == "1ms" || tmp != 0 {
		t.Fatalf("next user sees the previous user's session: application_name=%q statement_timeout=%q temp tables=%d", app, st, tmp)
	}
}

// The ordinary paths still reuse their connection: BeginTx with Commit or
// Rollback, on database/sql and on the native Pool.
func TestTxPathReusesConnection(t *testing.T) {
	t.Run("database/sql", func(t *testing.T) {
		db := resetDB(t)
		ctx := resetCtx(t)
		want := pgPID(ctx, t, db)
		for i := 0; i < 6; i++ {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(ctx, "SELECT 1"); err != nil {
				t.Fatal(err)
			}
			if i%2 == 0 {
				err = tx.Commit()
			} else {
				err = tx.Rollback()
			}
			if err != nil {
				t.Fatal(err)
			}
			if pid := pgPID(ctx, t, db); pid != want {
				t.Fatalf("round %d: backend %d, want %d (the conn was not reused)", i, pid, want)
			}
		}
	})
	t.Run("native", func(t *testing.T) {
		p := resetPool(t)
		ctx := resetCtx(t)
		want := nativePID(ctx, t, p)
		for i := 0; i < 6; i++ {
			tx, err := p.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(ctx, "SELECT 1"); err != nil {
				t.Fatal(err)
			}
			if i%2 == 0 {
				err = tx.Commit()
			} else {
				err = tx.Rollback()
			}
			if err != nil {
				t.Fatal(err)
			}
			if pid := nativePID(ctx, t, p); pid != want {
				t.Fatalf("round %d: backend %d, want %d (the conn was not reused)", i, pid, want)
			}
		}
	})
}
