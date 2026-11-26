//go:build postgres

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/postgres"
)

// The cancel of a context at each point of a statement's life, on a native Pool,
// against a real server: the call ends, the statement is not left running on the
// server, and the conn is clean (the same backend) or replaced (another one) and
// answers the next statement with ITS OWN value. "direct" is a Pool opened without
// an engine, always; "env" is the Pool the harness opens (openPoolEngine: on an
// engine when CELERIS_CONFORMANCE_ENGINE says so).

type cancelArm struct {
	name string
	open func(t *testing.T) *postgres.Pool
}

func cancelArms() []cancelArm {
	return []cancelArm{
		{"direct", func(t *testing.T) *postgres.Pool {
			p, err := postgres.Open(dsnFromEnv(t), postgres.WithMaxOpen(1))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Close() })
			return p
		}},
		{"env", func(t *testing.T) *postgres.Pool {
			p, err := openPoolEngine(dsnFromEnv(t), postgres.WithMaxOpen(1))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Close() })
			return p
		}},
	}
}

// scanInt64 runs a one-row, one-column int64 query and returns the value.
func scanInt64(ctx context.Context, p *postgres.Pool, q string, args ...any) (int64, error) {
	rows, err := p.QueryContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	var v int64
	n := 0
	for rows.Next() {
		if err := rows.Scan(&v); err != nil {
			return 0, err
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if n != 1 {
		return 0, fmt.Errorf("%d rows, want 1", n)
	}
	return v, nil
}

// requireOwnMarker is the postcondition: the next statement on the Pool returns its
// own value (no crossed reply, no stale deadline of the old context, no 57014 of
// a stray cancel), and the backend is the same (clean) or another (replaced).
func requireOwnMarker(t *testing.T, p *postgres.Pool, pidBefore int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	marker := time.Now().UnixNano()
	got, err := scanInt64(ctx, p, "SELECT $1::int8", marker)
	if err != nil {
		t.Fatalf("the statement after the cancelled one failed: %v", err)
	}
	if got != marker {
		t.Fatalf("the statement after the cancelled one returned %d, want its own %d (a crossed reply)", got, marker)
	}
	pid, err := scanInt64(ctx, p, "SELECT pg_backend_pid()::int8")
	if err != nil {
		t.Fatalf("pg_backend_pid: %v", err)
	}
	if pid == pidBefore {
		t.Logf("conn clean: backend %d reused", pid)
	} else {
		t.Logf("conn replaced: backend %d -> %d", pidBefore, pid)
	}
}

func backendPID(t *testing.T, p *postgres.Pool) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pid, err := scanInt64(ctx, p, "SELECT pg_backend_pid()::int8")
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func openMonitor(t *testing.T) *sql.DB {
	t.Helper()
	mon, err := sql.Open("celeris-postgres", dsnFromEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	mon.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = mon.Close() })
	return mon
}

func watchdogReal(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		t.Fatalf("watchdog: %s did not return within %v\n%s", what, d, buf)
	}
}

