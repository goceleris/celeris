package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/looptest"
	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// A context that ends while the auto-prepare step (Parse + Describe) of a
// cacheable statement waits for the server must end the call with the context's
// error, and the user's statement must never be sent (no Bind, no Execute). Two
// ways the cancel of the ended context can end: the cancel exchange is
// confirmed (the backend closes the cancel socket and answers the cancelled
// prepare with 57014: the conn stays usable) or it is not (the socket stays
// open: the conn is closed and discarded). The call used to swallow the prepare
// error and run its fallback path: on a conn whose cancel was confirmed that
// sent the statement (Parse + Bind + Execute) under the ended context, and a
// write could commit although the caller got the context's error; on a closed
// conn it returned ErrClosed in place of the context's error.

// parseStallPG is a backend that accepts a session, answers simple queries and
// never answers a Parse by itself. With confirm set a cancel request is
// answered by closing its socket and the statement that waits for the Sync of a
// stalled Parse then gets 57014 + ReadyForQuery, as a real backend answers a
// cancel; without it every cancel socket stays open, so the cancel exchange of
// the ended context cannot be confirmed. With notices set it sends a
// NoticeResponse every 20 ms after a Parse, as a backend does while it works: a
// Pool conn in direct mode looks at a cancel-only context only when bytes
// arrive, so these are what let it see the cancel. It counts the messages the
// client sent: parses, binds and executes (the user's statement).
type parseStallPG struct {
	addr    string
	notices bool
	confirm bool
	// answerPrepare, when set, answers the Sync of a Parse after this long with
	// ParseComplete, ParameterDescription (one int4) and NoData: the prepare
	// finishes.
	answerPrepare time.Duration
	// cancelCh gets a token for each cancel request (confirm only).
	cancelCh                chan struct{}
	parses, binds, executes atomic.Int64
	// parseCh gets a token each time a Parse reaches the backend.
	parseCh chan struct{}
	done    chan struct{}

	mu       sync.Mutex
	startups int
}

func startParseStallPG(t *testing.T, notices, confirm bool) *parseStallPG {
	t.Helper()
	f := &parseStallPG{notices: notices, confirm: confirm, cancelCh: make(chan struct{}, 16), parseCh: make(chan struct{}, 16), done: make(chan struct{})}
	f.addr = startFakePG(t, f.serve)
	t.Cleanup(func() { close(f.done) })
	return f
}

func (f *parseStallPG) sessions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.startups
}

func (f *parseStallPG) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	var lenBuf [4]byte
	if _, err := readFull(c, lenBuf[:]); err != nil {
		return
	}
	n := int(uint32(lenBuf[0])<<24 | uint32(lenBuf[1])<<16 | uint32(lenBuf[2])<<8 | uint32(lenBuf[3]))
	if n < 8 {
		return
	}
	body := make([]byte, n-4)
	if _, err := readFull(c, body); err != nil {
		return
	}
	if n == 16 { // a cancel request
		if f.confirm {
			f.cancelCh <- struct{}{}
			return // the socket closes
		}
		<-f.done // never answered, the socket stays open
		return
	}
	f.mu.Lock()
	f.startups++
	f.mu.Unlock()
	if writeAuthOK(c) != nil || writeBackendKeyData(c, 7, 9) != nil || writeReadyForQuery(c, 'I') != nil {
		return
	}
	for {
		typ, _, err := readMsg(c)
		if err != nil || typ == protocol.MsgTerminate {
			return
		}
		switch typ {
		case protocol.MsgBind:
			f.binds.Add(1)
		case protocol.MsgExecute:
			f.executes.Add(1)
		case protocol.MsgSync:
			if f.answerPrepare > 0 {
				go func() {
					time.Sleep(f.answerPrepare)
					// one write: the reply arrives whole, in one read
					var buf []byte
					for _, m := range []struct {
						typ     byte
						payload []byte
					}{{protocol.BackendParseComplete, nil}, {protocol.BackendParameterDesc, []byte{0, 1, 0, 0, 0, 23}}, {protocol.BackendNoData, nil}, {protocol.BackendReadyForQuery, []byte{'I'}}} {
						buf = append(buf, m.typ, 0, 0, 0, byte(4+len(m.payload)))
						buf = append(buf, m.payload...)
					}
					_, _ = c.Write(buf)
				}()
			} else if f.confirm {
				// the statement that waits is cancelled by the next cancel request
				go func() {
					select {
					case <-f.cancelCh:
						_ = writeMsg(c, protocol.BackendErrorResponse, []byte("SERROR\x00VERROR\x00C57014\x00Mcanceling statement due to user request\x00\x00"))
						_ = writeReadyForQuery(c, 'I')
					case <-f.done:
					}
				}()
			}
		case protocol.MsgParse:
			f.parses.Add(1)
			select {
			case f.parseCh <- struct{}{}:
			default:
			}
			if f.notices {
				go func() {
					tick := time.NewTicker(20 * time.Millisecond)
					defer tick.Stop()
					for {
						select {
						case <-tick.C:
							if writeMsg(c, protocol.BackendNoticeResponse, []byte("SNOTICE\x00VNOTICE\x00C00000\x00Mstill running\x00\x00")) != nil {
								return
							}
						case <-f.done:
							return
						}
					}
				}()
			}
		case protocol.MsgQuery:
			_ = writeCommandComplete(c, "SELECT 1")
			_ = writeReadyForQuery(c, 'I')
		}
	}
}

