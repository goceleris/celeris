package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// writeFakeError writes an ErrorResponse with severity, SQLSTATE and message.
func writeFakeError(c net.Conn, code, msg string) error {
	p := []byte("SERROR\x00C" + code + "\x00M" + msg + "\x00\x00")
	return writeMsg(c, protocol.BackendErrorResponse, p)
}

// resetFake is a fake server that tracks the transaction status of each
// connection it accepts, as a real server does: BEGIN opens a transaction
// (ReadyForQuery 'T'), FAIL is an error (ReadyForQuery 'E' inside a
// transaction, 'I' outside), COMMIT and ROLLBACK end it, and every other
// statement answers with the current status. With failDiscard set, a DISCARD
// ALL is an error as a server answers it inside a transaction block (25001).
type resetFake struct {
	mu          sync.Mutex
	conns       int
	ran         map[int][]string // connection number -> simple queries received
	closed      map[int]bool     // connection number -> client hung up
	failDiscard bool
	addr        string
}

func newResetFake(t *testing.T, failDiscard bool) *resetFake {
	f := &resetFake{ran: map[int][]string{}, closed: map[int]bool{}, failDiscard: failDiscard}
	f.addr = startFakePG(t, func(c net.Conn) {
		f.mu.Lock()
		f.conns++
		n := f.conns
		f.mu.Unlock()
		defer func() {
			f.mu.Lock()
			f.closed[n] = true
			f.mu.Unlock()
		}()
		fakePGTrustStartup(t, c, int32(n), 2, func(c net.Conn) {
			status := byte('I')
			for {
				typ, body, err := readMsg(c)
				if err != nil || typ == protocol.MsgTerminate {
					return
				}
				if typ != protocol.MsgQuery {
					continue
				}
				q := strings.TrimRight(string(body), "\x00")
				f.mu.Lock()
				f.ran[n] = append(f.ran[n], q)
				f.mu.Unlock()
				switch {
				case q == "DISCARD ALL" && f.failDiscard:
					_ = writeFakeError(c, "25001", "DISCARD ALL cannot run inside a transaction block")
					if status == 'T' {
						status = 'E'
					}
				case q == "DISCARD ALL":
					_ = writeCommandComplete(c, "DISCARD ALL")
					status = 'I'
				case q == "FAIL":
					_ = writeFakeError(c, "22012", "division by zero")
					if status == 'T' {
						status = 'E'
					}
				case q == "BEGIN;FAIL":
					_ = writeFakeError(c, "22012", "division by zero")
					status = 'E'
				case strings.HasPrefix(q, "BEGIN"):
					_ = writeCommandComplete(c, "BEGIN")
					status = 'T'
				case q == "COMMIT" || q == "ROLLBACK":
					_ = writeCommandComplete(c, q)
					status = 'I'
				default:
					_ = writeCommandComplete(c, "OK")
				}
				_ = writeReadyForQuery(c, status)
			}
		})
	})
	return f
}

// ranOn reports whether connection n received query q.
func (f *resetFake) ranOn(n int, q string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.ran[n] {
		if s == q {
			return true
		}
	}
	return false
}

func (f *resetFake) dump() map[int][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[int][]string{}
	for k, v := range f.ran {
		out[k] = append([]string(nil), v...)
	}
	return out
}

func (f *resetFake) clientClosed(n int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed[n]
}

func (f *resetFake) dial(t *testing.T) (*pgConn, context.Context) {
	t.Helper()
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eventloop.Release(prov) })
	host, port, _ := net.SplitHostPort(f.addr)
	dsn := DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable"}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	c, err := dialConn(ctx, prov, nil, dsn, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, ctx
}

func (f *resetFake) db(t *testing.T) *sql.DB {
	t.Helper()
	host, port, _ := net.SplitHostPort(f.addr)
	cn, err := NewConnector("postgres://u@" + host + ":" + port + "/d?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(cn)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// DISCARD ALL failing on a dirty session: ResetSession must return
// driver.ErrBadConn; database/sql discards a conn on that error only and hands
// the conn to the next caller on any other (#1382).
func TestResetSessionFailureIsDriverErrBadConn(t *testing.T) {
	f := newResetFake(t, true)
	c, ctx := f.dial(t)
	c.sessionDirty.Store(true)
	if err := c.ResetSession(ctx); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("failed DISCARD ALL: ResetSession = %v, want driver.ErrBadConn", err)
	}
	_ = c.Close()
	if err := c.ResetSession(ctx); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("closed conn: ResetSession = %v, want driver.ErrBadConn", err)
	}
}

