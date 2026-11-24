//go:build postgres

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/driver/postgres"
)

// TestStrayCancel: the cancel of an expired context must never land on the
// NEXT statement of the same conn. Each worker owns one conn and loops: run a
// short statement under a context that expires 0..N ms after it started (so
// the CancelRequest races the end of the statement), then run an innocent
// "SELECT $1::int8" on a background context. The innocent statement must never
// fail. Before the fix it failed with 57014 (query_canceled) now and then,
// because the CancelRequest was written on a new socket and the socket closed
// without waiting for the server to act on it.
//
// Two kinds of statement, because a Pool conn looks at its context only when
// bytes arrive: "select" (pg_sleep: no bytes until the end; reaches the cancel
// path on database/sql) and "notice" (a DO block that sends a NOTICE shortly
// before it ends, so a Pool conn wakes up after the context expired and
// cancels too). Knobs: STRAY_SECONDS (3 per arm and kind; the failing-first runs used 15-30), STRAY_SLEEP_MS (2),
// STRAY_WORKERS (2; more workers against the unfixed driver: its client-closed cancel sockets exhaust the ephemeral ports of a laptop above ~1000 cancels/s), STRAY_KIND (both|select|notice), STRAY_ARMS (all, or a
// comma list of pool-direct, pool-loop, database-sql).
func TestStrayCancel(t *testing.T) {
	dsn := dsnFromEnv(t)
	secs := envInt("STRAY_SECONDS", 3)
	if testing.Short() {
		t.Skip("stress test")
	}
	sleepMs := envInt("STRAY_SLEEP_MS", 2)
	workers := envInt("STRAY_WORKERS", 2)
	kinds := []string{"select", "notice"}
	if k := os.Getenv("STRAY_KIND"); k == "select" || k == "notice" {
		kinds = []string{k}
	}
	arms := []string{"pool-direct", "pool-loop", "database-sql"}
	if a := os.Getenv("STRAY_ARMS"); a != "" {
		arms = splitComma(a)
	}
	for _, mode := range arms {
		for _, kind := range kinds {
			t.Run(mode+"/"+kind, func(t *testing.T) {
				var run func(ctx context.Context, q string, arg any) error
				switch mode {
				case "database-sql":
					db, err := sql.Open("celeris-postgres", dsn)
					if err != nil {
						t.Fatal(err)
					}
					db.SetMaxOpenConns(workers)
					defer db.Close()
					run = func(ctx context.Context, q string, arg any) error {
						var rows *sql.Rows
						var err error
						if arg == nil {
							rows, err = db.QueryContext(ctx, q)
						} else {
							rows, err = db.QueryContext(ctx, q, arg)
						}
						if err != nil {
							return err
						}
						for rows.Next() {
						}
						err = rows.Err()
						_ = rows.Close()
						return err
					}
				default:
					opts := []postgres.Option{postgres.WithMaxOpen(workers)}
					if mode == "pool-loop" {
						opts = append(opts, postgres.WithEngine(celeris.New(celeris.Config{Addr: "127.0.0.1:0"})))
					}
					p, err := postgres.Open(dsn, opts...)
					if err != nil {
						t.Fatal(err)
					}
					defer p.Close()
					run = func(ctx context.Context, q string, arg any) error {
						var rows *postgres.Rows
						var err error
						if arg == nil {
							rows, err = p.QueryContext(ctx, q)
						} else {
							rows, err = p.QueryContext(ctx, q, arg)
						}
						if err != nil {
							return err
						}
						for rows.Next() {
						}
						err = rows.Err()
						_ = rows.Close()
						return err
					}
				}
				slow := func(ctx context.Context) error {
					if kind == "select" {
						return run(ctx, "SELECT pg_sleep($1::float8)", float64(sleepMs)/1000)
					}
					// 3/4 of the statement, a NOTICE (the response arrives in
					// pieces), then the last quarter.
					return run(ctx, fmt.Sprintf("DO $$ BEGIN PERFORM pg_sleep(%f); RAISE NOTICE 'x'; PERFORM pg_sleep(%f); END $$",
						float64(sleepMs)*0.75/1000, float64(sleepMs)*0.25/1000), nil)
				}
				var iters, cancelled, stray, other atomic.Int64
				var firstOther atomic.Value
				stop := time.Now().Add(time.Duration(secs) * time.Second)
				var wg sync.WaitGroup
				for w := range workers {
					wg.Add(1)
					go func() {
						defer wg.Done()
						rng := rand.New(rand.NewPCG(1, uint64(w)+2))
						for time.Now().Before(stop) {
							n := iters.Add(1)
							ctx, cancel := context.WithTimeout(context.Background(), time.Duration(rng.IntN(sleepMs+1))*time.Millisecond)
							if err := slow(ctx); err != nil {
								cancelled.Add(1)
							}
							cancel()
							if err := run(context.Background(), "SELECT $1::int8", n); err != nil {
								var pe *postgres.PGError
								if errors.As(err, &pe) && pe.Code == "57014" {
									stray.Add(1)
								} else {
									other.Add(1)
									firstOther.CompareAndSwap(nil, err.Error())
								}
							}
						}
					}()
				}
				wg.Wait()
				fmt.Printf("RESULT stray-cancel %s/%s: %d iterations, %d expired-context statements, %d innocent statements failed with 57014, %d with another error (%v)\n",
					mode, kind, iters.Load(), cancelled.Load(), stray.Load(), other.Load(), firstOther.Load())
				if stray.Load() > 0 || other.Load() > 0 {
					t.Fail()
				}
			})
		}
	}
}

func envInt(name string, def int) int {
	if s := os.Getenv(name); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			return n
		}
	}
	return def
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}
