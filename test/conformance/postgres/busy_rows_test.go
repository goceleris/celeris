//go:build postgres

package postgres_test

// A second statement on a connection that has an open, still-streaming Rows.
//
// database/sql lets a caller do it on a Tx and on a sql.Conn (it only pins the
// connection), and forgetting to close a Rows inside a Tx before the next
// statement is one of the commonest mistakes in Go database code. pgx answers
// it with "conn busy" and lib/pq with an error. On the event loop the driver
// used to answer it with a deadlock: the loop worker sits in the row delivery
// of the first statement (the consumer is not reading), holding the conn's
// receive lock, and the caller of the second statement sits in a plain lock
// wait for that same lock, with no way for its ctx, db.Close or anything else
// to break it.
//
// Every case runs against a real server, with a watchdog so that a hang fails
// the test instead of the package. The carriers are: a database/sql Tx, a
// database/sql Conn, and the native Pool's Tx. Direct mode (a bare postgres.Open)
// is one of the CELERIS_CONFORMANCE_ENGINE arms: the same program must go on
// working there.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/postgres"
)

// busyStreamQ is a result far larger than the driver's streaming threshold (64
// rows) and than what the socket buffers hold, so the first statement is still
// streaming (the server has not finished sending it) while its consumer is paused.
const (
	// Row 100 takes 0.2 s: the first rows reach the caller's sync poll, which finds the
	// reply unfinished and hands it to the loop, so that the first statement
	// streams on a fast server too (a result up to 64 MiB is otherwise buffered whole
	// by that poll when the server keeps ahead of it).
	busyStreamQ    = "SELECT g, repeat('x', 200) || g::text || CASE WHEN g = 100 THEN (SELECT '' FROM pg_sleep(0.2)) ELSE '' END FROM generate_series(1, 5000) g"
	busyStreamRows = 5000
	// busyBound is how fast the busy error must come back: no lock wait, no
	// round trip.
	busyBound = 100 * time.Millisecond
)

// busyRows is the part of *sql.Rows and *postgres.Rows the cases use.
type busyRows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close() error
}

// busyEnv is one carrier: a connection with the first statement's Rows open.
type busyEnv struct {
	name string
	r1   busyRows
	// the statements a caller can issue as the second one
	exec     func(ctx context.Context, q string) error
	query    func(ctx context.Context, q string) (busyRows, error)
	prepare  func(ctx context.Context, q string) error // Prepare, then Close the statement
	ping     func(ctx context.Context) error           // nil when the carrier has none
	savept   func(ctx context.Context) error           // nil when the carrier has none
	commit   func() error
	rollback func() error
	closeAll func() error // db.Close / Pool.Close
	// ctxCancel is the cancel func of the ctx the first statement ran under.
	ctxCancel context.CancelFunc
	// direct is set for the native Pool in direct mode, which buffers the whole
	// first result before it returns: there a second statement while the Rows is
	// open works, as it always did, and must go on working.
	direct bool
}

// busyWatch runs fn and fails the test when it has not returned after d. The
// goroutine of a call that hangs is left behind (there is nothing to stop it):
// the stacks go to the log, which is what the deadlock looks like.
func busyWatch[T any](t *testing.T, what string, d time.Duration, fn func() T) (T, time.Duration) {
	t.Helper()
	type res struct {
		v  T
		el time.Duration
	}
	ch := make(chan res, 1)
	go func() {
		st := time.Now()
		v := fn()
		ch <- res{v, time.Since(st)}
	}()
	select {
	case r := <-ch:
		return r.v, r.el
	case <-time.After(d):
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		t.Logf("WATCHDOG: %s still blocked after %v; goroutines:\n%s", what, d, trimStacks(string(buf)))
		t.Fatalf("WATCHDOG: %s still blocked after %v (a hang)", what, d)
		var zero T
		return zero, 0
	}
}