// The same through database/sql: the conn whose DISCARD ALL fails is dropped
// and the next statement runs on a new conn, not on the unreset one. The
// session is made dirty without leaving a transaction open (BEGIN, then COMMIT
// as plain SQL), so only the failed reset can discard it.
func TestResetSessionFailureDropsConnUnderDatabaseSQL(t *testing.T) {
	f := newResetFake(t, true)
	db := f.db(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Raw(func(dc any) error {
		_, err := dc.(driver.ConnBeginTx).BeginTx(ctx, driver.TxOptions{})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ExecContext(ctx, "COMMIT"); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "SELECT 'user B'"); err != nil {
		t.Fatalf("user B: %v", err)
	}
	if f.ranOn(1, "SELECT 'user B'") || !f.ranOn(2, "SELECT 'user B'") {
		t.Fatalf("user B must run on a new conn: %v", f.dump())
	}
}

// A conn that is not idle in the protocol's terms (ReadyForQuery status T or E)
// is never handed to the next user: ResetSession and IsValid refuse it, with no
// round trip.
func TestConnNotIdleIsNotReusable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stmts []string
		want  bool // reusable
	}{
		{"idle", []string{"SELECT 1"}, true},
		{"in transaction", []string{"BEGIN"}, false},
		{"failed transaction", []string{"BEGIN", "FAIL"}, false},
		{"transaction ended", []string{"BEGIN", "COMMIT"}, true},
		{"failed transaction rolled back", []string{"BEGIN", "FAIL", "ROLLBACK"}, true},
		{"error outside a transaction", []string{"FAIL"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newResetFake(t, false)
			c, ctx := f.dial(t)
			for _, s := range tc.stmts {
				_, _ = c.ExecContext(ctx, s, nil)
			}
			before := len(f.dump()[1])
			err := c.ResetSession(ctx)
			if tc.want && (err != nil || !c.IsValid()) {
				t.Fatalf("ResetSession = %v, IsValid = %v, want a reusable conn", err, c.IsValid())
			}
			if !tc.want && (!errors.Is(err, driver.ErrBadConn) || c.IsValid()) {
				t.Fatalf("ResetSession = %v, IsValid = %v, want driver.ErrBadConn and invalid", err, c.IsValid())
			}
			if after := len(f.dump()[1]); after != before {
				t.Fatalf("ResetSession sent %d more statements, want none", after-before)
			}
		})
	}
}

// database/sql: a transaction left open on a conn (BEGIN as plain SQL) is
// dropped when the conn is returned, rolled back by the server, and the next
// user gets a new conn.
func TestOpenTransactionConnIsDroppedUnderDatabaseSQL(t *testing.T) {
	for _, stmts := range [][]string{{"BEGIN"}, {"BEGIN", "FAIL"}} {
		t.Run(strings.Join(stmts, "+"), func(t *testing.T) {
			f := newResetFake(t, false)
			db := f.db(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			a, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range stmts {
				_, _ = a.ExecContext(ctx, s)
			}
			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, "SELECT 'user B'"); err != nil {
				t.Fatalf("user B: %v", err)
			}
			if f.ranOn(1, "SELECT 'user B'") || !f.ranOn(2, "SELECT 'user B'") {
				t.Fatalf("user B must run on a new conn: %v", f.dump())
			}
			deadline := time.Now().Add(5 * time.Second)
			for !f.clientClosed(1) && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if !f.clientClosed(1) {
				t.Fatal("the conn left in a transaction was not closed")
			}
		})
	}
}

// database/sql: the ordinary Tx path (BeginTx, Commit or Rollback) keeps
// reusing its conn.
func TestTxPathReusesConnUnderDatabaseSQL(t *testing.T) {
	f := newResetFake(t, false)
	db := f.db(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := 0; i < 4; i++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 {
			err = tx.Commit()
		} else {
			err = tx.Rollback()
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, "SELECT 1"); err != nil {
			t.Fatal(err)
		}
	}
	f.mu.Lock()
	n := f.conns
	f.mu.Unlock()
	if n != 1 {
		t.Fatalf("the Tx path opened %d conns, want 1 (reused): %v", n, f.dump())
	}
}

// The native Pool: the call that leaves a transaction open on its connection
// (Exec "BEGIN") returns ErrTxLeftOpen itself, the connection is closed and not
// put back, and the next statement runs on a new one: nothing after the BEGIN
// runs inside a transaction that no longer exists.
func TestNativePoolExecLeftTransactionOpenIsAnError(t *testing.T) {
	for _, stmt := range []string{"BEGIN", "BEGIN;FAIL"} {
		t.Run(stmt, func(t *testing.T) {
			f := newResetFake(t, false)
			host, port, _ := net.SplitHostPort(f.addr)
			p, err := Open("postgres://u@"+host+":"+port+"/d?sslmode=disable", WithMaxOpen(1))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = p.Close() }()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			res, err := p.ExecContext(ctx, stmt)
			if !errors.Is(err, ErrTxLeftOpen) {
				t.Fatalf("Exec %q = %v, want ErrTxLeftOpen", stmt, err)
			}
			if res != (Result{}) {
				t.Fatalf("Exec %q returned a result with its error: %+v", stmt, res)
			}
			if stmt == "BEGIN;FAIL" {
				var pe *PGError
				if !errors.As(err, &pe) || pe.Code != "22012" {
					t.Fatalf("the statement's own error is lost: %v", err)
				}
			}
			if _, err := p.ExecContext(ctx, "SELECT 'user B'"); err != nil {
				t.Fatalf("user B: %v", err)
			}
			if f.ranOn(1, "SELECT 'user B'") || !f.ranOn(2, "SELECT 'user B'") {
				t.Fatalf("user B must run on a new conn: %v", f.dump())
			}
			if st := p.Stats(); st.Open != 1 {
				t.Fatalf("Stats.Open = %d, want 1 (the dropped conn is not counted)", st.Open)
			}
		})
	}
}

