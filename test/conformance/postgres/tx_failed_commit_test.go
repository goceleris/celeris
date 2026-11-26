//go:build postgres

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/postgres"
)

// A call to Commit or Rollback ends the native Tx whatever it returns. After a Commit that
// failed (the server's error, or the connection gone) every method of the Tx returns
// postgres.ErrTxDone and sends nothing, a Rollback after it is safe (the deferred-Rollback
// idiom), the connection is released exactly once (never leaked: MaxOpen 1 in a loop), and
// what the table holds is judged by a second session. The database/sql route is unchanged.

type failedCommitEnv struct {
	db    *sql.DB // the second session: what is actually in the tables
	dsn   string
	ctx   context.Context
	count func(table string) int
}

func newFailedCommitEnv(t *testing.T) *failedCommitEnv {
	t.Helper()
	dsn := dsnFromEnv(t)
	db := openDB(t)
	ctx := context.Background()
	return &failedCommitEnv{db: db, dsn: dsn, ctx: ctx, count: func(table string) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}}
}

// uniqueDeferred creates a table whose unique constraint is checked at COMMIT, holding one
// row (1), plus a plain side table.
func (e *failedCommitEnv) uniqueDeferred(t *testing.T) (u, w string) {
	t.Helper()
	u, w = uniqueTableName(t, "ud"), uniqueTableName(t, "side")
	createTable(t, e.db, u, "id int, CONSTRAINT "+u+"_uq UNIQUE (id) DEFERRABLE INITIALLY DEFERRED")
	createTable(t, e.db, w, "id int")
	if _, err := e.db.ExecContext(e.ctx, "INSERT INTO "+u+" VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	return u, w
}

// kill terminates the backend and waits until the server no longer lists it, so the next
// call on the connection meets a closed socket, not a race with the termination.
func (e *failedCommitEnv) kill(t *testing.T, pid int) {
	t.Helper()
	if _, err := e.db.ExecContext(e.ctx, "SELECT pg_terminate_backend($1)", pid); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := e.db.QueryRowContext(e.ctx, "SELECT count(*) FROM pg_stat_activity WHERE pid = $1", pid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("backend %d still listed 10 s after pg_terminate_backend", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func poolArms(t *testing.T, dsn string, maxOpen int) map[string]*postgres.Pool {
	t.Helper()
	arms := map[string]*postgres.Pool{}
	direct, err := postgres.Open(dsn, postgres.WithMaxOpen(maxOpen))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = direct.Close() })
	arms["pool-direct"] = direct
	engine, err := openPoolEngine(dsn, postgres.WithMaxOpen(maxOpen))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	arms["pool-engine"] = engine
	return arms
}

func begin(t *testing.T, p *postgres.Pool) *postgres.Tx {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := p.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v (the connection of an earlier Tx was not released)", err)
	}
	return tx
}

// assertEnded: every method returns ErrTxDone, and nothing was written. finishers=false
// leaves out the second Commit and the Rollback: on a driver that held the conn after a
// failed Commit, either of them is what freed it, so a test of "the caller never rolls
// back" must not call them.
func assertEnded(t *testing.T, e *failedCommitEnv, tx *postgres.Tx, side string, what string, finishers bool) {
	t.Helper()
	ctx := e.ctx
	check := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, postgres.ErrTxDone) || !errors.Is(err, sql.ErrTxDone) {
			t.Errorf("%s: %s = %v, want postgres.ErrTxDone", what, name, err)
		}
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO "+side+" VALUES (42)")
	check("ExecContext", err)
	_, err = tx.QueryContext(ctx, "SELECT 1")
	check("QueryContext", err)
	check("QueryRow.Scan", tx.QueryRow(ctx, "SELECT 1").Scan(new(int)))
	check("Savepoint", tx.Savepoint(ctx, "sp"))
	check("ReleaseSavepoint", tx.ReleaseSavepoint(ctx, "sp"))
	check("RollbackToSavepoint", tx.RollbackToSavepoint(ctx, "sp"))
	if finishers {
		check("Commit (second)", tx.Commit())
		check("Rollback", tx.Rollback())
	}
	if n := e.count(side); n != 0 {
		t.Errorf("%s: %d rows in %s: a statement after the failed Commit committed on its own", what, n, side)
	}
}