// trimStacks keeps the goroutines that are inside the driver or database/sql.
func trimStacks(s string) string {
	var out []string
	for _, g := range strings.Split(s, "\n\n") {
		if strings.Contains(g, "driver/postgres") || strings.Contains(g, "driver/internal/eventloop") {
			out = append(out, g)
		}
	}
	return strings.Join(out, "\n\n")
}

// busyOpenSQL opens a database/sql handle on one connection.
func busyOpenSQL(t *testing.T) *sql.DB {
	t.Helper()
	db := openDB(t)
	db.SetMaxOpenConns(4)
	return db
}

// busyReadFirst reads the first row of r1 and checks its marker.
func busyReadFirst(t *testing.T, r busyRows) {
	t.Helper()
	if !r.Next() {
		t.Fatalf("first statement: Next = false, Err = %v", r.Err())
	}
	busyCheckRow(t, r, 1)
}

func busyCheckRow(t *testing.T, r busyRows, want int64) {
	t.Helper()
	var g int64
	var s string
	if err := r.Scan(&g, &s); err != nil {
		t.Fatalf("first statement: Scan row %d: %v", want, err)
	}
	if g != want || s != strings.Repeat("x", 200)+fmt.Sprint(want) {
		t.Fatalf("first statement: row marker = (%d, %.12q...), want row %d", g, s, want)
	}
}

// busyDrain reads r1 to the end (rows from..busyStreamRows), checking every row's marker.
func busyDrain(t *testing.T, r busyRows, from int64) {
	t.Helper()
	n := from
	for r.Next() {
		n++
		busyCheckRow(t, r, n)
	}
	if err := r.Err(); err != nil {
		t.Fatalf("first statement: iteration ended with %v after row %d", err, n)
	}
	if n != busyStreamRows {
		t.Fatalf("first statement: got %d rows, want %d", n, busyStreamRows)
	}
}

// busyCheckMarkers runs a small query on the carrier and checks it returns its
// own rows, not a leftover of the first statement.
func busyCheckMarkers(t *testing.T, env *busyEnv, tag string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, _ := busyWatch(t, tag+": next query", 15*time.Second, func() error {
		r, err := env.query(ctx, "SELECT 'm-' || g::text, g FROM generate_series(1, 3) g")
		if err != nil {
			return err
		}
		defer func() { _ = r.Close() }()
		for want := int64(1); want <= 3; want++ {
			if !r.Next() {
				return fmt.Errorf("next query: Next = false at row %d, Err = %v", want, r.Err())
			}
			var m string
			var g int64
			if err := r.Scan(&m, &g); err != nil {
				return fmt.Errorf("next query: Scan: %w", err)
			}
			if m != fmt.Sprintf("m-%d", want) || g != want {
				return fmt.Errorf("next query: row %d = (%q, %d): not its own rows", want, m, g)
			}
		}
		if r.Next() {
			return fmt.Errorf("next query: more than 3 rows")
		}
		return r.Err()
	})
	if rows != nil {
		t.Fatalf("%s: %v", tag, rows)
	}
}

// busyClose runs the carrier's final close under a watchdog and logs its latency.
func busyClose(t *testing.T, env *busyEnv, bound time.Duration) time.Duration {
	t.Helper()
	_, el := busyWatch(t, env.name+": close", bound, func() error {
		if env.ctxCancel != nil {
			// database/sql closes the Rows a cancelled ctx belongs to.
			env.ctxCancel()
		}
		if env.rollback != nil {
			_ = env.rollback()
		}
		return env.closeAll()
	})
	return el
}

