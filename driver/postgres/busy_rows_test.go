package postgres

// A new statement on a connection that has an open, still-streaming Rows.
//
// database/sql lets a caller do it on a Tx and on a sql.Conn. On the event loop
// the driver used to answer it with a deadlock: the worker sits in the row
// delivery of the first statement (its consumer is not reading), holding the
// conn's receive lock, and the caller of the second statement sits in a plain
// lock wait for that same lock (eventloop WriteAndPoll), deaf to its ctx, with
// writerMu held, so that Close is stuck behind it too.
//
// The fake server below streams a result far larger than the socket buffers, so
// the request is still being received while the test holds the first Rows open.
// Each entry point that can start a request is called by name, with a watchdog
// (a hang fails the test) and a latency bound on the answer.

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

const (
	busyPad = 200
	// busyFirstBurst rows are sent, then busyPause passes, before the rest of a
	// longer result (see busyFakeServer).
	busyFirstBurst = 100
	busyPause      = 100 * time.Millisecond
)

// busyFakeServer answers "stream:N" with N rows (id, "r<id>-" + padding), "marks"
// with three rows ("m-1".."m-3") and anything else with an empty command
// complete. The rows are written as fast as the client takes them.
func busyFakeServer(t *testing.T) string {
	t.Helper()
	pad := strings.Repeat("x", busyPad)
	return startFakePG(t, func(c net.Conn) {
		fakePGTrustStartup(t, c, 1, 2, func(c net.Conn) {
			cols := buildRowDescription([]colSpec{
				{name: "id", typeOID: protocol.OIDInt4, typeSize: 4, format: protocol.FormatText},
				{name: "v", typeOID: protocol.OIDText, typeSize: -1, format: protocol.FormatText},
			})
			for {
				typ, payload, err := readMsg(c)
				if err != nil || typ == protocol.MsgTerminate {
					return
				}
				if typ != protocol.MsgQuery {
					continue
				}
				q := strings.TrimRight(string(payload), "\x00")
				switch {
				case strings.HasPrefix(q, "stream:"), strings.HasPrefix(q, "fast:"):
					pause := busyPause
					if strings.HasPrefix(q, "fast:") {
						pause = 300 * time.Microsecond // for loops of thousands of iterations
					}
					n, _ := strconv.Atoi(q[strings.Index(q, ":")+1:])
					_ = writeMsg(c, protocol.BackendRowDescription, cols)
					for i := 1; i <= n; i++ {
						if writeMsg(c, protocol.BackendDataRow, buildDataRow([][]byte{
							[]byte(strconv.Itoa(i)), []byte("r" + strconv.Itoa(i) + "-" + pad),
						})) != nil {
							return
						}
						if i == busyFirstBurst && n > i {
							// A pause after the first burst: the caller's sync poll reads
							// what has arrived, finds the reply unfinished and hands the
							// rest to the loop, which streams it. Without it a fast server
							// can have the whole reply buffered by that poll (a result up
							// to 64 MiB is), and then the first statement never streams.
							time.Sleep(pause)
						}
					}
					_ = writeCommandComplete(c, "SELECT "+strconv.Itoa(n))
				case q == "marks":
					_ = writeMsg(c, protocol.BackendRowDescription, cols)
					for i := 1; i <= 3; i++ {
						_ = writeMsg(c, protocol.BackendDataRow, buildDataRow([][]byte{
							[]byte(strconv.Itoa(i)), []byte("m-" + strconv.Itoa(i)),
						}))
					}
					_ = writeCommandComplete(c, "SELECT 3")
				default:
					_ = writeCommandComplete(c, "SELECT 1")
				}
				_ = writeReadyForQuery(c, 'I')
			}
		})
	})
}

// busyDial opens a conn of the loop path (the standalone event loop).
func busyDial(t *testing.T, addr string, autoCache bool) *pgConn {
	t.Helper()
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Bounded: the loop of a wedged conn cannot be shut down.
		done := make(chan struct{})
		go func() { eventloop.Release(prov); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("eventloop.Release still blocked after 10 s")
		}
	})
	host, port, _ := net.SplitHostPort(addr)
	dsn := DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable", StatementCacheSize: 16}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := dialConn(ctx, prov, nil, dsn, 0)
	if err != nil {
		t.Fatal(err)
	}
	c.autoCache = autoCache
	t.Cleanup(func() {
		// Bounded: a conn that is wedged must not hang the package.
		done := make(chan struct{})
		go func() { _ = c.Close(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("conn.Close still blocked after 10 s")
		}
	})
	return c
}

