//go:build postgres

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/postgres"
)

// A write whose context ends while the driver prepares it is not run.
//
// The driver prepares a cacheable statement it has not seen (Parse + Describe)
// before it runs it. When the context of the call ended during that step, the
// call used to carry on with the statement itself (Parse + Bind + Execute) under
// the context that had already ended, and the write committed although the
// caller was told context.DeadlineExceeded or context.Canceled.
//
// The test holds ACCESS EXCLUSIVE on the table with another session, so the
// Parse of the write (which takes a lock on the target table) waits. The lock is
// released the moment the context ends, so the cancel of the call is confirmed
// by the server and the prepare finishes: nothing but the call's own handling of
// the ended context decides whether the statement is sent. Each call has a SQL
// text of its own (a statement cache miss) and a unique marker. The assertions:
//
//   - no marker has two rows, the sequence of the table advanced once per row
//     (a statement never runs twice), and the counter of an UPDATE went up by at
//     most one;
//   - a call that returned an error left no row for its marker: it returned the
//     context's error, or the server's answer to the cancel (57014), never
//     ErrClosed.
//
// A write whose context ends AFTER its Execute was sent may still commit, as
// with any cancel; the lock makes sure the context ends before.
//
//	CELERIS_PG_PREPARE_WRITE_CALLS   calls per arm (default 40; 10 with -short)
func TestPrepareEndedWriteIsNeverSent(t *testing.T) {
	dsn := dsnFromEnv(t)
	calls := 40
	if testing.Short() {
		calls = 10
	}
	if s := os.Getenv("CELERIS_PG_PREPARE_WRITE_CALLS"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			t.Fatalf("CELERIS_PG_PREPARE_WRITE_CALLS=%q: want a positive integer", s)
		}
		calls = n
	}
	admin := openDB(t)
	admin.SetMaxOpenConns(2)
	bg := context.Background()
	for _, mode := range evictModes {
		for _, what := range []string{"insert", "insert-query", "update"} {
			for _, end := range []string{"deadline", "cancel"} {
				t.Run(mode.name+"/"+what+"/"+end, func(t *testing.T) {
					tab := uniqueTableName(t, "prep_wr")
					ctr := uniqueTableName(t, "prep_ctr")
					createTable(t, admin, tab, "id serial, m bigint")
					createTable(t, admin, ctr, "k bigint, n int")
					for i := 0; i < calls; i++ {
						if _, err := admin.ExecContext(bg, fmt.Sprintf("INSERT INTO %s VALUES (%d, 0)", ctr, i)); err != nil {
							t.Fatal(err)
						}
					}
					pool := mode.open(t, dsn, 1)
					defer pool.Close()
					// the conn is open and the pool is warm; the statements below miss
					// the statement cache
					if err := pool.Exec(bg, "INSERT INTO "+tab+"(m) VALUES ($1)", int64(-1)); err != nil {
						t.Fatalf("warm-up: %v", err)
					}
					lockTab := tab
					if what == "update" {
						lockTab = ctr
					}
					erroredMarker := make([]bool, calls)
					outcomes := map[string]int{}
					for i := 0; i < calls; i++ {
						tx, err := admin.BeginTx(bg, nil)
						if err != nil {
							t.Fatal(err)
						}
						if _, err := tx.ExecContext(bg, "LOCK TABLE "+lockTab+" IN ACCESS EXCLUSIVE MODE"); err != nil {
							_ = tx.Rollback()
							t.Fatal(err)
						}
						var ctx context.Context
						var cancel context.CancelFunc
						want := context.Canceled
						if end == "deadline" {
							ctx, cancel = context.WithTimeout(bg, 20*time.Millisecond)
							want = context.DeadlineExceeded
						} else {
							ctx, cancel = context.WithCancel(bg)
							time.AfterFunc(20*time.Millisecond, cancel)
						}
						released := make(chan struct{})
						go func() {
							<-ctx.Done()
							_ = tx.Rollback() // releases the lock
							close(released)
						}()
						err2 := callWithin(t, fmt.Sprintf("call %d", i), func() error {
							switch what {
							case "insert":
								return pool.Exec(ctx, fmt.Sprintf("INSERT INTO %s(m) VALUES ($1) -- %d", tab, i), int64(i))
							case "insert-query":
								var m int64
								return queryOne(ctx, pool, fmt.Sprintf("WITH ins AS (INSERT INTO %s(m) VALUES ($1) RETURNING m) SELECT m FROM ins -- %d", tab, i), []any{int64(i)}, &m)
							}
							return pool.Exec(ctx, fmt.Sprintf("UPDATE %s SET n = n + 1 WHERE k = $1 -- %d", ctr, i), int64(i))
						})
						<-released
						cancel()
						var pe *postgres.PGError
						switch {
						case err2 == nil:
							outcomes["ok"]++
						case errors.Is(err2, postgres.ErrClosed):
							t.Fatalf("call %d: %v: ErrClosed in place of the context's error", i, err2)
						case errors.Is(err2, want):
							outcomes["context error"]++
							erroredMarker[i] = true
						case errors.As(err2, &pe) && pe.Code == "57014":
							outcomes["57014"]++
							erroredMarker[i] = true
						default:
							t.Fatalf("call %d: error %v (%T): not the context's (%v) nor the server's answer to the cancel", i, err2, err2, want)
						}
					}
					// a statement in flight on the server has finished by now
					time.Sleep(300 * time.Millisecond)
					ctx, cancel := context.WithTimeout(bg, 30*time.Second)
					defer cancel()
					rowsOf := func(q string) map[int64]int64 {
						out := map[int64]int64{}
						rows, err := admin.QueryContext(ctx, q)
						if err != nil {
							t.Fatalf("%s: %v", q, err)
						}
						defer func() { _ = rows.Close() }()
						for rows.Next() {
							var k, c int64
							if err := rows.Scan(&k, &c); err != nil {
								t.Fatal(err)
							}
							out[k] = c
						}
						return out
					}
					var executed int
					var withErr int
					if what == "update" {
						for k, n := range rowsOf("SELECT k, n FROM " + ctr) {
							if n > 1 {
								t.Fatalf("marker %d: the UPDATE ran %d times", k, n)
							}
							if n == 1 {
								executed++
								if erroredMarker[k] {
									withErr++
								}
							}
						}
					} else {
						for m, c := range rowsOf("SELECT m, count(*) FROM " + tab + " WHERE m >= 0 GROUP BY m") {
							if c > 1 {
								t.Fatalf("marker %d: %d rows: the statement ran %d times", m, c, c)
							}
							executed++
							if erroredMarker[m] {
								withErr++
							}
						}
						var seq int64
						if err := admin.QueryRowContext(ctx, "SELECT last_value FROM "+tab+"_id_seq").Scan(&seq); err != nil {
							t.Fatal(err)
						}
						if seq > int64(executed)+1 { // +1: the warm-up row
							t.Fatalf("the sequence is at %d for %d rows (+1 warm-up): a statement consumed a value without leaving a row (it ran twice, or ran and was undone)", seq, executed)
						}
					}
					t.Logf("%d calls: outcomes %v; %d markers executed, %d of them from a call that returned an error", calls, outcomes, executed, withErr)
					if withErr != 0 {
						t.Fatalf("%d calls returned an error although their write ran: the statement was sent after its context ended", withErr)
					}
				})
			}
		}
	}
}
