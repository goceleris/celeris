//go:build postgres

package postgres_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
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
			if failed {
				// The statement's own error, bare, as in v1.5.11.
				if pe, ok := err.(*postgres.PGError); !ok || pe.Code != "22012" {
					t.Fatalf("Exec %q = %#v, want the bare *PGError 22012", stmts, err)
				}
			} else if !errors.Is(err, postgres.ErrTxLeftOpen) {
				t.Fatalf("Exec %q = %v, want ErrTxLeftOpen", stmts, err)
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
// connection. Since the fix this case is caught by the transaction status of the
// last ReadyForQuery before DISCARD ALL is sent; the path of a DISCARD ALL that
// fails with the conn left usable is covered by the lock test below (a real
// server) and by TestResetSessionFailure* (the fake server).
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

// Forced reset failure on a real server, deterministic: the session is dirty, no
// transaction is open, and DISCARD ALL cannot finish because another session
// holds a lock on a temporary table of the first one (LOCK TABLE pg_temp_N.t
// works across sessions), so the reset runs into the driver's own 1 s bound and
// fails. Before the fix the failure was ignored by database/sql and the next
// user got the connection with the previous user's session; now the connection
// is discarded and the next user gets a clean one (another backend).
func TestForcedResetFailureBlockedDiscardDiscarded(t *testing.T) {
	db := resetDB(t)
	other := resetDB(t)
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
		"CREATE TEMP TABLE reset_blocked(x int)",
		"SET application_name = 'previous_user'",
	} {
		if _, err := a.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	var sch string
	var pidA int
	if err := a.QueryRowContext(ctx, "SELECT pg_my_temp_schema()::regnamespace::text, pg_backend_pid()").Scan(&sch, &pidA); err != nil {
		t.Fatal(err)
	}
	// Another session locks the temporary table, so DISCARD ALL has to wait.
	b, err := other.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = b.ExecContext(context.Background(), "ROLLBACK"); _ = b.Close() })
	for _, s := range []string{"BEGIN", "LOCK TABLE " + sch + ".reset_blocked IN ACCESS SHARE MODE"} {
		if _, err := b.ExecContext(ctx, s); err != nil {
			t.Skipf("cannot lock another session's temporary table: %v", err)
		}
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	var app string
	var pidB int
	if err := db.QueryRowContext(ctx, "SELECT current_setting('application_name'), pg_backend_pid()").Scan(&app, &pidB); err != nil {
		t.Fatalf("next user: %v", err)
	}
	if time.Since(start) < 900*time.Millisecond {
		t.Fatalf("the next user was served after %v: the reset did not run into its 1 s bound", time.Since(start))
	}
	if strings.Contains(app, "previous_user") || pidB == pidA {
		t.Fatalf("next user got the previous user's session: application_name=%q, backend %d (was %d)", app, pidB, pidA)
	}
}

// A one-shot transaction that fails returns its own error, bare, and does not
// poison the pool: BEGIN; INSERT; COMMIT in one call on a duplicate key is a
// *PGError 23505 on the native Pool and through database/sql, as in v1.5.11, the
// next caller gets a working connection (no 25P02), and the same call without
// the failure returns nil and keeps its connection.
func TestOneShotTransactionErrorIsBare(t *testing.T) {
	const stmt = "BEGIN; INSERT INTO %s VALUES (1); COMMIT"
	t.Run("native", func(t *testing.T) {
		p := resetPool(t)
		ctx := resetCtx(t)
		tbl := uniqueTableName(t, "oneshot")
		if _, err := p.ExecContext(ctx, "CREATE TABLE "+tbl+" (id int primary key)"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = p.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+tbl) })
		pid := nativePID(ctx, t, p)
		if _, err := p.ExecContext(ctx, fmt.Sprintf(stmt, tbl)); err != nil {
			t.Fatalf("one-shot transaction: %v", err)
		}
		if got := nativePID(ctx, t, p); got != pid {
			t.Fatalf("a successful one-shot transaction moved to backend %d (was %d)", got, pid)
		}
		_, err := p.ExecContext(ctx, fmt.Sprintf(stmt, tbl))
		if pe, ok := err.(*postgres.PGError); !ok || pe.Code != "23505" {
			t.Fatalf("duplicate key = %#v, want the bare *PGError 23505", err)
		}
		var n int
		if err := p.QueryRow(ctx, "SELECT count(*) FROM "+tbl).Scan(&n); err != nil || n != 1 {
			t.Fatalf("next caller: count = %d, %v, want 1 and no error", n, err)
		}
	})
	t.Run("database/sql", func(t *testing.T) {
		db := resetDB(t)
		ctx := resetCtx(t)
		tbl := uniqueTableName(t, "oneshot_sql")
		createTable(t, db, tbl, "id int primary key")
		if _, err := db.ExecContext(ctx, fmt.Sprintf(stmt, tbl)); err != nil {
			t.Fatalf("one-shot transaction: %v", err)
		}
		_, err := db.ExecContext(ctx, fmt.Sprintf(stmt, tbl))
		if pe, ok := err.(*postgres.PGError); !ok || pe.Code != "23505" {
			t.Fatalf("duplicate key = %#v, want the bare *PGError 23505", err)
		}
		var n int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+tbl).Scan(&n); err != nil || n != 1 {
			t.Fatalf("next user: count = %d, %v, want 1 and no error", n, err)
		}
	})
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

