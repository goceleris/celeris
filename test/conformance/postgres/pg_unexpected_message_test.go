//go:build postgres

package postgres_test

// A COPY statement sent through Query, QueryRow, Exec or Prepare (instead of
// CopyFrom / CopyTo) makes the server answer with a CopyOut / CopyIn stream the
// driver's query paths do not read. The connection must then be failed and
// replaced, not kept with the rest of that reply dispatched to whatever request
// is queued next. The caller that misused COPY gets an error that names CopyFrom
// and CopyTo; the callers that follow, on the same pool, get their own rows and
// never an "unexpected message" error. Pool accounting stays bounded.
//
// Every statement is followed AT ONCE by marked queries (simple and extended,
// alternating) on a pool of MaxOpen 1 and 2, in four pool modes: Pool direct,
// Pool on an engine, database/sql Connector, database/sql Connector on an engine.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	postgres "github.com/goceleris/celeris/driver/postgres"
)

type pguTarget struct {
	name   string
	open   func(maxOpen int) error
	close  func()
	op     func(ctx context.Context, op, stmt string) error
	marked func(ctx context.Context, marker string, extended bool) ([]string, error)
	// leaked reports the pool's slots that are in use while no call is running.
	leaked func() (inUse, open int)
}

func pguMarkedSQL(marker string, extended bool) (string, []any) {
	if extended {
		return "SELECT $1::text || '-' || g::text FROM generate_series(1, 3) g", []any{marker}
	}
	return fmt.Sprintf("SELECT '%s-' || g::text FROM generate_series(1, 3) g", marker), nil
}

func pguPoolTarget(name string, openPool func(maxOpen int) (*postgres.Pool, error)) *pguTarget {
	var pool *postgres.Pool
	tg := &pguTarget{name: name}
	tg.open = func(maxOpen int) error {
		var err error
		pool, err = openPool(maxOpen)
		return err
	}
	tg.close = func() { pguCloseBounded(func() { _ = pool.Close() }) }
	tg.leaked = func() (int, int) { s := pool.Stats(); return s.InUse, s.Open }
	tg.op = func(ctx context.Context, op, stmt string) error {
		switch op {
		case "Query":
			r, err := pool.QueryContext(ctx, stmt)
			if err != nil {
				return err
			}
			for r.Next() { //revive:disable-line:empty-block
			}
			err = r.Err()
			_ = r.Close()
			return err
		case "QueryRow":
			var s any
			return pool.QueryRow(ctx, stmt).Scan(&s)
		default:
			_, err := pool.ExecContext(ctx, stmt)
			return err
		}
	}
	tg.marked = func(ctx context.Context, marker string, extended bool) ([]string, error) {
		q, args := pguMarkedSQL(marker, extended)
		r, err := pool.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		defer func() { _ = r.Close() }()
		var out []string
		for r.Next() {
			var s string
			if err := r.Scan(&s); err != nil {
				return out, err
			}
			out = append(out, s)
		}
		return out, r.Err()
	}
	return tg
}

func pguSQLTarget(name string, openDB func() (*sql.DB, error)) *pguTarget {
	var db *sql.DB
	tg := &pguTarget{name: name}
	tg.open = func(maxOpen int) error {
		var err error
		db, err = openDB()
		if err != nil {
			return err
		}
		db.SetMaxOpenConns(maxOpen)
		db.SetMaxIdleConns(maxOpen)
		return nil
	}
	tg.close = func() { pguCloseBounded(func() { _ = db.Close() }) }
	tg.leaked = func() (int, int) { s := db.Stats(); return s.InUse, s.OpenConnections }
	tg.op = func(ctx context.Context, op, stmt string) error {
		switch op {
		case "Query":
			r, err := db.QueryContext(ctx, stmt)
			if err != nil {
				return err
			}
			for r.Next() { //revive:disable-line:empty-block
			}
			err = r.Err()
			_ = r.Close()
			return err
		case "QueryRow":
			var s any
			return db.QueryRowContext(ctx, stmt).Scan(&s)
		case "Prepared":
			st, err := db.PrepareContext(ctx, stmt)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			_, err = st.ExecContext(ctx)
			return err
		default:
			_, err := db.ExecContext(ctx, stmt)
			return err
		}
	}
	tg.marked = func(ctx context.Context, marker string, extended bool) ([]string, error) {
		q, args := pguMarkedSQL(marker, extended)
		r, err := db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		defer func() { _ = r.Close() }()
		var out []string
		for r.Next() {
			var s string
			if err := r.Scan(&s); err != nil {
				return out, err
			}
			out = append(out, s)
		}
		return out, r.Err()
	}
	return tg
}