// busyWatch runs fn; a call still blocked after d fails the test (a hang), with
// the stacks of the goroutines that sit in the driver.
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
		var keep []string
		for _, g := range strings.Split(string(buf), "\n\n") {
			if strings.Contains(g, "driver/postgres") || strings.Contains(g, "driver/internal/eventloop") {
				keep = append(keep, g)
			}
		}
		t.Logf("WATCHDOG: %s: goroutines in the driver:\n%s", what, strings.Join(keep, "\n\n"))
		t.Fatalf("WATCHDOG: %s still blocked after %v (a hang)", what, d)
		var zero T
		return zero, 0
	}
}

// busyOpen starts the streaming first statement and reads its first row.
func busyOpen(t *testing.T, c *pgConn, rows int) driver.Rows {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	r1, err := c.QueryContext(ctx, "stream:"+strconv.Itoa(rows), nil)
	if err != nil {
		t.Fatal(err)
	}
	busyNext(t, r1, 1)
	return r1
}

// busyNext reads one row of the first statement and checks its marker.
func busyNext(t *testing.T, r driver.Rows, want int) {
	t.Helper()
	dest := make([]driver.Value, 2)
	if err := r.Next(dest); err != nil {
		t.Fatalf("first statement: row %d: %v", want, err)
	}
	if got, ok := dest[1].(string); !ok || got != "r"+strconv.Itoa(want)+"-"+strings.Repeat("x", busyPad) {
		t.Fatalf("first statement: row %d marker = %.20v, want r%d-...", want, dest[1], want)
	}
}

// busyDrain reads the first statement to its end and checks every marker.
func busyDrain(t *testing.T, r driver.Rows, from, total int) {
	t.Helper()
	dest := make([]driver.Value, 2)
	n := from
	for {
		err := r.Next(dest)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("first statement: after row %d: %v", n, err)
		}
		n++
		if got, _ := dest[1].(string); got != "r"+strconv.Itoa(n)+"-"+strings.Repeat("x", busyPad) {
			t.Fatalf("first statement: row %d marker = %.20q", n, got)
		}
	}
	if n != total {
		t.Fatalf("first statement: %d rows, want %d", n, total)
	}
}