// Pool.ExecContext("BEGIN") returns ErrTxLeftOpen from the BEGIN itself, before
// anything else runs: a caller that stops at the error has written nothing, and
// the next caller gets another backend. Pool.BeginTx is unchanged, and is the
// Pool's pinned connection: BEGIN, savepoints and COMMIT run on one backend.
func TestNativeExecBeginIsAnError(t *testing.T) {
	p := resetPool(t)
	ctx := resetCtx(t)
	tbl := uniqueTableName(t, "begin_err")
	if _, err := p.ExecContext(ctx, "CREATE TABLE "+tbl+" (v text)"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = p.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+tbl) })

	pidA := nativePID(ctx, t, p)
	if _, err := p.ExecContext(ctx, "BEGIN"); !errors.Is(err, postgres.ErrTxLeftOpen) {
		t.Fatalf("Exec BEGIN = %v, want ErrTxLeftOpen", err)
	}
	// The caller stops at the error: no row.
	var n int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM "+tbl).Scan(&n); err != nil || n != 0 {
		t.Fatalf("count = %d, %v, want 0", n, err)
	}
	if pid := nativePID(ctx, t, p); pid == pidA {
		t.Fatalf("the BEGIN connection %d was reused", pid)
	}

	// The Query path: Rows.Close and Row.Scan carry the error.
	rows, err := p.QueryContext(ctx, "BEGIN")
	if err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); !errors.Is(err, postgres.ErrTxLeftOpen) {
		t.Fatalf("Rows.Close after Query BEGIN = %v, want ErrTxLeftOpen", err)
	}
	var x int
	// QueryRow("BEGIN") returns no row: the scan's own error, the conn is discarded.
	if err := p.QueryRow(ctx, "BEGIN").Scan(&x); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Row.Scan after QueryRow BEGIN = %v, want sql.ErrNoRows", err)
	}
	// A statement that returns a row and leaves the transaction open: ErrTxLeftOpen.
	if err := p.QueryRow(ctx, "BEGIN; SELECT 1").Scan(&x); !errors.Is(err, postgres.ErrTxLeftOpen) {
		t.Fatalf("Row.Scan after QueryRow BEGIN; SELECT 1 = %v, want ErrTxLeftOpen", err)
	}

	// The pinned connection: BeginTx, statements, a savepoint, Commit.
	tx, err := p.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var txPID int
	if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&txPID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO "+tbl+" VALUES ('kept')"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Savepoint(ctx, "sp"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO "+tbl+" VALUES ('undone')"); err != nil {
		t.Fatal(err)
	}
	if err := tx.RollbackToSavepoint(ctx, "sp"); err != nil {
		t.Fatal(err)
	}
	var pid2 int
	if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid2); err != nil || pid2 != txPID {
		t.Fatalf("the Tx moved between backends: %d then %d (%v)", txPID, pid2, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	var vals string
	if err := p.QueryRow(ctx, "SELECT string_agg(v, ',') FROM "+tbl).Scan(&vals); err != nil || vals != "kept" {
		t.Fatalf("rows after the Tx = %q, %v, want kept", vals, err)
	}
	// The Tx's connection went back to the pool and is reused (pool of one).
	if pid := nativePID(ctx, t, p); pid != txPID {
		t.Fatalf("the Tx's connection was not reused: %d then %d", txPID, pid)
	}
}