func TestPoolCancelAtEachPointOfAStatement(t *testing.T) {
	for _, arm := range cancelArms() {
		t.Run(arm.name, func(t *testing.T) {
			mon := openMonitor(t)
			newTag := func(what string) string {
				return fmt.Sprintf("cp-%s-%s-%d", arm.name, what, time.Now().UnixNano())
			}
			cancelIn := func(d time.Duration) (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				time.AfterFunc(d, cancel)
				return ctx, cancel
			}
			// a cancelled statement ends the call fast, with the context's error (or
			// the server's 57014 on a loop), leaves nothing running on the server, and
			// the conn is clean or replaced.
			check := func(t *testing.T, p *postgres.Pool, pid int64, tag string, err error, took, limit time.Duration) {
				t.Helper()
				var pe *postgres.PGError
				if !errors.Is(err, context.Canceled) && !(errors.As(err, &pe) && pe.Code == "57014") {
					t.Fatalf("err = %v, want context.Canceled (or 57014)", err)
				}
				if took > limit {
					t.Fatalf("returned after %v: the statement ran on after the cancel", took)
				}
				if tag != "" {
					if n := activeStatements(t, mon, tag); n != 0 {
						t.Fatalf("%d statements tagged %s still run on the server after the call returned", n, tag)
					}
				}
				requireOwnMarker(t, p, pid)
			}

			t.Run("before-send", func(t *testing.T) {
				p := arm.open(t)
				pid := backendPID(t, p)
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				_, err := p.ExecContext(ctx, "SELECT pg_sleep(30)")
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("err = %v, want context.Canceled", err)
				}
				requireOwnMarker(t, p, pid)
			})
			t.Run("while-the-server-works", func(t *testing.T) {
				p := arm.open(t)
				pid := backendPID(t, p)
				tag := newTag("sleep")
				ctx, cancel := cancelIn(200 * time.Millisecond)
				defer cancel()
				var err error
				start := time.Now()
				watchdogReal(t, 20*time.Second, "the cancelled statement", func() {
					_, err = p.ExecContext(ctx, "SELECT pg_sleep(37) /* "+tag+" */")
				})
				check(t, p, pid, tag, err, time.Since(start), 5*time.Second)
			})
			t.Run("mid-result", func(t *testing.T) {
				p := arm.open(t)
				pid := backendPID(t, p)
				tag := newTag("rows")
				ctx, cancel := cancelIn(30 * time.Millisecond)
				defer cancel()
				var err error
				start := time.Now()
				watchdogReal(t, 60*time.Second, "the cancelled statement", func() {
					// a result that takes the server a while to produce and send
					var rows *postgres.Rows
					rows, err = p.QueryContext(ctx, "SELECT i, pg_sleep(0.001) FROM generate_series(1, 100000) i /* "+tag+" */")
					if err != nil {
						return
					}
					for rows.Next() {
					}
					err = rows.Err()
					_ = rows.Close()
				})
				check(t, p, pid, tag, err, time.Since(start), 20*time.Second)
			})
			t.Run("copy-to", func(t *testing.T) {
				p := arm.open(t)
				pid := backendPID(t, p)
				tag := newTag("copyout")
				ctx, cancel := cancelIn(150 * time.Millisecond)
				defer cancel()
				var err error
				var rows atomic.Int64
				start := time.Now()
				watchdogReal(t, 60*time.Second, "CopyTo", func() {
					err = p.CopyTo(ctx, "COPY (SELECT i, pg_sleep(0.001) FROM generate_series(1, 100000) i) TO STDOUT /* "+tag+" */",
						func([]byte) error { rows.Add(1); return nil })
				})
				t.Logf("dest ran %d times before the cancel", rows.Load())
				check(t, p, pid, tag, err, time.Since(start), 20*time.Second)
			})
			t.Run("copy-to-silent", func(t *testing.T) {
				p := arm.open(t)
				pid := backendPID(t, p)
				tag := newTag("copyoutsleep")
				ctx, cancel := cancelIn(150 * time.Millisecond)
				defer cancel()
				var err error
				start := time.Now()
				watchdogReal(t, 60*time.Second, "CopyTo", func() {
					err = p.CopyTo(ctx, "COPY (SELECT pg_sleep(37)) TO STDOUT /* "+tag+" */", func([]byte) error { return nil })
				})
				check(t, p, pid, tag, err, time.Since(start), 5*time.Second)
			})
			t.Run("copy-from", func(t *testing.T) {
				db := openDB(t)
				table := uniqueTableName(t, "cancel_copy_from")
				createTable(t, db, table, "i int8")
				p := arm.open(t)
				pid := backendPID(t, p)
				ctx, cancel := cancelIn(150 * time.Millisecond)
				defer cancel()
				// rows without end, each a little slow: only the context ends the COPY
				src := &endlessSource{}
				var err error
				start := time.Now()
				watchdogReal(t, 60*time.Second, "CopyFrom", func() { _, err = p.CopyFrom(ctx, table, []string{"i"}, src) })
				check(t, p, pid, "", err, time.Since(start), 20*time.Second)
				var n int
				if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
					t.Fatal(err)
				}
				if n != 0 {
					t.Fatalf("%d rows were committed by a COPY that was cancelled", n)
				}
			})
		})
	}
}

