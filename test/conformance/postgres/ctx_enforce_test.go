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

// A context that ends while a statement runs must end the call, on every route:
// database/sql, the Pool in direct mode (no engine) and the Pool with the driver's
// own loop, whether it ends by its deadline or by cancel() with no deadline.
// `SELECT pg_sleep(3)` under a 300 ms context: the call returns well before the
// statement would have finished, with the context's error (a direct-mode Pool) or
// the server's 57014 (database/sql, a Pool on a loop), the statement is no longer
// running on the server (pg_stat_activity, read from another connection once the
// call has returned: the CancelRequest reached the backend), and the conn is usable
// afterwards (the next statement runs).
func TestContextEndingMidStatementEndsTheCall(t *testing.T) {
	dsn := dsnFromEnv(t)
	type arm struct {
		name string
		run  func(ctx context.Context, q string) error
	}
	var arms []arm
	mon, err := sql.Open("celeris-postgres", dsn) // reads pg_stat_activity, from a connection of its own
	if err != nil {
		t.Fatal(err)
	}
	mon.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = mon.Close() })
	db, err := sql.Open("celeris-postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	arms = append(arms, arm{"database-sql", func(ctx context.Context, q string) error { _, err := db.ExecContext(ctx, q); return err }})
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
		arms = append(arms, arm{mode, func(ctx context.Context, q string) error { _, err := p.ExecContext(ctx, q); return err }})
	}
	for _, a := range arms {
		for _, kind := range []string{"deadline", "cancel"} {
			t.Run(a.name+"/"+kind, func(t *testing.T) {
				var ctx context.Context
				var cancel context.CancelFunc
				if kind == "deadline" {
					ctx, cancel = context.WithTimeout(context.Background(), 300*time.Millisecond)
				} else {
					ctx, cancel = context.WithCancel(context.Background())
					time.AfterFunc(300*time.Millisecond, cancel)
				}
				defer cancel()
				start := time.Now()
				tag := fmt.Sprintf("enforce-%s-%s-%d", a.name, kind, time.Now().UnixNano())
				err := a.run(ctx, "SELECT pg_sleep(3) /* "+tag+" */")
				if el := time.Since(start); el > 2*time.Second {
					t.Fatalf("returned after %v (context ended after 300 ms, statement runs 3 s), err=%v", el, err)
				}
				var pe *postgres.PGError
				// database/sql and the Pool on a loop may surface the server's 57014;
				// a Pool conn in direct mode always returns the context's error.
				// A deadline can end the call a hair before the context's own timer
				// has set Err (the socket's deadline is the context's): the want is
				// then DeadlineExceeded.
				want := ctx.Err()
				if want == nil && kind == "deadline" {
					want = context.DeadlineExceeded
				}
				if a.name == "pool-direct" {
					if !errors.Is(err, want) {
						t.Fatalf("err = %v, want the context's error (%v)", err, want)
					}
				} else if !errors.Is(err, want) && !(errors.As(err, &pe) && pe.Code == "57014") {
					t.Fatalf("err = %v, want the context's error or 57014", err)
				}
				if n := activeStatements(t, mon, tag); n != 0 {
					t.Fatalf("%d statements tagged %s are still running on the server after the call returned: the cancel did not reach the backend", n, tag)
				}
				nctx, ncancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer ncancel()
				if err := a.run(nctx, "SELECT 1"); err != nil {
					t.Fatalf("the statement after the cancelled one failed: %v", err)
				}
			})
		}
	}
}

// activeStatements counts the backends (other than mon's own) that are running a
// statement whose text carries tag.
func activeStatements(t *testing.T, mon *sql.DB, tag string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var n int
	if err := mon.QueryRowContext(ctx,
		"SELECT count(*) FROM pg_stat_activity WHERE state = 'active' AND pid <> pg_backend_pid() AND query LIKE '%' || $1 || '%'", tag).Scan(&n); err != nil {
		t.Fatalf("pg_stat_activity: %v", err)
	}
	return n
}