type prepareArm struct {
	name string
	// open returns the call to make: kind is "query" or "exec" (a statement
	// with an argument, auto-prepared) or "simple" (no argument).
	open func(t *testing.T, addr string) func(ctx context.Context, kind string) error
}

func prepareArms() []prepareArm {
	dsnFor := func(addr string) string {
		host, port, _ := net.SplitHostPort(addr)
		return "postgres://u@" + host + ":" + port + "/d?sslmode=disable"
	}
	const (
		sel = "SELECT $1::int"
		ins = "INSERT INTO t VALUES ($1)"
	)
	poolRun := func(p *Pool) func(context.Context, string) error {
		return func(ctx context.Context, kind string) error {
			switch kind {
			case "simple":
				_, err := p.ExecContext(ctx, "SELECT next")
				return err
			case "exec":
				_, err := p.ExecContext(ctx, ins, 1)
				return err
			}
			rows, err := p.QueryContext(ctx, sel, 1)
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
	return []prepareArm{
		{"pool-direct", func(t *testing.T, addr string) func(context.Context, string) error {
			p, err := Open(dsnFor(addr), WithMaxOpen(1))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Close() })
			return poolRun(p)
		}},
		{"pool-loop", func(t *testing.T, addr string) func(context.Context, string) error {
			p, err := Open(dsnFor(addr), WithMaxOpen(1), WithEngine(looptest.Unstarted()))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Close() })
			return poolRun(p)
		}},
		{"database-sql", func(t *testing.T, addr string) func(context.Context, string) error {
			db, err := sql.Open(DriverName, dsnFor(addr))
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = db.Close() })
			return func(ctx context.Context, kind string) error {
				switch kind {
				case "simple":
					_, err := db.ExecContext(ctx, "SELECT next")
					return err
				case "exec":
					_, err := db.ExecContext(ctx, ins, 1)
					return err
				}
				rows, err := db.QueryContext(ctx, sel, 1)
				if err != nil {
					return err
				}
				for rows.Next() {
				}
				err = rows.Err()
				_ = rows.Close()
				return err
			}
		}},
	}
}