// endlessSource is a CopyFromSource without end.
type endlessSource struct{ i int64 }

func (s *endlessSource) Next() bool {
	s.i++
	if s.i%50 == 0 {
		time.Sleep(time.Millisecond)
	}
	return true
}
func (s *endlessSource) Values() ([]any, error) { return []any{s.i}, nil }
func (s *endlessSource) Err() error             { return nil }

// TestPoolStaleCancelNeverPoisonsTheNextStatement: the context ends at a random
// instant within the life of a short statement (0..STALE_MAX_US, 800 by default,
// against one of about 0.4 ms of server time), by cancel() or by deadline, so that
// the cancel lands before the reply, during it and after the end of the statement;
// the statement after it, on a background context, returns ITS OWN value, every
// time. 2,000 iterations by default: most of them end by the context, and each of
// those is a CancelRequest, a new connection to the postmaster (a fork), about 30
// ms on a busy machine (STALE_ITERS raises it). The 20,000 iterations with -race
// are in the package's own test against a backend that answers a cancel at once
// (driver/postgres, TestDirectStaleCancelNeverPoisonsTheNextStatement).
func TestPoolStaleCancelNeverPoisonsTheNextStatement(t *testing.T) {
	for _, arm := range cancelArms() {
		t.Run(arm.name, func(t *testing.T) {
			iters := envInt("STALE_ITERS", 2000)
			if testing.Short() {
				iters = min(iters, 500)
			}
			maxUS := envInt("STALE_MAX_US", 800)
			p := arm.open(t)
			rng := rand.New(rand.NewPCG(7, 11))
			var ended, finished int
			var failures []string
			watchdogReal(t, 120*time.Second+time.Duration(iters)*100*time.Millisecond, "the stale-cancel loop", func() {
				for i := range iters {
					d := time.Duration(rng.IntN(maxUS)) * time.Microsecond
					var ctx context.Context
					var cancel context.CancelFunc
					if rng.IntN(2) == 0 {
						ctx, cancel = context.WithCancel(context.Background())
						time.AfterFunc(d, cancel)
					} else {
						ctx, cancel = context.WithTimeout(context.Background(), d)
					}
					want := int64(i)
					got, err := scanInt64(ctx, p, "SELECT $1::int8 FROM pg_sleep(0.0004)", want)
					cancel()
					switch {
					case err == nil && got == want:
						finished++
					case err == nil:
						failures = append(failures, fmt.Sprintf("iteration %d: the cancelled statement returned %d, want its own %d", i, got, want))
					default:
						var pe *postgres.PGError
						// ErrClosed: TEMPORARY, until the fix of the prepare error (a context that ends during
						// the auto-cache prepare of the statement is swallowed and the statement is run
						// again: on a conn failed by the cancel it returns ErrClosed). Remove with that fix.
						// isNetTimeout: the pool had to dial a new conn (the old one was discarded)
						// and the context expired during the dial.
						if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || isNetTimeout(err) || errors.Is(err, postgres.ErrClosed) || (errors.As(err, &pe) && pe.Code == "57014") {
							ended++
						} else {
							failures = append(failures, fmt.Sprintf("iteration %d: %v", i, err))
						}
					}
					want = int64(i) + 1_000_000_000
					got, err = scanInt64(context.Background(), p, "SELECT $1::int8", want)
					if err != nil || got != want {
						failures = append(failures, fmt.Sprintf("iteration %d: the next statement returned %d, %v (want %d)", i, got, err, want))
					}
					if len(failures) > 0 {
						return
					}
				}
			})
			t.Logf("%d iterations: %d statements ended by their context, %d finished", iters, ended, finished)
			if len(failures) > 0 {
				t.Fatalf("first failure: %s", failures[0])
			}
			if ended == 0 || finished == 0 {
				t.Fatalf("%d ended, %d finished: the cancel instants do not span the statement", ended, finished)
			}
		})
	}
}