// busyMarks runs the next query on the conn and checks it returns its own rows.
func busyMarks(t *testing.T, c *pgConn, when string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err, _ := busyWatch(t, when+": next query", 15*time.Second, func() error {
		r, err := c.QueryContext(ctx, "marks", nil)
		if err != nil {
			return err
		}
		defer func() { _ = r.Close() }()
		dest := make([]driver.Value, 2)
		for i := 1; i <= 3; i++ {
			if err := r.Next(dest); err != nil {
				return fmt.Errorf("row %d: %w", i, err)
			}
			if got, _ := dest[1].(string); got != "m-"+strconv.Itoa(i) {
				return fmt.Errorf("row %d = %v: not its own rows", i, dest[1])
			}
		}
		if err := r.Next(dest); err != io.EOF {
			return fmt.Errorf("after 3 rows: %v, want EOF", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("%s: %v", when, err)
	}
}

// busyEntry is one way a new request can start on a conn.
type busyEntry struct {
	name string
	// autoCache: run on a conn with the statement cache on (the database/sql default).
	autoCache bool
	run       func(ctx context.Context, c *pgConn) error
}

type busyNoSrc struct{}

func (busyNoSrc) Next() bool             { return false }
func (busyNoSrc) Values() ([]any, error) { return nil, nil }
func (busyNoSrc) Err() error             { return nil }

var busyEntries = []busyEntry{
	{name: "ExecContext_simpleExec", run: func(ctx context.Context, c *pgConn) error {
		_, err := c.ExecContext(ctx, "SELECT 1", nil)
		return err
	}},
	{name: "QueryContext_simpleQuery", run: func(ctx context.Context, c *pgConn) error {
		r, err := c.QueryContext(ctx, "SELECT 2", nil)
		if r != nil {
			_ = r.Close()
		}
		return err
	}},
	{name: "QueryContext-args_extendedQuery", run: func(ctx context.Context, c *pgConn) error {
		r, err := c.QueryContext(ctx, "SELECT $1::int", []driver.NamedValue{{Ordinal: 1, Value: int64(1)}})
		if r != nil {
			_ = r.Close()
		}
		return err
	}},
	{name: "ExecContext-args_extendedExec", run: func(ctx context.Context, c *pgConn) error {
		_, err := c.ExecContext(ctx, "SELECT $1::int", []driver.NamedValue{{Ordinal: 1, Value: int64(1)}})
		return err
	}},
	{name: "QueryContext-args-autoCache_prepareStatement", autoCache: true, run: func(ctx context.Context, c *pgConn) error {
		r, err := c.QueryContext(ctx, "SELECT $1::int", []driver.NamedValue{{Ordinal: 1, Value: int64(1)}})
		if r != nil {
			_ = r.Close()
		}
		return err
	}},
	{name: "ExecContext-args-autoCache_prepareStatement", autoCache: true, run: func(ctx context.Context, c *pgConn) error {
		_, err := c.ExecContext(ctx, "INSERT INTO t VALUES ($1)", []driver.NamedValue{{Ordinal: 1, Value: int64(1)}})
		return err
	}},
	{name: "PrepareContext_prepareStatement", run: func(ctx context.Context, c *pgConn) error {
		st, err := c.PrepareContext(ctx, "SELECT $1::int")
		if st != nil {
			_ = st.Close()
		}
		return err
	}},
	{name: "Ping_simpleExecCmd", run: func(ctx context.Context, c *pgConn) error { return c.Ping(ctx) }},
	{name: "BeginTx_simpleExecCmd", run: func(ctx context.Context, c *pgConn) error {
		_, err := c.BeginTx(ctx, driver.TxOptions{})
		return err
	}},
	{name: "Savepoint_simpleExecCmd", run: func(ctx context.Context, c *pgConn) error { return c.Savepoint(ctx, "sp1") }},
	{name: "ResetSession_simpleExecCmd", run: func(ctx context.Context, c *pgConn) error {
		c.sessionDirty.Store(true)
		return c.ResetSession(ctx)
	}},
	{name: "Tx.Commit_simpleExecCmd", run: func(ctx context.Context, c *pgConn) error { return (&pgTx{conn: c}).Commit() }},
	{name: "Tx.Rollback_simpleExecCmd", run: func(ctx context.Context, c *pgConn) error { return (&pgTx{conn: c}).Rollback() }},
	{name: "copyFrom", run: func(ctx context.Context, c *pgConn) error {
		_, err := c.copyFrom(ctx, "t", nil, busyNoSrc{})
		return err
	}},
	{name: "copyTo", run: func(ctx context.Context, c *pgConn) error {
		return c.copyTo(ctx, "COPY t TO STDOUT", func([]byte) error { return nil })
	}},
}

// TestBusyEntryPoints: every way to start a request on a conn whose first
// statement is still streaming returns the busy error at once. The first
// statement is then intact (every row, with its marker), and the conn is
// reusable: the next query returns its own rows.
func TestBusyEntryPoints(t *testing.T) {
	const total = 20000
	for _, e := range busyEntries {
		t.Run(e.name, func(t *testing.T) {
			addr := busyFakeServer(t)
			c := busyDial(t, addr, e.autoCache)
			r1 := busyOpen(t, c, total)

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			err, el := busyWatch(t, e.name, 10*time.Second, func() error { return e.run(ctx, c) })
			if !isRowsOpenErr(err) {
				t.Fatalf("%s with an open streaming Rows: err = %v, want the busy error (ErrRowsOpen)", e.name, err)
			}
			if el > 100*time.Millisecond {
				t.Fatalf("%s took %v to answer, want < 100ms (no lock wait, no round trip)", e.name, el)
			}
			busyDrain(t, r1, 1, total)
			if err := r1.Close(); err != nil {
				t.Fatalf("first statement Close: %v", err)
			}
			busyMarks(t, c, "after the first statement")
		})
	}
}

// TestBusyCloseThenNext: Close of the unread Rows, then the next statement on
// the same conn works, whatever it is, and returns its own rows.
func TestBusyCloseThenNext(t *testing.T) {
	addr := busyFakeServer(t)
	c := busyDial(t, addr, false)
	r1 := busyOpen(t, c, 20000)
	_, _ = busyWatch(t, "Close of the unread Rows", 30*time.Second, func() error { return r1.Close() })
	busyMarks(t, c, "after Close")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err, _ := busyWatch(t, "Exec after Close", 10*time.Second, func() error {
		_, err := c.ExecContext(ctx, "SELECT 1", nil)
		return err
	}); err != nil {
		t.Fatalf("Exec after Close: %v", err)
	}
}

// TestBusyNotBusyBelowThreshold: a result that never streamed (under 64 rows)
// is buffered whole when the call returns; a second statement while that Rows
// is open has always worked and must go on working. 64 rows is the first
// streamed size, and a stream whose request is complete (every row is in the
// channel already) is not busy either.
func TestBusyNotBusyBelowThreshold(t *testing.T) {
	for _, n := range []int{1, 10, 63, 64} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			addr := busyFakeServer(t)
			c := busyDial(t, addr, false)
			r1 := busyOpen(t, c, n)
			// Wait until the loop has taken the whole (small) reply: the request is off the queue.
			for i := 0; i < 500; i++ {
				c.pendingMu.Lock()
				n := len(c.pending)
				c.pendingMu.Unlock()
				if n == 0 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err, _ := busyWatch(t, "second statement over a small Rows", 10*time.Second, func() error {
				_, err := c.ExecContext(ctx, "SELECT 1", nil)
				return err
			})
			if err != nil {
				t.Fatalf("second statement while a %d-row Rows is open: %v (it worked before the guard)", n, err)
			}
			busyDrain(t, r1, 1, n)
			_ = r1.Close()
			busyMarks(t, c, "after")
		})
	}
}

// TestBusyMidSizeNeverHangs: results just past the streaming threshold. The
// first statement may or may not be complete (rows fit in the channel) when the
// second one starts: it either gets the busy error or succeeds, and never
// hangs; the first statement is intact either way.
func TestBusyMidSizeNeverHangs(t *testing.T) {
	for _, n := range []int{65, 70, 128, 200, 1000} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			addr := busyFakeServer(t)
			c := busyDial(t, addr, false)
			r1 := busyOpen(t, c, n)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err, el := busyWatch(t, "second statement over a mid-size Rows", 10*time.Second, func() error {
				_, err := c.ExecContext(ctx, "SELECT 1", nil)
				return err
			})
			if err != nil && !isRowsOpenErr(err) {
				t.Fatalf("second statement: %v, want nil or the busy error", err)
			}
			if el > 100*time.Millisecond {
				t.Fatalf("second statement took %v", el)
			}
			busyDrain(t, r1, 1, n)
			_ = r1.Close()
			busyMarks(t, c, "after")
		})
	}
}

