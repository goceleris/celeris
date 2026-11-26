//go:build postgres

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/postgres"
)

// A call whose context ends while the driver prepares the statement (the Parse +
// Describe of a cacheable statement it has not seen) returns an error that says
// why: the context's, or the server's. The statements here are all new (a text
// of their own per call), so every call prepares, under a deadline or a cancel
// of 0.02-2 ms that lands anywhere between the first write and the reply. The
// cancel of an ended context can leave its conn closed (the cancel exchange did
// not finish); the call used to run on the closed conn after the failed prepare
// and return postgres.ErrClosed in place of the context's error.
//
// Per call the test asserts the outcome is one of: success; the error of the
// context that ended it (DeadlineExceeded for a deadline, Canceled for a
// cancel); a server error (*postgres.PGError, e.g. 57014); a network timeout
// (the pool had to dial and the deadline passed during the dial). ErrClosed, or
// anything else, fails the test.
//
// The statements run on a pool of their own (see TestStatementCacheEvictionStress
// for why a stray cancel needs a conn of its own).
//
//	CELERIS_PG_PREPARE_CALLS   calls per arm (default 300; 60 with -short)
func TestPrepareEndedByTheContextReportsTheContext(t *testing.T) {
	dsn := dsnFromEnv(t)
	calls := 300
	if testing.Short() {
		calls = 60
	}
	if s := os.Getenv("CELERIS_PG_PREPARE_CALLS"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			t.Fatalf("CELERIS_PG_PREPARE_CALLS=%q: want a positive integer", s)
		}
		calls = n
	}
	db := openDB(t)
	for _, mode := range evictModes {
		for _, kind := range []string{"query", "exec"} {
			for _, end := range []string{"deadline", "cancel"} {
				t.Run(mode.name+"/"+kind+"/"+end, func(t *testing.T) {
					table := uniqueTableName(t, "prep_ctx")
					createTable(t, db, table, "w int, m bigint, k int")
					pool := mode.open(t, dsn, 3)
					defer pool.Close()
					warmPool(t, pool, 3)
					var (
						wg                                 sync.WaitGroup
						next                               atomic.Int64
						okN, ctxN, srvN, timeoutN, closedN atomic.Int64
						mu                                 sync.Mutex
						bad                                string
						errored                            = make([]atomic.Bool, calls+1) // per marker: the call returned an error
					)
					for w := 0; w < 3; w++ { // as many callers as conns: a call does not wait for one
						wg.Add(1)
						go func(w int) {
							defer wg.Done()
							for {
								n := next.Add(1)
								if n > int64(calls) {
									return
								}
								d := 20*time.Microsecond + time.Duration(n*7919%1981)*time.Microsecond // 0.02-2 ms
								var ctx context.Context
								var cancel context.CancelFunc
								want := context.DeadlineExceeded
								if end == "deadline" {
									ctx, cancel = context.WithTimeout(context.Background(), d)
								} else {
									want = context.Canceled
									ctx, cancel = context.WithCancel(context.Background())
									time.AfterFunc(d, cancel)
								}
								err := callWithin(t, fmt.Sprintf("call %d (%s, %s)", n, kind, end), func() error {
									if kind == "query" {
										q := fmt.Sprintf("SELECT $1::int8 AS p%d_%d", w, n)
										var m int64
										return queryOne(ctx, pool, q, []any{n}, &m)
									}
									q := fmt.Sprintf("INSERT INTO %s(w, m, k) VALUES ($1, $2, %d)", table, n)
									return pool.Exec(ctx, q, w, n)
								})
								cancel()
								errored[n].Store(err != nil)
								var pe *postgres.PGError
								switch {
								case err == nil:
									okN.Add(1)
								case errors.Is(err, postgres.ErrClosed):
									closedN.Add(1)
									mu.Lock()
									if bad == "" {
										bad = fmt.Sprintf("call %d (%s, %s, %v): %v: ErrClosed in place of the context's error", n, kind, end, d, err)
									}
									mu.Unlock()
								case errors.Is(err, want):
									ctxN.Add(1)
								case errors.As(err, &pe):
									srvN.Add(1)
								case isNetTimeout(err):
									timeoutN.Add(1)
								default:
									mu.Lock()
									if bad == "" {
										bad = fmt.Sprintf("call %d (%s, %s, %v): error %v (%T) is not the context's (%v), a server error or a network timeout", n, kind, end, d, err, err, want)
									}
									mu.Unlock()
								}
							}
						}(w)
					}
					wg.Wait()
					t.Logf("%d calls: ok %d, context error %d, server error %d, network timeout %d, ErrClosed %d", calls, okN.Load(), ctxN.Load(), srvN.Load(), timeoutN.Load(), closedN.Load())
					if bad != "" {
						t.Fatal(bad)
					}
					if kind == "exec" {
						// A write that ran, ran once: never two rows for a marker. A call
						// that returned the context's error may have committed when its
						// context ended after the Execute was sent (the ordinary ambiguity
						// of a cancel); TestPrepareEndedWriteIsNeverSent is the strict test.
						ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
						defer cancel()
						var dup, withRow int64
						if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FILTER (WHERE c > 1), count(*) FROM (SELECT m, count(*) AS c FROM %s GROUP BY m) s", table)).Scan(&dup, &withRow); err != nil {
							t.Fatalf("count: %v", err)
						}
						if dup != 0 {
							t.Fatalf("%d markers have more than one row: a statement ran twice", dup)
						}
						rows, err := db.QueryContext(ctx, fmt.Sprintf("SELECT m FROM %s", table))
						if err != nil {
							t.Fatalf("select: %v", err)
						}
						var erroredWithRow int
						for rows.Next() {
							var m int64
							if err := rows.Scan(&m); err == nil && m >= 1 && m <= int64(calls) && errored[m].Load() {
								erroredWithRow++
							}
						}
						_ = rows.Close()
						t.Logf("%d markers have a row, %d of them from a call that returned an error", withRow, erroredWithRow)
					}
					// The pool is healthy afterwards.
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					var m int64
					if err := queryOne(ctx, pool, "SELECT $1::int8", []any{int64(42)}, &m); err != nil || m != 42 {
						t.Fatalf("the pool after the run: m=%d err=%v", m, err)
					}
				})
			}
		}
	}
}

// warmPool opens n conns of pool, so that the short contexts of the test do not
// spend themselves on dials.
func warmPool(t *testing.T, pool stressDB, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var m int64
			errs <- queryOne(ctx, pool, "SELECT pg_sleep(0.05)::text IS NOT NULL, $1::int8", []any{int64(1)}, new(bool), &m)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("warm-up: %v", err)
		}
	}
}

// prepareCallDeadline is the longest one call of these tests may take. A call
// under a context of a few milliseconds ends within a second even when its
// cancel is not confirmed (the cancel exchange gives up after one second).
const prepareCallDeadline = 15 * time.Second

var errCallHung = errors.New("the call did not return")

// callWithin runs f on its own goroutine and returns its error. When f has not
// returned after prepareCallDeadline it logs a goroutine dump, fails the test
// and returns errCallHung: a hung conn fails the test within seconds, not at
// the timeout of the whole package. Safe to call from a goroutine of the test.
func callWithin(t *testing.T, what string, f func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- f() }()
	select {
	case err := <-done:
		return err
	case <-time.After(prepareCallDeadline):
		dumpGoroutines(t)
		t.Errorf("%s: no result after %v (hung)", what, prepareCallDeadline)
		return errCallHung
	}
}
