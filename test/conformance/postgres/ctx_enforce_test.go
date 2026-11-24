//go:build postgres

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/driver/postgres"
)

// A context that ends while a statement runs must end the call, on every route:
// database/sql, the Pool in direct mode (no engine) and the Pool with the
// driver's own loop. `SELECT pg_sleep(3)` under a context that ends after
// 300 ms, by deadline and by cancel(): the call returns well before the
// statement would have finished, with the context's error or the server's
// 57014, and the conn is usable afterwards (the next statement runs).
func TestContextEndingMidStatementEndsTheCall(t *testing.T) {
	dsn := dsnFromEnv(t)
	type arm struct {
		name string
		run  func(ctx context.Context, q string) error
	}
	var arms []arm
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
				err := a.run(ctx, "SELECT pg_sleep(3)")
				if el := time.Since(start); el > 2*time.Second {
					t.Fatalf("returned after %v (context ended after 300 ms, statement runs 3 s), err=%v", el, err)
				}
				var pe *postgres.PGError
				// database/sql and the Pool on a loop may surface the server's 57014;
				// a Pool conn in direct mode always returns the context's error.
				if a.name == "pool-direct" {
					if !errors.Is(err, ctx.Err()) {
						t.Fatalf("err = %v, want the context's error (%v)", err, ctx.Err())
					}
				} else if !errors.Is(err, ctx.Err()) && !(errors.As(err, &pe) && pe.Code == "57014") {
					t.Fatalf("err = %v, want the context's error or 57014", err)
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