// TestBusyCtxOfSecondDoesNotTouchFirst: a second statement that carries a
// cancelled ctx gets an error and does not disturb the first statement.
func TestBusyCtxOfSecondDoesNotTouchFirst(t *testing.T) {
	addr := busyFakeServer(t)
	c := busyDial(t, addr, false)
	r1 := busyOpen(t, c, 20000)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err, _ := busyWatch(t, "second statement with a cancelled ctx", 10*time.Second, func() error {
		_, err := c.ExecContext(ctx, "SELECT 1", nil)
		return err
	})
	if err == nil {
		t.Fatal("second statement returned nil with a streaming Rows open")
	}
	busyDrain(t, r1, 1, 20000)
	_ = r1.Close()
	busyMarks(t, c, "after")
}

// busyIters is the number of iterations of the cancel loops: 300 in the always-on
// path, CELERIS_BUSY_ITERS (3000 for the acceptance runs, with and without -race)
// when set.
func busyIters() int {
	if v, err := strconv.Atoi(os.Getenv("CELERIS_BUSY_ITERS")); err == nil && v > 0 {
		return v
	}
	return 300
}

// busyOpenQ starts a first statement and reads its first row.
func busyOpenQ(t *testing.T, c *pgConn, q string) (driver.Rows, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r1, err := c.QueryContext(ctx, q, nil)
	if err != nil {
		t.Fatal(err)
	}
	busyNext(t, r1, 1)
	return r1, cancel
}

// TestBusyCancelThenCloseThenNext: cancel the ctx of a streaming query, Close the
// Rows (under database/sql this is synchronous: Rows.Close, or its awaitDone
// goroutine, which Rows.Next does not wait for), and run the next statement on the
// same conn: no busy error, no hang, and the statement's rows are its own, in
// many iterations (300, CELERIS_BUSY_ITERS=3000 for the acceptance run). dc models database/sql's driverConn lock, held across
// every driver call.
func TestBusyCancelThenCloseThenNext(t *testing.T) {
	iters := busyIters()
	c := busyDial(t, busyFakeServer(t), false)
	var dc sync.Mutex
	streamed := 0
	for i := 0; i < iters; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		r1, err := c.QueryContext(ctx, "fast:800", nil)
		if err != nil {
			t.Fatalf("iteration %d: first statement: %v", i, err)
		}
		busyNext(t, r1, 1)
		if _, ok := r1.(*streamRows); ok {
			streamed++
		}
		cancel()
		dc.Lock()
		_ = r1.Close()
		dc.Unlock()
		ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		err, _ = busyWatch(t, fmt.Sprint("iteration ", i, ": next statement"), 15*time.Second, func() error {
			dc.Lock()
			defer dc.Unlock()
			r, err := c.QueryContext(ctx2, "marks", nil)
			if err != nil {
				return err
			}
			defer func() { _ = r.Close() }()
			dest := make([]driver.Value, 2)
			for k := 1; k <= 3; k++ {
				if err := r.Next(dest); err != nil {
					return fmt.Errorf("row %d: %w", k, err)
				}
				if got, _ := dest[1].(string); got != "m-"+strconv.Itoa(k) {
					return fmt.Errorf("row %d = %v: not its own rows", k, dest[1])
				}
			}
			return nil
		})
		cancel2()
		if err != nil {
			t.Fatalf("iteration %d: the statement after cancel + Close got %v (busy = %v), want nil", i, err, isRowsOpenErr(err))
		}
	}
	// The loop is only a test of the window when the first statement streams.
	if streamed < iters/4 {
		t.Fatalf("only %d of %d first statements streamed: the loop does not exercise the window", streamed, iters)
	}
}

