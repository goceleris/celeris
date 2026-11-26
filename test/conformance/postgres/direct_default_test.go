//go:build postgres

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/postgres"
)

// database/sql (a Connector, which sql.Open returns, without WithStreaming) dials
// direct connections: the three freezes of a *sql.DB on the event loop (a paused
// streaming Rows, a deadline over a large result, a second statement while a Rows
// is open) do not happen. Each test is the reproducer of the freeze, with checks
// on what comes back. With CELERIS_CONFORMANCE_STREAMING=1 they would hang: the
// default is what they test, so they open the handle with sql.Open.

// openDirectDB opens a database/sql handle the default way.
func openDirectDB(t *testing.T, maxOpen int) *sql.DB {
	t.Helper()
	db, err := sql.Open(driverName, dsnFromEnv(t))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxOpen)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// withWatchdog runs f and fails the test, with every goroutine's stack, if it is
// still running after d: a hang must not wait for go test's own timeout.
func withWatchdog(t *testing.T, d time.Duration, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(d):
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("%s: still running after %v\n%s", what, d, buf[:n])
	}
}

// A second statement on a Tx while a Rows of that Tx is open returns its own
// result, and the first Rows still delivers every one of its rows, in order, each
// with its own marker.
func TestDefaultTxSecondStatementWhileRowsOpen(t *testing.T) {
	const total = 100000
	db := openDirectDB(t, 4)
	ctx := context.Background()
	withWatchdog(t, 90*time.Second, "tx second statement", func() {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Errorf("BeginTx: %v", err)
			return
		}
		defer func() { _ = tx.Rollback() }()
		r1, err := tx.QueryContext(ctx, fmt.Sprintf(
			"SELECT g, 'first-' || g || '-' || repeat('x', 40) FROM generate_series(1, %d) g", total))
		if err != nil {
			t.Errorf("first query: %v", err)
			return
		}
		defer func() { _ = r1.Close() }()
		next := 0
		readFirst := func() bool {
			var g int
			var m string
			if err := r1.Scan(&g, &m); err != nil {
				t.Errorf("first Rows, row %d: scan: %v", next+1, err)
				return false
			}
			next++
			if want := fmt.Sprintf("first-%d-%s", next, strings.Repeat("x", 40)); g != next || m != want {
				t.Errorf("first Rows, row %d: got (%d, %q), want (%d, %q)", next, g, m, next, want)
				return false
			}
			return true
		}
		if !r1.Next() {
			t.Errorf("first Rows has no row: %v", r1.Err())
			return
		}
		if !readFirst() {
			return
		}

		// The second statements, with the first Rows open and mid-way.
		type res struct {
			rows int
			err  error
		}
		out := make(chan res, 1)
		go func() {
			sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			r2, err := tx.QueryContext(sctx, "SELECT g, 'second-' || g FROM generate_series(1, 1000) g")
			if err != nil {
				out <- res{err: err}
				return
			}
			defer func() { _ = r2.Close() }()
			n := 0
			for r2.Next() {
				n++
				var g int
				var m string
				if err := r2.Scan(&g, &m); err != nil {
					out <- res{n, err}
					return
				}
				if want := fmt.Sprintf("second-%d", n); g != n || m != want {
					out <- res{n, fmt.Errorf("second Rows, row %d: got (%d, %q), want (%d, %q)", n, g, m, n, want)}
					return
				}
			}
			if _, err := tx.ExecContext(sctx, "SELECT 1"); err != nil {
				out <- res{n, fmt.Errorf("Exec: %w", err)}
				return
			}
			var one int
			if err := tx.QueryRowContext(sctx, "SELECT $1::int", 7).Scan(&one); err != nil || one != 7 {
				out <- res{n, fmt.Errorf("QueryRow: %d, %v", one, err)}
				return
			}
			out <- res{n, r2.Err()}
		}()
		select {
		case r := <-out:
			if r.err != nil || r.rows != 1000 {
				t.Errorf("second statement: %d rows, err = %v; want 1000 rows, no error", r.rows, r.err)
			}
		case <-time.After(20 * time.Second):
			t.Error("the second statement is still blocked after 20 s")
			return
		}

		// The first Rows delivers the rest, in order.
		for r1.Next() {
			if !readFirst() {
				return
			}
		}
		if err := r1.Err(); err != nil {
			t.Errorf("first Rows: Err after %d rows: %v", next, err)
		}
		if next != total {
			t.Errorf("first Rows delivered %d rows, want %d", next, total)
		}
		if err := tx.Commit(); err != nil {
			t.Errorf("Commit: %v", err)
		}
	})
}