// busyOpenSQLTx starts a Tx and the first statement.
func busyOpenSQLTx(t *testing.T) *busyEnv {
	db := busyOpenSQL(t)
	ctx, cancel := context.WithCancel(context.Background())
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	r1, err := tx.QueryContext(ctx, busyStreamQ)
	if err != nil {
		t.Fatal(err)
	}
	env := &busyEnv{
		name: "sql.Tx", r1: r1, ctxCancel: cancel,
		exec: func(ctx context.Context, q string) error { _, err := tx.ExecContext(ctx, q); return err },
		query: func(ctx context.Context, q string) (busyRows, error) {
			r, err := tx.QueryContext(ctx, q)
			if err != nil {
				return nil, err
			}
			return r, nil
		},
		prepare: func(ctx context.Context, q string) error {
			st, err := tx.PrepareContext(ctx, q)
			if err != nil {
				return err
			}
			return st.Close()
		},
		commit:   tx.Commit,
		rollback: tx.Rollback,
		closeAll: db.Close,
	}
	return env
}

// busyOpenSQLConn starts a sql.Conn and the first statement.
func busyOpenSQLConn(t *testing.T) *busyEnv {
	db := busyOpenSQL(t)
	ctx, cancel := context.WithCancel(context.Background())
	c, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r1, err := c.QueryContext(ctx, busyStreamQ)
	if err != nil {
		t.Fatal(err)
	}
	env := &busyEnv{
		name: "sql.Conn", r1: r1, ctxCancel: cancel,
		exec: func(ctx context.Context, q string) error { _, err := c.ExecContext(ctx, q); return err },
		query: func(ctx context.Context, q string) (busyRows, error) {
			r, err := c.QueryContext(ctx, q)
			if err != nil {
				return nil, err
			}
			return r, nil
		},
		prepare: func(ctx context.Context, q string) error {
			st, err := c.PrepareContext(ctx, q)
			if err != nil {
				return err
			}
			return st.Close()
		},
		ping: c.PingContext,
		// c.Close is not part of it: database/sql's Conn.Close waits for the open
		// Rows to be closed (its closemu), by design, and a case that leaves the
		// first Rows unread ends it by cancelling the ctx (busyClose).
		closeAll: db.Close,
	}
	return env
}

// busyOpenNativeTx starts a native Pool Tx and the first statement.
func busyOpenNativeTx(t *testing.T) *busyEnv {
	dsn := dsnFromEnv(t)
	p, err := openPoolEngine(dsn, postgres.WithMaxOpen(4))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	tx, err := p.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	r1, err := tx.QueryContext(ctx, busyStreamQ)
	if err != nil {
		t.Fatal(err)
	}
	return &busyEnv{
		name: "native Tx", r1: r1, ctxCancel: cancel, direct: !busyPoolOnLoop(),
		exec: func(ctx context.Context, q string) error { _, err := tx.ExecContext(ctx, q); return err },
		query: func(ctx context.Context, q string) (busyRows, error) {
			r, err := tx.QueryContext(ctx, q)
			if err != nil {
				return nil, err
			}
			return r, nil
		},
		savept:   func(ctx context.Context) error { return tx.Savepoint(ctx, "sp1") },
		commit:   tx.Commit,
		rollback: tx.Rollback,
		closeAll: p.Close,
	}
}

// busyPoolOnLoop reports whether the native Pool of this CELERIS_CONFORMANCE_ENGINE
// arm runs on the event loop (and streams a large result). Of the arms that
// bind the Pool to a server, only "default" does (measured: the others, the
// started ones and the explicit epoll, run the Pool in direct mode, which
// buffers the whole result); a bare Open is direct as well.
func busyPoolOnLoop() bool {
	return os.Getenv("CELERIS_CONFORMANCE_ENGINE") == "default"
}

// busyCarriers lists, for each carrier, the second statements it has (a skipped
// subtest is not coverage, and the conformance job rejects any SKIP line, so a
// combination that does not exist is not listed instead of skipped).
var busyCarriers = []struct {
	name    string
	open    func(*testing.T) *busyEnv
	seconds []string
}{
	{"sqlTx", busyOpenSQLTx, []string{"Exec", "Query", "Prepare"}},
	{"sqlConn", busyOpenSQLConn, []string{"Exec", "Query", "Prepare", "Ping"}},
	{"nativeTx", busyOpenNativeTx, []string{"Exec", "Query", "Savepoint"}},
}