// The same error on the Query path: Rows.Close returns it, and so does
// Row.Scan.
func TestNativePoolQueryLeftTransactionOpenIsAnError(t *testing.T) {
	f := newResetFake(t, false)
	host, port, _ := net.SplitHostPort(f.addr)
	p, err := Open("postgres://u@"+host+":"+port+"/d?sslmode=disable", WithMaxOpen(1))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := p.QueryContext(ctx, "BEGIN")
	if err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); !errors.Is(err, ErrTxLeftOpen) {
		t.Fatalf("Rows.Close = %v, want ErrTxLeftOpen", err)
	}
	var x string
	if err := p.QueryRow(ctx, "BEGIN").Scan(&x); !errors.Is(err, ErrTxLeftOpen) {
		t.Fatalf("Row.Scan = %v, want ErrTxLeftOpen", err)
	}
	// An ordinary statement is unaffected.
	if _, err := p.ExecContext(ctx, "SELECT 1"); err != nil {
		t.Fatalf("Exec after: %v", err)
	}
}

// The native Pool's Tx path keeps reusing its conn.
func TestNativePoolTxPathReusesConn(t *testing.T) {
	f := newResetFake(t, false)
	host, port, _ := net.SplitHostPort(f.addr)
	p, err := Open("postgres://u@"+host+":"+port+"/d?sslmode=disable", WithMaxOpen(1))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := 0; i < 4; i++ {
		tx, err := p.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 {
			err = tx.Commit()
		} else {
			err = tx.Rollback()
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	f.mu.Lock()
	n := f.conns
	f.mu.Unlock()
	if n != 1 {
		t.Fatalf("the Tx path opened %d conns, want 1: %v", n, f.dump())
	}
}

// captureLog is a slog handler that records the messages it gets.
type captureLog struct {
	mu   sync.Mutex
	msgs []string
}

func (h *captureLog) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureLog) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.msgs = append(h.msgs, r.Level.String()+" "+r.Message)
	h.mu.Unlock()
	return nil
}
func (h *captureLog) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureLog) WithGroup(string) slog.Handler      { return h }
func (h *captureLog) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.msgs)
}

// database/sql gives the driver no error to return when a connection comes back
// inside a transaction, so the discard is logged at Warn on slog's default
// logger, with the words of ErrTxLeftOpen: the first time, then at most once a
// minute.
func TestTxDiscardIsLoggedOncePerMinute(t *testing.T) {
	h := &captureLog{}
	old := slog.Default()
	slog.SetDefault(slog.New(h))
	oldLast := lastTxDiscardLog.Swap(0)
	t.Cleanup(func() {
		slog.SetDefault(old)
		lastTxDiscardLog.Store(oldLast)
	})

	f := newResetFake(t, false)
	c, ctx := f.dial(t)
	if _, err := c.ExecContext(ctx, "BEGIN", nil); err != nil {
		t.Fatal(err)
	}
	if c.IsValid() {
		t.Fatal("a conn in a transaction is valid")
	}
	if n := h.count(); n != 1 {
		t.Fatalf("first discard logged %d records, want 1: %v", n, h.msgs)
	}
	if want := "WARN " + ErrTxLeftOpen.Error(); h.msgs[0] != want {
		t.Fatalf("logged %q, want %q", h.msgs[0], want)
	}
	// Again at once, through both entry points: rate limited.
	_ = c.IsValid()
	if err := c.ResetSession(ctx); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("ResetSession = %v", err)
	}
	if n := h.count(); n != 1 {
		t.Fatalf("a second discard within a minute logged: %d records", n)
	}
	// A minute later it logs again (through ResetSession this time).
	lastTxDiscardLog.Store(time.Now().Add(-61 * time.Second).UnixNano())
	if err := c.ResetSession(ctx); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("ResetSession = %v", err)
	}
	if n := h.count(); n != 2 {
		t.Fatalf("a discard a minute later logged %d records in all, want 2", n)
	}
	// A conn that is idle logs nothing.
	g := newResetFake(t, false)
	idle, ictx := g.dial(t)
	lastTxDiscardLog.Store(0)
	if !idle.IsValid() || idle.ResetSession(ictx) != nil {
		t.Fatal("an idle conn was refused")
	}
	if n := h.count(); n != 2 {
		t.Fatalf("an idle conn logged: %d records", n)
	}
}