// A caller that holds a result and does not read it (a paused consumer), or that
// reads it slowly, does not slow the other callers of the handle.
func TestDefaultPausedConsumerDoesNotAffectOthers(t *testing.T) {
	db := openDirectDB(t, 32)
	ctx := context.Background()
	const big = "SELECT g, repeat('x', 200) FROM generate_series(1, 300000) g"

	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var x int
			_ = db.QueryRowContext(ctx, "SELECT 1, pg_sleep(0.05)").Scan(&x, new(any))
		}()
	}
	wg.Wait()

	type stat struct {
		n, errs  int
		p50, max time.Duration
	}
	measure := func(d time.Duration) stat {
		var mu sync.Mutex
		var lat []time.Duration
		errs := 0
		stop := time.Now().Add(d)
		var wg sync.WaitGroup
		for g := 0; g < 16; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for time.Now().Before(stop) {
					st := time.Now()
					var x int
					qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
					err := db.QueryRowContext(qctx, "SELECT 1").Scan(&x)
					cancel()
					el := time.Since(st)
					mu.Lock()
					lat = append(lat, el)
					if err != nil {
						errs++
					}
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		return stat{len(lat), errs, lat[len(lat)/2], lat[len(lat)-1]}
	}
	base := measure(2 * time.Second)
	t.Logf("baseline: n=%d errs=%d p50=%v max=%v", base.n, base.errs, base.p50, base.max)
	if base.errs != 0 {
		t.Fatalf("baseline: %d failed calls", base.errs)
	}
	// The bounds are loose on purpose (the freeze is a call that waits for its
	// 10 s timeout or for ever, and the test may share a host with other work).
	check := func(name string, s stat) {
		t.Helper()
		t.Logf("%s: n=%d errs=%d p50=%v max=%v", name, s.n, s.errs, s.p50, s.max)
		if s.errs != 0 {
			t.Errorf("%s: %d calls failed", name, s.errs)
		}
		if s.max > 3*time.Second {
			t.Errorf("%s: max latency %v, baseline max %v", name, s.max, base.max)
		}
		if lim := 10*base.p50 + 20*time.Millisecond; s.p50 > lim {
			t.Errorf("%s: p50 %v is over %v (baseline p50 %v)", name, s.p50, lim, base.p50)
		}
	}

	t.Run("paused", func(t *testing.T) {
		withWatchdog(t, 60*time.Second, "paused consumer", func() {
			rows, err := db.QueryContext(ctx, big)
			if err != nil {
				t.Errorf("query: %v", err)
				return
			}
			rows.Next() // take one row, then stop iterating
			check("while one Rows is paused", measure(3*time.Second))
			_ = rows.Close()
			check("after Close", measure(time.Second))
		})
	})
	t.Run("slow", func(t *testing.T) {
		withWatchdog(t, 60*time.Second, "slow consumer", func() {
			rows, err := db.QueryContext(ctx, big)
			if err != nil {
				t.Errorf("query: %v", err)
				return
			}
			stop := make(chan struct{})
			consumed := make(chan int, 1)
			go func() {
				n := 0
				for rows.Next() {
					n++
					time.Sleep(200 * time.Microsecond)
					select {
					case <-stop:
						consumed <- n
						return
					default:
					}
				}
				consumed <- n
			}()
			check("while a consumer reads one row per 200us", measure(3*time.Second))
			close(stop)
			t.Logf("the consumer read %d rows", <-consumed)
			_ = rows.Close()
		})
	})
}

// A deadline that fires over a large result ends the call, with the context's
// error, and never hangs it; the handle is usable afterwards.
func TestDefaultDeadlineOverLargeResultDoesNotHang(t *testing.T) {
	const iterations = 200
	db := openDirectDB(t, 4)
	seed := uint64(time.Now().UnixNano())
	rng := rand.New(rand.NewPCG(seed, 1))
	t.Logf("seed %d", seed)
	var ok, ctxErr, tooBig, other int
	for i := 0; i < iterations; i++ {
		d := time.Duration(200+rng.IntN(14800)) * time.Microsecond
		var outcome string
		withWatchdog(t, 30*time.Second, fmt.Sprintf("iteration %d (deadline %v)", i, d), func() {
			ctx, cancel := context.WithTimeout(context.Background(), d)
			defer cancel()
			rows, err := db.QueryContext(ctx, "SELECT g, repeat('x', 200) FROM generate_series(1, 300000) g")
			if err == nil {
				c := 0
				for rows.Next() && c < 10 {
					c++
				}
				err = rows.Err()
				_ = rows.Close()
			}
			switch {
			case err == nil:
				outcome = "ok"
			case errors.Is(err, context.DeadlineExceeded):
				outcome = "ctx"
			case errors.Is(err, postgres.ErrResultTooBig):
				outcome = "toobig"
			default:
				outcome = "other: " + err.Error()
			}
		})
		switch {
		case outcome == "ok":
			ok++
		case outcome == "ctx":
			ctxErr++
		case outcome == "toobig":
			tooBig++
		default:
			other++
			if other <= 3 {
				t.Errorf("iteration %d (deadline %v): %s", i, d, outcome)
			}
		}
	}
	t.Logf("iterations=%d ok=%d deadline=%d toobig=%d other=%d", iterations, ok, ctxErr, tooBig, other)
	if ctxErr == 0 {
		t.Errorf("no iteration ended on its deadline: the test did not exercise it")
	}
	pctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var one int
	if err := db.QueryRowContext(pctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("the handle after the loop: %d, %v", one, err)
	}
}