func busyHas(list []string, name string) bool {
	for _, n := range list {
		if n == name {
			return true
		}
	}
	return false
}

// busySeconds are the second statements.
var busySeconds = []struct {
	name string
	run  func(ctx context.Context, env *busyEnv) error
}{
	{"Exec", func(ctx context.Context, env *busyEnv) error { return env.exec(ctx, "SELECT 1") }},
	{"Query", func(ctx context.Context, env *busyEnv) error {
		r, err := env.query(ctx, "SELECT 2")
		if r != nil {
			_ = r.Close()
		}
		return err
	}},
	{"Prepare", func(ctx context.Context, env *busyEnv) error {
		return env.prepare(ctx, "SELECT $1::int")
	}},
	{"Ping", func(ctx context.Context, env *busyEnv) error {
		return env.ping(ctx)
	}},
	{"Savepoint", func(ctx context.Context, env *busyEnv) error {
		return env.savept(ctx)
	}},
}

// TestBusyConnSecondStatement is the reproducer: the second statement on a
// connection whose first statement is still streaming. It must come back at
// once with an error a caller can act on, and then the first statement must
// still be usable, to its last row, and the connection must be reusable.
func TestBusyConnSecondStatement(t *testing.T) {
	for _, car := range busyCarriers {
		for _, sec := range busySeconds {
			if !busyHas(car.seconds, sec.name) {
				continue
			}
			t.Run(car.name+"/"+sec.name, func(t *testing.T) {
				env := car.open(t)
				defer func() { busyClose(t, env, 15*time.Second) }()
				busyReadFirst(t, env.r1)

				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				err, el := busyWatch(t, car.name+" second "+sec.name, 15*time.Second, func() error { return sec.run(ctx, env) })
				if env.direct {
					// Direct mode: the first result is buffered whole, the second
					// statement is served, and the first Rows is intact.
					if err != nil {
						t.Fatalf("direct mode: second %s while the first Rows is open: %v (it worked before the guard)", sec.name, err)
					}
					busyDrain(t, env.r1, 1)
					if err := env.r1.Close(); err != nil {
						t.Fatalf("first statement Close: %v", err)
					}
					busyCheckMarkers(t, env, "after the first statement")
					return
				}
				if !isRowsOpen(err) {
					t.Fatalf("second %s on a conn with an open streaming Rows: err = %v, want the busy error (ErrRowsOpen)", sec.name, err)
				}
				if el > busyBound {
					t.Fatalf("second %s took %v to return the busy error, want < %v", sec.name, el, busyBound)
				}
				t.Logf("%s %s: %v in %v", car.name, sec.name, err, el)

				// The first statement is untouched: every remaining row, with its marker.
				busyDrain(t, env.r1, 1)
				if err := env.r1.Close(); err != nil {
					t.Fatalf("first statement Close: %v", err)
				}
				// The connection is reusable and the next query returns its own rows.
				busyCheckMarkers(t, env, "after the first statement")
			})
		}
	}
}

// TestBusyConnCloseThenNext is the way out the error asks for: Close the Rows
// (without reading it to the end), then the next statement on the same Tx / Conn
// succeeds and returns its own rows, with the first statement's unread rows
// not leaking into it.
func TestBusyConnCloseThenNext(t *testing.T) {
	for _, car := range busyCarriers {
		t.Run(car.name, func(t *testing.T) {
			env := car.open(t)
			defer func() { busyClose(t, env, 15*time.Second) }()
			busyReadFirst(t, env.r1)
			_, _ = busyWatch(t, "Close of the unread streaming Rows", 30*time.Second, func() error { return env.r1.Close() })
			busyCheckMarkers(t, env, "after Close")
			// And once more with an Exec, which has a command tag, not rows.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err, _ := busyWatch(t, "Exec after Close", 10*time.Second, func() error { return env.exec(ctx, "SELECT 1") }); err != nil {
				t.Fatalf("Exec after Close: %v", err)
			}
		})
	}
}