func TestPrepareEndedByTheContextReportsTheContextError(t *testing.T) {
	old := cancelExchangeTimeout
	cancelExchangeTimeout = 150 * time.Millisecond
	t.Cleanup(func() { cancelExchangeTimeout = old })

	for _, arm := range prepareArms() {
		for _, kind := range []string{"exec", "query"} {
			for _, end := range []string{"deadline", "cancel"} {
				for _, outcome := range []string{"confirmed", "unconfirmed"} {
					t.Run(arm.name+"/"+kind+"/"+end+"/"+outcome, func(t *testing.T) {
						confirmed := outcome == "confirmed"
						f := startParseStallPG(t, end == "cancel", confirmed)
						run := arm.open(t, f.addr)

						ctx, stop := context.WithCancel(context.Background())
						defer stop()
						want := context.Canceled
						if end == "deadline" {
							var cancel context.CancelFunc
							ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
							defer cancel()
							want = context.DeadlineExceeded
						} else {
							go func() {
								select {
								case <-f.parseCh:
									stop()
								case <-f.done:
								}
							}()
						}
						errc := make(chan error, 1)
						go func() { errc <- run(ctx, kind) }()
						var err error
						select {
						case err = <-errc:
						case <-time.After(10 * time.Second):
							t.Fatal("the call did not return")
						}
						// The context's error; on the event loop a confirmed cancel may also
						// surface as the server's own answer to it, 57014.
						var pe *PGError
						if ok := errors.Is(err, want) || (confirmed && errors.As(err, &pe) && pe.Code == "57014"); !ok {
							t.Fatalf("err = %v, want it to satisfy errors.Is(err, %v)", err, want)
						}
						if errors.Is(err, ErrClosed) {
							t.Fatalf("err = %v: ErrClosed in place of the context's error", err)
						}
						// The next statement (no argument): on the same session when the
						// cancel was confirmed, on a new one when the conn was discarded.
						// Messages are handled in order per session, so after it the
						// counts include everything the ended call sent.
						next := make(chan error, 1)
						go func() {
							nctx, ncancel := context.WithTimeout(context.Background(), 5*time.Second)
							defer ncancel()
							next <- run(nctx, "simple")
						}()
						select {
						case err := <-next:
							if err != nil {
								t.Fatalf("the next statement failed: %v", err)
							}
						case <-time.After(10 * time.Second):
							t.Fatal("the next statement did not return")
						}
						wantSessions := 2
						if confirmed {
							wantSessions = 1
						}
						if got := f.sessions(); got != wantSessions {
							t.Fatalf("sessions = %d, want %d (cancel %s)", got, wantSessions, outcome)
						}
						// The user's statement is never sent: one Parse (the prepare), no
						// Bind, no Execute.
						if p, b, e := f.parses.Load(), f.binds.Load(), f.executes.Load(); p != 1 || b != 0 || e != 0 {
							t.Fatalf("the backend got %d Parse, %d Bind, %d Execute; want 1, 0, 0: the statement was sent after its context ended", p, b, e)
						}
					})
				}
			}
		}
	}
}