// TestBusyCancelWithoutCloseThenNext: the shape that is not helped: cancel, no
// Close, the next statement at once. The driver does not look at the ctx of a Rows
// once the call has returned, so the outcome is success (the stream had ended) or
// ErrRowsOpen, never a hang and never a wrong row, and after Close one retry
// succeeds.
func TestBusyCancelWithoutCloseThenNext(t *testing.T) {
	iters := busyIters()
	c := busyDial(t, busyFakeServer(t), false)
	busyN, okN := 0, 0
	streamed := 0
	for i := 0; i < iters; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		r1, err := c.QueryContext(ctx, "fast:800", nil)
		if err != nil {
			t.Fatalf("iteration %d: first statement: %v", i, err)
		}
		busyNext(t, r1, 1)
		if _, ok := r1.(*streamRows); ok {
			streamed++
		}
		cancel()
		ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		err, _ = busyWatch(t, fmt.Sprint("iteration ", i, ": next statement"), 15*time.Second, func() error {
			_, err := c.ExecContext(ctx2, "SELECT 1", nil)
			return err
		})
		switch {
		case err == nil:
			okN++
		case isRowsOpenErr(err):
			busyN++
		default:
			t.Fatalf("iteration %d: %v, want nil or ErrRowsOpen", i, err)
		}
		_ = r1.Close()
		if err, _ := busyWatch(t, fmt.Sprint("iteration ", i, ": retry"), 15*time.Second, func() error {
			_, err := c.ExecContext(ctx2, "SELECT 1", nil)
			return err
		}); err != nil {
			t.Fatalf("iteration %d: the retry after Close: %v", i, err)
		}
		cancel2()
	}
	t.Logf("cancel without Close, then the next statement at once: %d ok, %d ErrRowsOpen of %d (%d streamed)", okN, busyN, iters, streamed)
	if streamed < iters/4 {
		t.Fatalf("only %d of %d first statements streamed: the loop does not exercise the window", streamed, iters)
	}
	busyMarks(t, c, "after the iterations")
}

// TestBusyDiscardEndsOpenRowsWithAnError is the mechanism behind native
// Tx.Rollback with a streaming Rows open: failAll with an explicit error, then the
// Close of the discard. The Rows must end with that error. Without the failAll the
// Close reports the teardown to the loop as io.EOF, which a consumer reads as the
// clean end of its rows (a truncated result that looks complete).
func TestBusyDiscardEndsOpenRowsWithAnError(t *testing.T) {
	streamed := 0
	for i := 0; i < 50; i++ {
		c := busyDial(t, busyFakeServer(t), false)
		r1, _ := busyOpenQ(t, c, "fast:2000")
		if _, ok := r1.(*streamRows); !ok {
			continue // the sync poll took the whole reply: nothing is truncated
		}
		streamed++
		c.failAll(errRollbackDiscard)
		_ = c.Close()
		dest := make([]driver.Value, 2)
		var err error
		err, _ = busyWatch(t, "drain after discard", 15*time.Second, func() error {
			for {
				if err := r1.Next(dest); err != nil {
					return err
				}
			}
		})
		if err == io.EOF || err == nil {
			t.Fatalf("iteration %d: the Rows of a discarded conn ended with %v, want an error", i, err)
		}
		if !errors.Is(err, ErrClosed) || !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("iteration %d: the Rows ended with %v, want one matching ErrClosed and io.ErrUnexpectedEOF", i, err)
		}
	}
	if streamed < 10 {
		t.Fatalf("only %d of 50 first statements streamed: the test does not exercise the discard", streamed)
	}
}