func TestFailedCommitEndsTheTx(t *testing.T) {
	e := newFailedCommitEnv(t)
	for _, withSavepoint := range []bool{false, true} {
		for _, rollbackAfter := range []bool{false, true} {
			name := "deferred-unique-23505"
			if withSavepoint {
				name += "/savepoint-open"
			}
			if rollbackAfter {
				name += "/then-rollback"
			}
			t.Run(name, func(t *testing.T) {
				u, w := e.uniqueDeferred(t)
				for arm, p := range poolArms(t, e.dsn, 1) { // MaxOpen 1: a leaked conn blocks the next BeginTx
					for i := 0; i < 5; i++ {
						tx := begin(t, p)
						if _, err := tx.ExecContext(e.ctx, "INSERT INTO "+u+" VALUES (1)"); err != nil {
							t.Fatalf("%s: %v", arm, err)
						}
						if withSavepoint {
							if err := tx.Savepoint(e.ctx, "sp"); err != nil {
								t.Fatal(err)
							}
						}
						err := tx.Commit()
						var pge *postgres.PGError
						if !errors.As(err, &pge) || pge.Code != "23505" {
							t.Fatalf("%s: Commit = %v, want SQLSTATE 23505", arm, err)
						}
						assertEnded(t, e, tx, w, fmt.Sprintf("%s round %d", arm, i), rollbackAfter)
						if rollbackAfter {
							if err := tx.Rollback(); !errors.Is(err, postgres.ErrTxDone) {
								t.Fatalf("%s: Rollback after the failed Commit = %v, want ErrTxDone", arm, err)
							}
						}
						if st := p.Stats(); st.InUse != 0 {
							t.Fatalf("%s round %d: slot not released: %+v", arm, i, st)
						}
					}
					if n := e.count(u); n != 1 {
						t.Fatalf("%s: %d rows in %s, want the original 1", arm, n, u)
					}
				}
			})
		}
	}
}

// 40001: a serialization failure reported by COMMIT itself (write skew, the second to commit).
func TestFailedCommitSerializationFailure(t *testing.T) {
	e := newFailedCommitEnv(t)
	for arm, p := range poolArms(t, e.dsn, 2) {
		t.Run(arm, func(t *testing.T) {
			s, w := uniqueTableName(t, "ssi"), uniqueTableName(t, "side")
			createTable(t, e.db, s, "k int, v int")
			createTable(t, e.db, w, "id int")
			if _, err := e.db.ExecContext(e.ctx, "INSERT INTO "+s+" VALUES (1,10),(2,20)"); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 4; i++ {
				opts := &sql.TxOptions{Isolation: sql.LevelSerializable}
				bctx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
				t1, err := p.BeginTx(bctx, opts)
				if err != nil {
					t.Fatal(err)
				}
				t2, err := p.BeginTx(bctx, opts)
				cancel()
				if err != nil {
					t.Fatal(err)
				}
				mustRun := func(tx *postgres.Tx, q string) {
					t.Helper()
					if _, err := tx.ExecContext(e.ctx, q); err != nil {
						t.Fatalf("%s: %v", q, err)
					}
				}
				one := func(tx *postgres.Tx, q string) {
					t.Helper()
					var n int
					if err := tx.QueryRow(e.ctx, q).Scan(&n); err != nil {
						t.Fatalf("%s: %v", q, err)
					}
				}
				one(t1, "SELECT sum(v) FROM "+s+" WHERE k = 1")
				mustRun(t1, "UPDATE "+s+" SET v = v + 1 WHERE k = 2")
				one(t2, "SELECT sum(v) FROM "+s+" WHERE k = 2")
				mustRun(t2, "UPDATE "+s+" SET v = v + 1 WHERE k = 1")
				if err := t2.Commit(); err != nil {
					t.Fatalf("first Commit: %v", err)
				}
				err = t1.Commit()
				var pge *postgres.PGError
				if !errors.As(err, &pge) || pge.Code != "40001" {
					t.Fatalf("second Commit = %v, want SQLSTATE 40001", err)
				}
				assertEnded(t, e, t1, w, fmt.Sprintf("round %d", i), false)
				if st := p.Stats(); st.InUse != 0 {
					t.Fatalf("round %d: slot not released: %+v", i, st)
				}
			}
			var sum int
			if err := e.db.QueryRowContext(e.ctx, "SELECT sum(v) FROM "+s).Scan(&sum); err != nil || sum != 30+4 {
				t.Fatalf("sum = %d (err %v), want 34: only the committing transactions applied", sum, err)
			}
		})
	}
}

// The connection is gone (pg_terminate_backend) when Commit runs.
func TestFailedCommitOnAKilledConnection(t *testing.T) {
	e := newFailedCommitEnv(t)
	for _, rollbackAfter := range []bool{false, true} {
		name := "kill"
		if rollbackAfter {
			name += "/then-rollback"
		}
		t.Run(name, func(t *testing.T) {
			u, w := e.uniqueDeferred(t)
			_ = u
			for arm, p := range poolArms(t, e.dsn, 1) {
				for i := 0; i < 5; i++ {
					tx := begin(t, p)
					var pid int
					if err := tx.QueryRow(e.ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
						t.Fatal(err)
					}
					if _, err := tx.ExecContext(e.ctx, "INSERT INTO "+w+" VALUES (7)"); err != nil {
						t.Fatal(err)
					}
					e.kill(t, pid)
					if err := tx.Commit(); err == nil {
						t.Fatalf("%s: Commit on a killed connection returned nil", arm)
					}
					assertEnded(t, e, tx, w, fmt.Sprintf("%s round %d", arm, i), rollbackAfter)
					if rollbackAfter {
						if err := tx.Rollback(); !errors.Is(err, postgres.ErrTxDone) {
							t.Fatalf("%s: Rollback = %v, want ErrTxDone", arm, err)
						}
					}
					if st := p.Stats(); st.InUse != 0 {
						t.Fatalf("%s round %d: slot not released: %+v", arm, i, st)
					}
				}
				// the pool still works, on a new connection
				var one int
				if err := p.QueryRow(e.ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
					t.Fatalf("%s: the pool after the kills: %d, %v", arm, one, err)
				}
			}
		})
	}
}