func TestPGCopyThroughQueryFailsConn(t *testing.T) {
	base := dsnFromEnv(t)
	app := fmt.Sprintf("pgunexp%d", time.Now().UnixNano()%1_000_000_000)
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	dsn := base + sep + "application_name=" + app

	setup, err := sql.Open(driverName, base)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = setup.Close() }()
	table := uniqueTableName(t, "pgunexp")
	if _, err := setup.Exec("CREATE TABLE " + table + " (a int, b text)"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = setup.Exec("DROP TABLE IF EXISTS " + table) })
	if _, err := setup.Exec("INSERT INTO " + table + " SELECT g, 'row' || g FROM generate_series(1,3) g"); err != nil {
		t.Fatal(err)
	}
	sessions := func(t *testing.T) int {
		var n int
		if err := setup.QueryRow("SELECT count(*) FROM pg_stat_activity WHERE application_name = $1", app).Scan(&n); err != nil {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		return n
	}

	iters := 6
	if v, err := strconv.Atoi(os.Getenv("CELERIS_PGUNEXP_ITERS")); err == nil && v > 0 {
		iters = v
	}
	stmts := []struct{ name, sql string }{
		{"copyout-small", "COPY " + table + " TO STDOUT"},
		{"copyout-50k-rows", "COPY (SELECT g, repeat('x', 100) FROM generate_series(1, 50000) g) TO STDOUT"},
		{"copyout-one-200KB-row", "COPY (SELECT repeat('x', 200000)) TO STDOUT"},
		{"copyin", "COPY " + table + " FROM STDIN"},
	}
	engine := func() postgres.ServerProvider { return celeris.New(celeris.Config{Addr: "127.0.0.1:0"}) }
	targets := []*pguTarget{
		pguPoolTarget("pool-direct", func(n int) (*postgres.Pool, error) {
			return postgres.Open(dsn, postgres.WithMaxOpen(n))
		}),
		pguPoolTarget("pool-loop", func(n int) (*postgres.Pool, error) {
			return postgres.Open(dsn, postgres.WithMaxOpen(n), postgres.WithEngine(engine()))
		}),
		pguSQLTarget("sql-connector", func() (*sql.DB, error) {
			c, err := postgres.NewConnector(dsn)
			if err != nil {
				return nil, err
			}
			return sql.OpenDB(c), nil
		}),
		pguSQLTarget("sql-engine", func() (*sql.DB, error) {
			c, err := postgres.NewConnector(dsn)
			if err != nil {
				return nil, err
			}
			return sql.OpenDB(c.WithEngine(engine())), nil
		}),
		// The engine of CELERIS_CONFORMANCE_ENGINE, as the other conformance tests
		// open theirs: the same as pool-direct / sql-connector when it is unset,
		// the native loop under epoll-ready.
		pguPoolTarget("pool-mode", func(n int) (*postgres.Pool, error) {
			return openPoolEngine(dsn, postgres.WithMaxOpen(n))
		}),
		pguSQLTarget("sql-mode", func() (*sql.DB, error) { return openSQLEngine(dsn) }),
	}
	var seq atomic.Int64
	for _, tg := range targets {
		ops := []string{"Query", "QueryRow", "Exec"}
		if strings.HasPrefix(tg.name, "sql") {
			ops = append(ops, "Prepared")
		}
		for _, maxOpen := range []int{1, 2} {
			for _, st := range stmts {
				for _, op := range ops {
					name := fmt.Sprintf("%s/maxopen%d/%s/%s", tg.name, maxOpen, st.name, op)
					t.Run(name, func(t *testing.T) {
						if err := tg.open(maxOpen); err != nil {
							t.Fatalf("open: %v", err)
						}
						closed := false
						defer func() {
							if !closed {
								tg.close()
							}
						}()
						for i := 0; i < iters; i++ {
							ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
							err := tg.op(ctx, op, st.sql)
							cancel()
							// Errorf, not Fatalf: the marked queries below are the other half
							// of the contract and must run whatever this returned.
							if err == nil {
								t.Errorf("iteration %d: a COPY through %s returned no error", i, op)
							} else if !strings.Contains(err.Error(), "CopyFrom") || !strings.Contains(err.Error(), "CopyTo") {
								t.Errorf("iteration %d: a COPY through %s returned %q, want an error that names CopyFrom and CopyTo", i, op, err)
							}
							// Three marked queries AT ONCE, simple and extended alternating.
							for k := 0; k < 3; k++ {
								m := fmt.Sprintf("pu%d", seq.Add(1))
								mctx, mcancel := context.WithTimeout(context.Background(), 20*time.Second)
								got, merr := tg.marked(mctx, m, (i+k)%2 == 1)
								mcancel()
								if merr != nil {
									t.Errorf("iteration %d marked query %d after the COPY: %v (want its own rows; \"unexpected message\" is the leftover of the COPY reply)", i, k, merr)
									continue
								}
								want := []string{m + "-1", m + "-2", m + "-3"}
								if strings.Join(got, ",") != strings.Join(want, ",") {
									t.Errorf("iteration %d marked query %d: got %q, want %q", i, k, got, want)
								}
							}
						}
						// Pool accounting: nothing in use, nothing above MaxOpen, and the
						// failed conns are gone from the server.
						if inUse, open := tg.leaked(); inUse != 0 || open > maxOpen {
							t.Fatalf("pool accounting: InUse %d, Open %d, MaxOpen %d", inUse, open, maxOpen)
						}
						deadline := time.Now().Add(10 * time.Second)
						for sessions(t) > maxOpen {
							if time.Now().After(deadline) {
								t.Fatalf("pg_stat_activity: %d sessions of this pool, MaxOpen %d (failed conns are not closed)", sessions(t), maxOpen)
							}
							time.Sleep(50 * time.Millisecond)
						}
						tg.close()
						closed = true
						deadline = time.Now().Add(10 * time.Second)
						for sessions(t) != 0 {
							if time.Now().After(deadline) {
								t.Fatalf("pg_stat_activity: %d sessions left after the pool closed", sessions(t))
							}
							time.Sleep(50 * time.Millisecond)
						}
					})
				}
			}
		}
	}
}

func pguCloseBounded(f func()) {
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
}