// The result cap is a setting. The default (64 MiB) fails a larger result with
// ErrResultTooBig after the rows that fit; Connector.WithMaxResultBytes sets it
// either way; a Connector WithStreaming has no cap; the connection stays usable
// after the error.
func TestDefaultResultCapIsASetting(t *testing.T) {
	dsn := dsnFromEnv(t)
	open := func(t *testing.T, f func(*postgres.Connector) *postgres.Connector) *sql.DB {
		t.Helper()
		cn, err := postgres.NewConnector(dsn)
		if err != nil {
			t.Fatal(err)
		}
		db := sql.OpenDB(f(cn))
		db.SetMaxOpenConns(1) // one connection: the statement after an error runs on the same one
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	count := func(t *testing.T, db *sql.DB, q string) (n int, err error) {
		t.Helper()
		rows, err := db.QueryContext(context.Background(), q)
		if err != nil {
			return 0, err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			n++
		}
		return n, rows.Err()
	}
	usable := func(t *testing.T, db *sql.DB) {
		t.Helper()
		marker := fmt.Sprintf("after-%d", time.Now().UnixNano())
		var got string
		if err := db.QueryRow("SELECT $1::text", marker).Scan(&got); err != nil || got != marker {
			t.Fatalf("the connection after the error: %q, %v; want %q", got, err, marker)
		}
	}
	const small = "SELECT g, repeat('x', 1000) FROM generate_series(1, 5000) g" // about 5 MB
	const big = "SELECT g, repeat('x', 200) FROM generate_series(1, 350000) g"  // about 76 MB

	t.Run("default is 64 MiB", func(t *testing.T) {
		db := open(t, func(c *postgres.Connector) *postgres.Connector { return c })
		if n, err := count(t, db, small); err != nil || n != 5000 {
			t.Fatalf("5 MB: %d rows, %v", n, err)
		}
		n, err := count(t, db, big)
		if !errors.Is(err, postgres.ErrResultTooBig) || n == 0 || n >= 350000 {
			t.Fatalf("76 MB: %d rows, err = %v; want some rows and ErrResultTooBig", n, err)
		}
		usable(t, db)
	})
	t.Run("lowered", func(t *testing.T) {
		db := open(t, func(c *postgres.Connector) *postgres.Connector { return c.WithMaxResultBytes(1 << 20) })
		n, err := count(t, db, small)
		if !errors.Is(err, postgres.ErrResultTooBig) || n == 0 || n >= 5000 {
			t.Fatalf("5 MB under a 1 MiB cap: %d rows, err = %v", n, err)
		}
		usable(t, db)
		// One row larger than the cap fails the call itself, with no rows.
		if n, err := count(t, db, "SELECT repeat('x', 2000000)"); !errors.Is(err, postgres.ErrResultTooBig) || n != 0 {
			t.Fatalf("a 2 MB row under a 1 MiB cap: %d rows, err = %v", n, err)
		}
		usable(t, db)
	})
	t.Run("raised", func(t *testing.T) {
		db := open(t, func(c *postgres.Connector) *postgres.Connector { return c.WithMaxResultBytes(256 << 20) })
		if n, err := count(t, db, big); err != nil || n != 350000 {
			t.Fatalf("76 MB under a 256 MiB cap: %d rows, %v", n, err)
		}
		usable(t, db)
	})
	t.Run("streaming has none", func(t *testing.T) {
		db := open(t, func(c *postgres.Connector) *postgres.Connector { return c.WithStreaming().WithMaxResultBytes(1 << 20) })
		if n, err := count(t, db, small); err != nil || n != 5000 {
			t.Fatalf("5 MB on the loop under a 1 MiB cap: %d rows, %v", n, err)
		}
		usable(t, db)
	})
	t.Run("zero is refused", func(t *testing.T) {
		db := open(t, func(c *postgres.Connector) *postgres.Connector { return c.WithMaxResultBytes(0) })
		if err := db.Ping(); err == nil || !strings.Contains(err.Error(), "WithMaxResultBytes") {
			t.Fatalf("Ping with WithMaxResultBytes(0): %v, want an error that names it", err)
		}
	})
	t.Run("pool", func(t *testing.T) {
		p, err := postgres.Open(dsn, postgres.WithMaxOpen(1), postgres.WithMaxResultBytes(1<<20))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = p.Close() })
		rows, err := p.QueryContext(context.Background(), small)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for rows.Next() {
			n++
		}
		if err := rows.Err(); !errors.Is(err, postgres.ErrResultTooBig) || n == 0 || n >= 5000 {
			t.Fatalf("native direct Pool, 1 MiB cap: %d rows, err = %v", n, err)
		}
		_ = rows.Close()
	})
}