// TestBusyConnFinishWithRowsOpen: Commit and Rollback with a Rows still open
// return within a bound (database/sql closes the Rows first, as documented; the
// native Tx returns the busy error or finishes, but does not hang), and the
// connection is not left wedged.
func TestBusyConnFinishWithRowsOpen(t *testing.T) {
	for _, car := range []string{"sqlTx", "nativeTx"} {
		for _, how := range []string{"Rollback", "Commit"} {
			t.Run(car+"/"+how, func(t *testing.T) {
				var env *busyEnv
				for _, c := range busyCarriers {
					if c.name == car {
						env = c.open(t)
					}
				}
				defer func() { busyClose(t, env, 15*time.Second) }()
				busyReadFirst(t, env.r1)
				fn := env.rollback
				if how == "Commit" {
					fn = env.commit
				}
				err, el := busyWatch(t, car+" "+how+" with a Rows open", 30*time.Second, func() error { return fn() })
				t.Logf("%s %s with an open Rows: err = %v in %v", car, how, err, el)
				switch {
				case car == "nativeTx" && how == "Commit" && !env.direct:
					// The Rows is still open: Commit says so, sends nothing and leaves the
					// Tx open, so that the caller can close the Rows and commit again, or
					// roll back.
					if !isRowsOpen(err) {
						t.Fatalf("native Tx Commit with a streaming Rows open: err = %v, want ErrRowsOpen", err)
					}
					if err := busyOnly(t, env, "Rollback after the refused Commit", env.rollback); err != nil {
						t.Fatalf("Rollback after the refused Commit: %v", err)
					}
				case err != nil:
					// database/sql closes the Rows first and finishes; a direct Pool has
					// buffered the result; a native Rollback discards the conn: all nil.
					t.Fatalf("%s %s with an open Rows: %v, want nil", car, how, err)
				}
				if car == "nativeTx" && how == "Rollback" && !env.direct {
					// The conn was discarded under the open Rows: it must end with an
					// error, not with a clean end that makes a truncated result look
					// complete.
					n := 0
					_, _ = busyWatch(t, "drain after the discarding Rollback", 30*time.Second, func() int {
						for env.r1.Next() {
							n++
						}
						return n
					})
					rerr := env.r1.Err()
					if rerr == nil || rerr == io.EOF || !errors.Is(rerr, postgres.ErrClosed) || !errors.Is(rerr, io.ErrUnexpectedEOF) {
						t.Fatalf("native Tx Rollback with a streaming Rows open: the Rows ended after %d rows with Err = %v, want an error matching ErrClosed and io.ErrUnexpectedEOF", n, rerr)
					}
				}
				// The Rows the finish left behind is closed or closable, bounded.
				_, _ = busyWatch(t, car+" Rows.Close after "+how, 30*time.Second, func() error { return env.r1.Close() })
			})
		}
	}
}

// busyOnly runs fn under a watchdog.
func busyOnly(t *testing.T, env *busyEnv, what string, fn func() error) error {
	t.Helper()
	err, _ := busyWatch(t, env.name+": "+what, 30*time.Second, fn)
	return err
}

// busyIters is the number of iterations of the cancel loops: n (300) in the always-on
// path, CELERIS_BUSY_ITERS (3000 for the acceptance runs, with and without -race) when set.
func busyIters(n int) int {
	if v, err := strconv.Atoi(os.Getenv("CELERIS_BUSY_ITERS")); err == nil && v > 0 {
		return v
	}
	return n
}

