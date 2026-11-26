//go:build postgres

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"runtime/pprof"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/driver/postgres"
)

// Two bugs that were in v1.5.11 and are fixed together:
//
//  1. Evicting a statement from the per-conn statement cache sent its DEALLOCATE
//     from a background goroutine that used the same conn as the caller's next
//     request. In direct mode (every standalone Pool, and a Pool opened on an
//     async or io_uring/epoll server) two goroutines read the socket and the
//     caller blocked in Read for ever; on the event loop (database/sql on Linux,
//     a Pool on a std server) the replies were matched to the wrong request and
//     the conn died ("unexpected CommandComplete in extended query").
//  2. A statement without a cached description (an unnamed one, or the first run
//     of a cached one) that the server cut short with an error returned the rows
//     received before the error without their column description, so an int4 cell
//     reached a *string as its raw wire bytes and an *int as "cannot convert".
//
// The randomized concurrent test is in evict_stress_test.go.

// evictDeadline is the longest any of these tests may take to finish a loop.
const evictDeadline = 30 * time.Second

// dumpGoroutines logs every goroutine's stack, for a test that found a hang.
func dumpGoroutines(t *testing.T) {
	t.Helper()
	var b strings.Builder
	_ = pprof.Lookup("goroutine").WriteTo(&b, 2)
	s := b.String()
	if len(s) > 60<<10 {
		s = s[:60<<10] + "\n... (truncated)"
	}
	t.Logf("goroutine dump:\n%s", s)
}

// runWithDeadline runs f on its own goroutine and fails the test, with a
// goroutine dump, when it has not returned by evictDeadline: a hung conn must
// fail the test, not the whole package at its timeout.
func runWithDeadline(t *testing.T, what string, f func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- f() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-time.After(evictDeadline):
		dumpGoroutines(t)
		t.Fatalf("%s: no result after %v (hung)", what, evictDeadline)
	}
}

// evictMode is one way to reach a conn: the native Pool in direct mode, the
// native Pool on the event loop, and database/sql (event loop on Linux).
type evictMode struct {
	name string
	open func(t *testing.T, dsn string, maxOpen int) stressDB
}

var evictModes = []evictMode{
	{"pool-direct", openStressPool(false)},
	{"pool-loop", openStressPool(true)},
	{"database-sql", openStressSQL},
}

// stressDB is what the tests need from a pool, native or database/sql.
type stressDB interface {
	Query(ctx context.Context, q string, args ...any) (stressRows, error)
	Exec(ctx context.Context, q string, args ...any) error
	Close()
}

type stressRows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close() error
}

type stressPool struct{ p *postgres.Pool }