// A Rollback that fails (the connection is gone) ends the Tx too.
func TestFailedRollbackEndsTheTx(t *testing.T) {
	e := newFailedCommitEnv(t)
	_, w := e.uniqueDeferred(t)
	for arm, p := range poolArms(t, e.dsn, 1) {
		for i := 0; i < 5; i++ {
			tx := begin(t, p)
			var pid int
			if err := tx.QueryRow(e.ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				t.Fatal(err)
			}
			if err := tx.Savepoint(e.ctx, "sp"); err != nil {
				t.Fatal(err)
			}
			e.kill(t, pid)
			if err := tx.Rollback(); err == nil {
				t.Fatalf("%s: Rollback on a killed connection returned nil", arm)
			}
			assertEnded(t, e, tx, w, fmt.Sprintf("%s round %d", arm, i), true)
			if st := p.Stats(); st.InUse != 0 {
				t.Fatalf("%s round %d: slot not released: %+v", arm, i, st)
			}
		}
	}
}

// The database/sql route is database/sql's own and unchanged: the driver's error from
// Commit as it is, then sql.ErrTxDone, the conn back (MaxOpenConns 1), nothing committed.
func TestFailedCommitDatabaseSQLUnchanged(t *testing.T) {
	e := newFailedCommitEnv(t)
	u, w := e.uniqueDeferred(t)
	db, err := openSQLEngine(e.dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	for i := 0; i < 5; i++ {
		tx, err := db.BeginTx(e.ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(e.ctx, "INSERT INTO "+u+" VALUES (1)"); err != nil {
			t.Fatal(err)
		}
		err = tx.Commit()
		var pge *postgres.PGError
		if !errors.As(err, &pge) || pge.Code != "23505" {
			t.Fatalf("Commit = %v, want SQLSTATE 23505", err)
		}
		if _, err := tx.ExecContext(e.ctx, "INSERT INTO "+w+" VALUES (42)"); !errors.Is(err, sql.ErrTxDone) {
			t.Fatalf("Exec after the failed Commit = %v, want sql.ErrTxDone", err)
		}
		if err := tx.Commit(); !errors.Is(err, sql.ErrTxDone) {
			t.Fatalf("second Commit = %v, want sql.ErrTxDone", err)
		}
		if err := tx.Rollback(); !errors.Is(err, sql.ErrTxDone) {
			t.Fatalf("Rollback = %v, want sql.ErrTxDone", err)
		}
		cctx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
		var one int
		err = db.QueryRowContext(cctx, "SELECT 1").Scan(&one)
		cancel()
		if err != nil || one != 1 {
			t.Fatalf("round %d: the conn was not released: %d, %v", i, one, err)
		}
	}
	if n := e.count(w); n != 0 {
		t.Fatalf("%d rows in %s", n, w)
	}
}

// The leak with NO later call on the Tx: MaxOpen 1, a failed Commit, nothing else (no Rollback,
// no second Commit, no statement): the next BeginTx must still get the slot. On a driver that
// holds the connection after a failed Commit this fails with "acquire blocked and timed out".
func TestFailedCommitWithNoLaterCallFreesTheSlot(t *testing.T) {
	e := newFailedCommitEnv(t)
	for _, mode := range []string{"deferred-unique", "killed"} {
		t.Run(mode, func(t *testing.T) {
			u, _ := e.uniqueDeferred(t)
			for arm, p := range poolArms(t, e.dsn, 1) {
				for i := 0; i < 5; i++ {
					tx := begin(t, p) // 10 s bound: fails here when the slot leaked
					if mode == "killed" {
						var pid int
						if err := tx.QueryRow(e.ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
							t.Fatal(err)
						}
						e.kill(t, pid)
					} else if _, err := tx.ExecContext(e.ctx, "INSERT INTO "+u+" VALUES (1)"); err != nil {
						t.Fatalf("%s: %v", arm, err)
					}
					if err := tx.Commit(); err == nil {
						t.Fatalf("%s round %d: Commit returned nil", arm, i)
					}
				}
				if st := p.Stats(); st.InUse != 0 {
					t.Fatalf("%s: slot not released: %+v", arm, st)
				}
			}
		})
	}
}