// busyIterQ is a streaming result of its own size (thousands of rows: still
// arriving when the statement after it starts), for the cancel loops.
const busyIterQ = "SELECT g, repeat('x', 200) || g::text FROM generate_series(1, 2000) g"

// busyIterCarrier runs one statement of the cancel loops on a database/sql Conn or Tx.
type busyIterCarrier struct {
	query func(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	done  func()
}

func busyOpenIterCarrier(t *testing.T, kind string) busyIterCarrier {
	t.Helper()
	db := busyOpenSQL(t)
	ctx := context.Background()
	switch kind {
	case "sqlConn":
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return busyIterCarrier{query: c.QueryContext, done: func() { _ = c.Close() }}
	default:
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		return busyIterCarrier{query: tx.QueryContext, done: func() { _ = tx.Rollback() }}
	}
}

// busyIterMarker runs the next statement of an iteration (alternately a literal
// and one with an argument, which is the extended protocol) and checks the row is
// its own: the marker of iteration i.
func busyIterMarker(c busyIterCarrier, i int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var rows *sql.Rows
	var err error
	if i%2 == 0 {
		rows, err = c.query(ctx, fmt.Sprintf("SELECT 'm-%d'::text, %d::int", i, i))
	} else {
		rows, err = c.query(ctx, "SELECT 'm-' || $1::int::text, $1::int", i)
	}
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return fmt.Errorf("no row, Err = %v", rows.Err())
	}
	var m string
	var n int
	if err := rows.Scan(&m, &n); err != nil {
		return err
	}
	if m != fmt.Sprintf("m-%d", i) || n != i {
		return fmt.Errorf("got (%q, %d), want its own row m-%d: a row of another statement", m, n, i)
	}
	if rows.Next() {
		return fmt.Errorf("more than one row")
	}
	return rows.Err()
}

// TestBusyConnCancelThenCloseThenNext: cancel the ctx of a streaming query, Close
// the Rows (synchronous under database/sql), and run the next statement on the same
// Conn or Tx, thousands of times: no busy error, no hang, and every statement
// gets its own row.
func TestBusyConnCancelThenCloseThenNext(t *testing.T) {
	for _, kind := range []string{"sqlConn", "sqlTx"} {
		t.Run(kind, func(t *testing.T) {
			c := busyOpenIterCarrier(t, kind)
			defer c.done()
			iters := busyIters(300)
			for i := 0; i < iters; i++ {
				qctx, cancel := context.WithCancel(context.Background())
				rows, err := c.query(qctx, busyIterQ)
				if err != nil {
					t.Fatalf("iteration %d: first statement: %v", i, err)
				}
				if !rows.Next() {
					t.Fatalf("iteration %d: no first row: %v", i, rows.Err())
				}
				cancel()
				_ = rows.Close()
				err, _ = busyWatch(t, fmt.Sprint(kind, " iteration ", i, ": next statement"), 30*time.Second, func() error { return busyIterMarker(c, i) })
				if err != nil {
					t.Fatalf("iteration %d: the statement after cancel + Close: %v (busy = %v), want nil", i, err, isRowsOpen(err))
				}
			}
		})
	}
}