func (s stressPool) Query(ctx context.Context, q string, args ...any) (stressRows, error) {
	r, err := s.p.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func (s stressPool) Exec(ctx context.Context, q string, args ...any) error {
	_, err := s.p.ExecContext(ctx, q, args...)
	return err
}

func (s stressPool) Close() { _ = s.p.Close() }

type stressSQL struct{ db *sql.DB }

func (s stressSQL) Query(ctx context.Context, q string, args ...any) (stressRows, error) {
	r, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func (s stressSQL) Exec(ctx context.Context, q string, args ...any) error {
	_, err := s.db.ExecContext(ctx, q, args...)
	return err
}

func (s stressSQL) Close() { _ = s.db.Close() }

// openStressPool opens a native Pool. Without an engine it is in direct mode;
// WithEngine on a server that is not started gives the standalone event loop
// and no async dispatch, so the Pool takes the event-loop path (the sync poll
// on Linux).
func openStressPool(loop bool) func(t *testing.T, dsn string, maxOpen int) stressDB {
	return func(t *testing.T, dsn string, maxOpen int) stressDB {
		t.Helper()
		opts := []postgres.Option{postgres.WithMaxOpen(maxOpen)}
		if loop {
			opts = append(opts, postgres.WithEngine(celeris.New(celeris.Config{Addr: "127.0.0.1:0"})))
		}
		p, err := postgres.Open(dsn, opts...)
		if err != nil {
			t.Fatalf("postgres.Open: %v", err)
		}
		return stressPool{p}
	}
}

func openStressSQL(t *testing.T, dsn string, maxOpen int) stressDB {
	t.Helper()
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxOpen)
	return stressSQL{db}
}

// queryOne runs a one-row query and scans it.
func queryOne(ctx context.Context, db stressDB, q string, args []any, dest ...any) error {
	rows, err := db.Query(ctx, q, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return errors.New("no row")
	}
	if err := rows.Scan(dest...); err != nil {
		return err
	}
	return rows.Err()
}

// The cache holds 4 statements; a conn runs 120 distinct ones, one after the
// other, so every one past the fourth evicts. Before the fix: a hang in direct
// mode, a dead conn on the event loop, at the fifth statement.
func TestStatementCacheEvictionRunsToCompletion(t *testing.T) {
	dsn := dsnFromEnv(t)
	for _, size := range []int{4, 0} { // 0: the default size (256), more statements than it holds
		n := 120
		if size == 0 {
			n = 600
		}
		for _, m := range evictModes {
			t.Run(fmt.Sprintf("%s/cache=%d", m.name, size), func(t *testing.T) {
				d := dsn
				if size > 0 {
					d = withParams(dsn, "statement_cache_size="+strconv.Itoa(size))
				}
				db := m.open(t, d, 1)
				defer db.Close()
				runWithDeadline(t, "distinct statements", func() error {
					ctx := context.Background()
					for i := 0; i < n; i++ {
						var v int32
						q := fmt.Sprintf("SELECT $1::int4 + %d AS c%d", i, i)
						if err := queryOne(ctx, db, q, []any{1}, &v); err != nil {
							return fmt.Errorf("statement %d: %w", i, err)
						}
						if int(v) != i+1 {
							return fmt.Errorf("statement %d: got %d, want %d", i, v, i+1)
						}
					}
					return nil
				})
			})
		}
	}
}

// The evicted statements are gone from the server: on one conn the server
// holds the statements the cache holds, not every statement ever run.
func TestStatementCacheEvictionDeallocates(t *testing.T) {
	dsn := dsnFromEnv(t)
	const size = 4
	for _, m := range evictModes {
		t.Run(m.name, func(t *testing.T) {
			db := m.open(t, withParams(dsn, "statement_cache_size="+strconv.Itoa(size)), 1)
			defer db.Close()
			runWithDeadline(t, "deallocate", func() error {
				ctx := context.Background()
				for i := 0; i < 50; i++ {
					var v int32
					if err := queryOne(ctx, db, fmt.Sprintf("SELECT $1::int4 + %d AS d%d", i, i), []any{1}, &v); err != nil {
						return err
					}
				}
				// No arguments: the simple protocol, nothing cached or prepared.
				var n int
				if err := queryOne(ctx, db, "SELECT count(*) FROM pg_prepared_statements", nil, &n); err != nil {
					return err
				}
				if n > size {
					return fmt.Errorf("the server holds %d prepared statements, the cache holds at most %d", n, size)
				}
				return nil
			})
		})
	}
}

// The rows that arrived before a server error keep their column description, on
// the cached and on the unnamed statement, through the native Pool and
// database/sql: an int4 cell reads as 1, not as the raw bytes 00 00 00 01.
func TestRowsBeforeServerErrorKeepTheirColumns(t *testing.T) {
	dsn := dsnFromEnv(t)
	const q = "SELECT n::int4 AS a, n::int4 AS b, n::int8 AS c, n::text AS d, n::int4 AS e FROM generate_series(1,5) AS n WHERE $1::int4 = 1 AND 1/(n-3) > -100"
	for _, route := range []struct{ name, dsn string }{
		{"cached", dsn},
		{"unnamed", withParams(dsn, "auto_cache_statements=false")},
	} {
		for _, m := range evictModes {
			t.Run(route.name+"/"+m.name, func(t *testing.T) {
				db := m.open(t, route.dsn, 1)
				defer db.Close()
				runWithDeadline(t, "rows before an error", func() error {
					ctx := context.Background()
					rows, err := db.Query(ctx, q, 1)
					if err != nil {
						return fmt.Errorf("the rows before the error were not handed over: Query failed with %v", err)
					}
					defer func() { _ = rows.Close() }()
					got := 0
					for rows.Next() {
						var s string
						var i int
						var i32 int32
						var i64 int64
						var d string
						if err := rows.Scan(&s, &i, &i64, &d, &i32); err != nil {
							return fmt.Errorf("row %d: %w", got+1, err)
						}
						want := strconv.Itoa(got + 1)
						if s != want || i != got+1 || i64 != int64(got+1) || d != want || int(i32) != got+1 {
							return fmt.Errorf("row %d: *string %q, *int %d, *int64 %d, text %q, *int32 %d; want %s", got+1, s, i, i64, d, i32, want)
						}
						got++
					}
					if got != 2 {
						return fmt.Errorf("%d rows before the error at n=3, want 2 (n=1 and n=2)", got)
					}
					return pgCode(rows.Err(), "22012")
				})
			})
		}
	}
}

// pgCode returns nil when err is a server error with the SQLSTATE code.
func pgCode(err error, code string) error {
	if err == nil {
		return errors.New("no error, want SQLSTATE " + code)
	}
	var pe *postgres.PGError
	if errors.As(err, &pe) && pe.Code == code {
		return nil
	}
	return fmt.Errorf("error %v, want SQLSTATE %s", err, code)
}