// A prepare that FINISHES under a context that ended while it ran (a Pool conn
// in direct mode looks at a cancel-only context only when bytes arrive, so the
// prepare's reply is what ends its wait) must not go on to send the statement.
func TestPrepareFinishedUnderAnEndedContextDoesNotSendTheStatement(t *testing.T) {
	for _, kind := range []string{"exec", "query"} {
		t.Run(kind, func(t *testing.T) {
			f := startParseStallPG(t, false, false)
			f.answerPrepare = 100 * time.Millisecond
			run := prepareArms()[0].open(t, f.addr) // pool-direct
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			go func() {
				select {
				case <-f.parseCh:
					// The conn is in its blocking read by now (the reply comes after
					// 100 ms): the cancel is not seen until the reply arrives.
					time.Sleep(30 * time.Millisecond)
					stop()
				case <-f.done:
				}
			}()
			errc := make(chan error, 1)
			go func() { errc <- run(ctx, kind) }()
			select {
			case err := <-errc:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("err = %v, want context.Canceled", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the call did not return")
			}
			if err := run(context.Background(), "simple"); err != nil {
				t.Fatalf("the next statement failed: %v", err)
			}
			if p, b, e := f.parses.Load(), f.binds.Load(), f.executes.Load(); p != 1 || b != 0 || e != 0 {
				t.Fatalf("the backend got %d Parse, %d Bind, %d Execute; want 1, 0, 0: the statement was sent after its context ended", p, b, e)
			}
			if got := f.sessions(); got != 1 {
				t.Fatalf("sessions = %d, want 1: the conn finished the prepare and is reusable", got)
			}
		})
	}
}

// lostStmtPG is a backend that prepares every statement and runs every Bind
// with success, except the first Bind after the first run, which it answers with
// 26000 (the server lost the cached statement: a DEALLOCATE ALL, a pooler in
// transaction mode). The Parse that follows (the driver's re-prepare) is
// answered after reprepareDelay. It counts the messages it got.
type lostStmtPG struct {
	addr           string
	reprepareDelay time.Duration
	parseCh        chan int64 // the number of Parses seen, one send per Parse
	done           chan struct{}

	parses, binds, executes atomic.Int64
	mu                      sync.Mutex
	startups                int
}

func startLostStmtPG(t *testing.T, reprepareDelay time.Duration) *lostStmtPG {
	t.Helper()
	f := &lostStmtPG{reprepareDelay: reprepareDelay, parseCh: make(chan int64, 16), done: make(chan struct{})}
	f.addr = startFakePG(t, f.serve)
	t.Cleanup(func() { close(f.done) })
	return f
}

func fakeReply(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func fakeMsg(typ byte, payload ...byte) []byte {
	n := 4 + len(payload)
	return append([]byte{typ, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}, payload...)
}

func (f *lostStmtPG) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	var lenBuf [4]byte
	if _, err := readFull(c, lenBuf[:]); err != nil {
		return
	}
	n := int(uint32(lenBuf[0])<<24 | uint32(lenBuf[1])<<16 | uint32(lenBuf[2])<<8 | uint32(lenBuf[3]))
	if n < 8 {
		return
	}
	body := make([]byte, n-4)
	if _, err := readFull(c, body); err != nil {
		return
	}
	if n == 16 && int32(body[0])<<24|int32(body[1])<<16|int32(body[2])<<8|int32(body[3]) == cancelRequestCode { // a cancel request: its socket closes
		return
	}
	f.mu.Lock()
	f.startups++
	f.mu.Unlock()
	if writeAuthOK(c) != nil || writeBackendKeyData(c, 7, 9) != nil || writeReadyForQuery(c, 'I') != nil {
		return
	}
	var sawParse, sawBind, describePortal bool
	isSelect := false // the statement prepared last returns a row
	lost := false     // set once the first Bind has been answered
	ran := false
	for {
		typ, payload, err := readMsg(c)
		if err != nil || typ == protocol.MsgTerminate {
			return
		}
		switch typ {
		case protocol.MsgParse:
			sawParse = true
			isSelect = bytes.Contains(payload, []byte("SELECT"))
			select {
			case f.parseCh <- f.parses.Add(1):
			default:
			}
		case protocol.MsgDescribe:
			describePortal = len(payload) > 0 && payload[0] == 'P'
		case protocol.MsgBind:
			sawBind = true
			f.binds.Add(1)
		case protocol.MsgExecute:
			f.executes.Add(1)
		case protocol.MsgQuery:
			_ = writeCommandComplete(c, "SELECT 1")
			_ = writeReadyForQuery(c, 'I')
		case protocol.MsgSync:
			switch {
			case sawParse && !sawBind: // a prepare: Parse + Describe + Sync
				if ran && f.reprepareDelay > 0 {
					time.Sleep(f.reprepareDelay)
				}
				desc := fakeMsg(protocol.BackendNoData)
				if isSelect { // one int4 column "c"
					desc = fakeMsg('T', 0, 1, 'c', 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 23, 0, 4, 0xff, 0xff, 0xff, 0xff, 0, 0)
				}
				_, _ = c.Write(fakeReply(fakeMsg(protocol.BackendParseComplete), fakeMsg(protocol.BackendParameterDesc, 0, 1, 0, 0, 0, 23), desc, fakeMsg(protocol.BackendReadyForQuery, 'I')))
			case sawBind && lost:
				// the statement is lost on the server, once
				lost = false
				_, _ = c.Write(fakeReply(fakeMsg(protocol.BackendErrorResponse, []byte("SERROR\x00VERROR\x00C26000\x00Mprepared statement does not exist\x00\x00")...), fakeMsg(protocol.BackendReadyForQuery, 'I')))
			case sawBind:
				ran = true
				lost = true // the next Bind finds the statement gone
				if isSelect {
					var rd []byte
					if describePortal {
						rd = fakeMsg('T', 0, 1, 'c', 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 23, 0, 4, 0xff, 0xff, 0xff, 0xff, 0, 1)
					}
					_, _ = c.Write(fakeReply(fakeMsg(protocol.BackendBindComplete), rd, fakeMsg('D', 0, 1, 0, 0, 0, 4, 0, 0, 0, 1), fakeMsg(protocol.BackendCommandComplete, []byte("SELECT 1\x00")...), fakeMsg(protocol.BackendReadyForQuery, 'I')))
				} else {
					_, _ = c.Write(fakeReply(fakeMsg(protocol.BackendBindComplete), fakeMsg(protocol.BackendCommandComplete, []byte("INSERT 0 1\x00")...), fakeMsg(protocol.BackendReadyForQuery, 'I')))
				}
			}
			sawParse, sawBind, describePortal = false, false, false
		}
	}
}

// After the server lost a cached statement (26000) the driver re-prepares it and
// runs it: ExecContext on a cache hit, and a prepared statement's QueryContext
// or ExecContext (extendedExec, extendedQuery). A context that ends during the
// re-prepare must end the call with its error and the statement must not be sent
// again: no second Bind, no Execute. The conn is a direct one, which does not see
// a cancel-only context until the re-prepare's reply arrives.
func TestRepreparedStatementIsNotSentUnderAnEndedContext(t *testing.T) {
	for _, kind := range []string{"exec", "stmt-exec", "stmt-query"} {
		t.Run(kind, func(t *testing.T) {
			f := startLostStmtPG(t, 100*time.Millisecond)
			host, port, _ := net.SplitHostPort(f.addr)
			dsn := DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable", StatementCacheSize: 16, AutoCacheStatements: true}}
			dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer dcancel()
			conn, err := dialDirectConn(dctx, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			args := []driver.NamedValue{{Ordinal: 1, Value: int64(1)}}
			query := "SELECT $1::int"
			if kind != "stmt-query" {
				query = "INSERT INTO t VALUES ($1)"
			}
			var stmt driver.Stmt
			if kind != "exec" {
				if stmt, err = conn.PrepareContext(dctx, query); err != nil {
					t.Fatal(err)
				}
			}
			run := func(ctx context.Context) error {
				switch kind {
				case "exec":
					_, err := conn.ExecContext(ctx, query, args)
					return err
				case "stmt-exec":
					_, err := stmt.(driver.StmtExecContext).ExecContext(ctx, args)
					return err
				}
				rows, err := stmt.(driver.StmtQueryContext).QueryContext(ctx, args)
				if err != nil {
					return err
				}
				return rows.Close()
			}
			// the first run prepares (exec: caches) and runs the statement
			if err := run(dctx); err != nil {
				t.Fatalf("the first call: %v", err)
			}
			for len(f.parseCh) > 0 {
				<-f.parseCh
			}
			wantParses := f.parses.Load() + 1 // the re-prepare's
			// the second run gets 26000 for the cached statement and re-prepares; the
			// context is cancelled while the re-prepare waits
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			go func() {
				select {
				case <-f.parseCh: // the re-prepare's Parse
					time.Sleep(30 * time.Millisecond)
					stop()
				case <-f.done:
				}
			}()
			errc := make(chan error, 1)
			go func() { errc <- run(ctx) }()
			select {
			case err := <-errc:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("err = %v, want context.Canceled", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the call did not return")
			}
			if _, err := conn.ExecContext(dctx, "SELECT next", nil); err != nil { // orders the counts
				t.Fatalf("the next statement failed: %v", err)
			}
			// The second call's own Bind (answered 26000) is the second Bind; a third
			// is the re-prepared statement sent under the ended context.
			if p, b, e := f.parses.Load(), f.binds.Load(), f.executes.Load(); p != wantParses || b != 2 || e != 2 {
				t.Fatalf("the backend got %d Parse, %d Bind, %d Execute; want %d, 2, 2 (the statement was not sent again after its re-prepare)", p, b, e, wantParses)
			}
		})
	}
}