// TestBusyConnCancelWithoutClose: the shape that is not helped: cancel, no Close,
// the next statement at once. Every outcome is success or ErrRowsOpen, never a
// hang and never a wrong row, and one retry after Close succeeds. On a Conn or a
// Tx database/sql closes the Rows on a goroutine of its own, and Rows.Next
// already returns false before it has: that is the ErrRowsOpen window.
func TestBusyConnCancelWithoutClose(t *testing.T) {
	for _, kind := range []string{"sqlConn", "sqlTx"} {
		t.Run(kind, func(t *testing.T) {
			c := busyOpenIterCarrier(t, kind)
			defer c.done()
			iters := busyIters(300)
			okN, busyN := 0, 0
			for i := 0; i < iters; i++ {
				qctx, cancel := context.WithCancel(context.Background())
				rows, err := c.query(qctx, busyIterQ)
				if err != nil {
					t.Fatalf("iteration %d: first statement: %v", i, err)
				}
				if !rows.Next() {
					t.Fatalf("iteration %d: no first row: %v", i, rows.Err())
				}
				cancel()
				err, _ = busyWatch(t, fmt.Sprint(kind, " iteration ", i, ": next statement"), 30*time.Second, func() error { return busyIterMarker(c, i) })
				switch {
				case err == nil:
					okN++
				case isRowsOpen(err):
					busyN++
					_ = rows.Close()
					if err, _ := busyWatch(t, fmt.Sprint(kind, " iteration ", i, ": retry"), 30*time.Second, func() error { return busyIterMarker(c, i) }); err != nil {
						t.Fatalf("iteration %d: the retry after Close: %v", i, err)
					}
				default:
					t.Fatalf("iteration %d: %v, want nil or ErrRowsOpen (a wrong row or another error)", i, err)
				}
				_ = rows.Close()
			}
			t.Logf("%s: cancel without Close, next statement at once: %d ok, %d ErrRowsOpen of %d", kind, okN, busyN, iters)
		})
	}
}

// TestBusyConnOrdinary: the sequences that always worked still work.
func TestBusyConnOrdinary(t *testing.T) {
	for _, car := range busyCarriers {
		t.Run(car.name+"/iterated-then-next", func(t *testing.T) {
			env := car.open(t)
			defer func() { busyClose(t, env, 15*time.Second) }()
			busyReadFirst(t, env.r1)
			busyDrain(t, env.r1, 1)
			// no Close: iterating to the end is enough for database/sql (it
			// closes the Rows when Next returns false); the native Rows is
			// closed by the caller as its docs say.
			if err := env.r1.Close(); err != nil {
				t.Fatal(err)
			}
			busyCheckMarkers(t, env, "after a fully iterated Rows")
		})
		if car.name == "nativeTx" {
			continue // the native Rows wants an explicit Close: nothing else to run
		}
		t.Run(car.name+"/iterated-no-close", func(t *testing.T) {
			env := car.open(t)
			defer func() { busyClose(t, env, 15*time.Second) }()
			busyReadFirst(t, env.r1)
			busyDrain(t, env.r1, 1)
			busyCheckMarkers(t, env, "after Next returned false")
		})
	}
}

// TestBusyConnSmallResultIsNotBusy: a result under the streaming threshold is
// already buffered when the call returns, so a second statement while that Rows
// is open has always worked, and still does.
func TestBusyConnSmallResultIsNotBusy(t *testing.T) {
	dsn := dsnFromEnv(t)
	db, err := openSQLEngine(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	for _, n := range []int{1, 10, 63} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			r1, err := tx.QueryContext(ctx, "SELECT g FROM generate_series(1, $1::int) g", n)
			if err != nil {
				t.Fatal(err)
			}
			if !r1.Next() {
				t.Fatal("no first row")
			}
			var got int
			err, _ = busyWatch(t, "second statement over a buffered Rows", 15*time.Second, func() error {
				return tx.QueryRowContext(ctx, "SELECT 'second'::text || 42").Scan(new(string))
			})
			if err != nil {
				t.Fatalf("second statement while a buffered (under the threshold) Rows is open: %v", err)
			}
			if err := r1.Scan(&got); err != nil || got != 1 {
				t.Fatalf("first Rows row 1 = %d, %v", got, err)
			}
			for want := 2; want <= n; want++ {
				if !r1.Next() {
					t.Fatalf("Next = false at row %d: %v", want, r1.Err())
				}
				if err := r1.Scan(&got); err != nil || got != want {
					t.Fatalf("row %d = %d, %v", want, got, err)
				}
			}
			if r1.Next() || r1.Err() != nil {
				t.Fatalf("extra row or error: %v", r1.Err())
			}
		})
	}
}
