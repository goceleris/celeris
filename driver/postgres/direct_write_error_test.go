package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/looptest"
	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// A connection whose socket failed a write must not go back into the pool.
//
// A direct-mode conn whose peer sent a RST fails the next write into it
// ("write: connection reset by peer", then "write: broken pipe"). writeRaw
// returned that error and left the conn open, so Pool.release put it back, and
// the next call got the same dead socket, until the pool's health check (30 s)
// closed it. A FIN heals (the first write succeeds, the read sees EOF and
// fails the conn), and so does the event loop (its write error closes the conn).
//
// The backend below answers every statement of a session and, on killAll,
// closes every live session with a RST (SetLinger(0)) or a FIN. After the kill
// each call of the test runs on a pool whose every conn is dead: each dead conn
// may fail ONE call, then the pool must run on fresh conns.

// poisonPG is a backend that answers simple and extended statements with one
// row (a statement whose text contains SELECT) or a CommandComplete, tracks the
// transaction status, and can reset or close every live session.
type poisonPG struct {
	addr string
	done chan struct{}

	mu       sync.Mutex
	conns    []net.Conn
	startups int
}

func startPoisonPG(t *testing.T) *poisonPG {
	t.Helper()
	f := &poisonPG{done: make(chan struct{})}
	f.addr = startFakePG(t, f.serve)
	t.Cleanup(func() { close(f.done) })
	return f
}

func (f *poisonPG) sessions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.startups
}

// killAll closes every live session: with a RST when rst is set, with a FIN
// otherwise. It returns the number of sessions it closed.
func (f *poisonPG) killAll(rst bool) int {
	f.mu.Lock()
	conns := f.conns
	f.conns = nil
	f.mu.Unlock()
	for _, c := range conns {
		if tc, ok := c.(*net.TCPConn); ok && rst {
			_ = tc.SetLinger(0)
		}
		_ = c.Close()
	}
	return len(conns)
}

func poisonMsg(typ byte, payload ...byte) []byte {
	n := 4 + len(payload)
	return append([]byte{typ, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}, payload...)
}

// poisonRowDesc is a RowDescription body of one int4 column "c" in the format
// (0 text, 1 binary).
func poisonRowDesc(format byte) []byte {
	return []byte{0, 1, 'c', 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 23, 0, 4, 0xff, 0xff, 0xff, 0xff, 0, format}
}

func poisonCat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func (f *poisonPG) serve(c net.Conn) {
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
	if n == 16 && int32(body[0])<<24|int32(body[1])<<16|int32(body[2])<<8|int32(body[3]) == cancelRequestCode {
		return // a cancel request: the socket closes
	}
	f.mu.Lock()
	f.startups++
	f.conns = append(f.conns, c)
	f.mu.Unlock()
	if writeAuthOK(c) != nil || writeBackendKeyData(c, 7, 9) != nil || writeReadyForQuery(c, 'I') != nil {
		return
	}
	inTx := false
	status := func() byte {
		if inTx {
			return 'T'
		}
		return 'I'
	}
	var sawParse, sawBind, sawExec, describeStmt, describePortal, isSelect bool
	for {
		typ, payload, err := readMsg(c)
		if err != nil || typ == protocol.MsgTerminate {
			return
		}
		switch typ {
		case protocol.MsgQuery:
			q := strings.ToUpper(strings.TrimRight(string(payload), "\x00"))
			if strings.Contains(q, "SLOW") {
				time.Sleep(150 * time.Millisecond)
			}
			tag := "INSERT 0 1"
			switch {
			case strings.HasPrefix(q, "BEGIN"):
				inTx, tag = true, "BEGIN"
			case strings.HasPrefix(q, "COMMIT"):
				inTx, tag = false, "COMMIT"
			case strings.HasPrefix(q, "ROLLBACK"):
				inTx, tag = false, "ROLLBACK"
			}
			if strings.Contains(q, "SELECT") {
				_, _ = c.Write(poisonCat(poisonMsg('T', poisonRowDesc(0)...), poisonMsg('D', 0, 1, 0, 0, 0, 1, '1'), poisonMsg(protocol.BackendCommandComplete, []byte("SELECT 1\x00")...), poisonMsg(protocol.BackendReadyForQuery, status())))
			} else {
				_, _ = c.Write(poisonCat(poisonMsg(protocol.BackendCommandComplete, []byte(tag+"\x00")...), poisonMsg(protocol.BackendReadyForQuery, status())))
			}
		case protocol.MsgParse:
			sawParse = true
			isSelect = bytes.Contains(bytes.ToUpper(payload), []byte("SELECT"))
		case protocol.MsgDescribe:
			if len(payload) > 0 && payload[0] == 'P' {
				describePortal = true
			} else {
				describeStmt = true
			}
		case protocol.MsgBind:
			sawBind = true
		case protocol.MsgExecute:
			sawExec = true
		case protocol.MsgSync:
			var out []byte
			if sawParse {
				out = append(out, poisonMsg(protocol.BackendParseComplete)...)
			}
			if describeStmt {
				out = append(out, poisonMsg(protocol.BackendParameterDesc, 0, 1, 0, 0, 0, 23)...)
				if isSelect {
					out = append(out, poisonMsg('T', poisonRowDesc(0)...)...)
				} else {
					out = append(out, poisonMsg(protocol.BackendNoData)...)
				}
			}
			if sawBind {
				out = append(out, poisonMsg(protocol.BackendBindComplete)...)
			}
			if describePortal {
				if isSelect {
					out = append(out, poisonMsg('T', poisonRowDesc(1)...)...)
				} else {
					out = append(out, poisonMsg(protocol.BackendNoData)...)
				}
			}
			if sawExec {
				if isSelect {
					out = append(out, poisonMsg('D', 0, 1, 0, 0, 0, 4, 0, 0, 0, 1)...)
					out = append(out, poisonMsg(protocol.BackendCommandComplete, []byte("SELECT 1\x00")...)...)
				} else {
					out = append(out, poisonMsg(protocol.BackendCommandComplete, []byte("INSERT 0 1\x00")...)...)
				}
			}
			out = append(out, poisonMsg(protocol.BackendReadyForQuery, status())...)
			_, _ = c.Write(out)
			sawParse, sawBind, sawExec, describeStmt, describePortal = false, false, false, false, false
		}
	}
}

// poisonArm is one way to reach the driver with up to n conns.
type poisonArm struct {
	name string
	open func(t *testing.T, addr string, n int) poisonDB
}

// poisonDB is what the tests need from a pool, native or database/sql.
type poisonDB interface {
	// call runs one statement of the kind: exec-simple, exec-args, query-simple,
	// query-args, queryrow.
	call(ctx context.Context, kind string) error
	// begin starts a transaction.
	begin(ctx context.Context) (poisonTx, error)
	// open, idle and inUse are the pool's counts.
	stats() (open, idle, inUse int)
}

type poisonTx interface {
	exec(ctx context.Context) error
	commit() error
	rollback() error
}

const (
	poisonExecSQL   = "INSERT INTO t VALUES ($1)"
	poisonSelectSQL = "SELECT $1::int"
)

type poisonPool struct{ p *Pool }

func (d poisonPool) call(ctx context.Context, kind string) error {
	switch kind {
	case "exec-simple":
		_, err := d.p.ExecContext(ctx, "INSERT INTO t VALUES (1)")
		return err
	case "exec-args":
		_, err := d.p.ExecContext(ctx, poisonExecSQL, 1)
		return err
	case "query-simple", "query-args":
		var rows *Rows
		var err error
		if kind == "query-simple" {
			rows, err = d.p.QueryContext(ctx, "SELECT 1")
		} else {
			rows, err = d.p.QueryContext(ctx, poisonSelectSQL, 1)
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
	var v int
	return d.p.QueryRow(ctx, poisonSelectSQL, 1).Scan(&v)
}

func (d poisonPool) begin(ctx context.Context) (poisonTx, error) {
	tx, err := d.p.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return poisonPoolTx{tx}, nil
}

func (d poisonPool) stats() (int, int, int) {
	s := d.p.Stats()
	return s.Open, s.Idle, s.InUse
}

type poisonPoolTx struct{ tx *Tx }

func (x poisonPoolTx) exec(ctx context.Context) error {
	_, err := x.tx.ExecContext(ctx, "INSERT INTO t VALUES (1)")
	return err
}
func (x poisonPoolTx) commit() error   { return x.tx.Commit() }
func (x poisonPoolTx) rollback() error { return x.tx.Rollback() }

type poisonSQL struct{ db *sql.DB }

func (d poisonSQL) call(ctx context.Context, kind string) error {
	switch kind {
	case "exec-simple":
		_, err := d.db.ExecContext(ctx, "INSERT INTO t VALUES (1)")
		return err
	case "exec-args":
		_, err := d.db.ExecContext(ctx, poisonExecSQL, 1)
		return err
	case "query-simple", "query-args":
		var rows *sql.Rows
		var err error
		if kind == "query-simple" {
			rows, err = d.db.QueryContext(ctx, "SELECT 1")
		} else {
			rows, err = d.db.QueryContext(ctx, poisonSelectSQL, 1)
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
	var v int
	return d.db.QueryRowContext(ctx, poisonSelectSQL, 1).Scan(&v)
}

func (d poisonSQL) begin(ctx context.Context) (poisonTx, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return poisonSQLTx{tx}, nil
}

func (d poisonSQL) stats() (int, int, int) {
	s := d.db.Stats()
	return s.OpenConnections, s.Idle, s.InUse
}

type poisonSQLTx struct{ tx *sql.Tx }

func (x poisonSQLTx) exec(ctx context.Context) error {
	_, err := x.tx.ExecContext(ctx, "INSERT INTO t VALUES (1)")
	return err
}
func (x poisonSQLTx) commit() error   { return x.tx.Commit() }
func (x poisonSQLTx) rollback() error { return x.tx.Rollback() }

func poisonArms() []poisonArm {
	dsnFor := func(addr string) string {
		host, port, _ := net.SplitHostPort(addr)
		return "postgres://u@" + host + ":" + port + "/d?sslmode=disable"
	}
	return []poisonArm{
		{"pool-direct", func(t *testing.T, addr string, n int) poisonDB {
			p, err := Open(dsnFor(addr), WithMaxOpen(n))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Close() })
			return poisonPool{p}
		}},
		{"pool-loop", func(t *testing.T, addr string, n int) poisonDB {
			p, err := Open(dsnFor(addr), WithMaxOpen(n), WithEngine(looptest.Unstarted()))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Close() })
			return poisonPool{p}
		}},
		{"database-sql", func(t *testing.T, addr string, n int) poisonDB {
			db, err := sql.Open(DriverName, dsnFor(addr))
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(n)
			db.SetMaxIdleConns(n)
			t.Cleanup(func() { _ = db.Close() })
			return poisonSQL{db}
		}},
	}
}

// poisonWarm opens n conns: n concurrent statements that the backend holds for
// 150 ms cannot share a conn.
func poisonWarm(t *testing.T, db poisonDB, f *poisonPG, n int) {
	t.Helper()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			switch d := db.(type) {
			case poisonPool:
				_, err := d.p.ExecContext(ctx, "INSERT INTO slow VALUES (1)")
				errs <- err
			case poisonSQL:
				_, err := d.db.ExecContext(ctx, "INSERT INTO slow VALUES (1)")
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("warm-up: %v", err)
		}
	}
	if got := f.sessions(); got != n {
		t.Fatalf("warm-up opened %d sessions, want %d", got, n)
	}
}

// poisonLocalPort is the local port of the socket a failed call used, when its
// error carries it (a write or read error of a direct conn), else 0.
func poisonLocalPort(err error) int {
	var oe *net.OpError
	if errors.As(err, &oe) {
		if a, ok := oe.Source.(*net.TCPAddr); ok {
			return a.Port
		}
	}
	return 0
}

// After every pooled conn was reset (or closed), each dead conn fails at most one
// call and the pool then runs on fresh conns; a failed call never leaves a dead
// socket in the pool for the next one.
func TestPoolRunsOnFreshConnsAfterEveryConnWasKilled(t *testing.T) {
	for _, arm := range poisonArms() {
		for _, kind := range []string{"exec-simple", "exec-args", "query-simple", "query-args", "queryrow"} {
			for _, mode := range []string{"rst", "fin"} {
				for _, n := range []int{1, 3} {
					t.Run(fmt.Sprintf("%s/%s/%s/conns=%d", arm.name, kind, mode, n), func(t *testing.T) {
						f := startPoisonPG(t)
						db := arm.open(t, f.addr, n)
						poisonWarm(t, db, f, n)
						if got := f.killAll(mode == "rst"); got != n {
							t.Fatalf("killed %d sessions, want %d", got, n)
						}
						var failed []error
						lastFail := -1
						const calls = 12
						for i := 0; i < calls; i++ {
							// A pause before each call: the event loop tears a conn down a moment
							// before its close callback marks the conn closed, and a call inside
							// that window finds a conn that is dead but not yet marked.
							time.Sleep(20 * time.Millisecond)
							ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
							err := db.call(ctx, kind)
							cancel()
							if err != nil {
								failed = append(failed, err)
								lastFail = i
							}
						}
						if len(failed) > n {
							t.Fatalf("%d of %d calls failed on a pool of %d dead conns (at most one per dead conn); the errors: %v", len(failed), calls, n, failed)
						}
						if lastFail >= n {
							t.Fatalf("a call failed at position %d: the pool kept running on a dead conn after the first %d calls; errors: %v", lastFail+1, n, failed)
						}
						if n == 1 && mode == "rst" && arm.name == "pool-direct" && len(failed) != 1 {
							t.Fatalf("%d calls failed, want exactly 1: the one dead conn fails one call and is replaced", len(failed))
						}
						// the same socket never fails two calls (a local port is in the error of
						// a direct conn's write or read error)
						ports := map[int]bool{}
						for _, err := range failed {
							if p := poisonLocalPort(err); p != 0 {
								if ports[p] {
									t.Fatalf("local port %d failed two calls: the same dead socket was used twice (%v)", p, failed)
								}
								ports[p] = true
							}
						}
						if got := f.sessions(); got <= n {
							t.Fatalf("sessions = %d: no fresh conn was dialled after the kill", got)
						}
						open, idle, inUse := db.stats()
						if inUse != 0 || open > n || idle > n {
							t.Fatalf("pool after the run: open %d idle %d in use %d (max %d): a slot leaked", open, idle, inUse, n)
						}
					})
				}
			}
		}
	}
}

// Transaction control on a reset conn: BEGIN into a dead conn, and a conn reset
// in the middle of a transaction (the next statement, Commit and Rollback each
// fail on it). The failed call returns its error, the conn is discarded and the
// next Begin runs on a fresh one; no pool slot leaks.
func TestPoolBeginWorksAfterTheConnWasKilled(t *testing.T) {
	for _, arm := range poisonArms() {
		for _, step := range []string{"begin", "exec", "commit", "rollback"} {
			for _, mode := range []string{"rst", "fin"} {
				t.Run(fmt.Sprintf("%s/%s/%s", arm.name, step, mode), func(t *testing.T) {
					f := startPoisonPG(t)
					db := arm.open(t, f.addr, 1)
					poisonWarm(t, db, f, 1)
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					defer cancel()
					var tx poisonTx
					if step != "begin" {
						var err error
						if tx, err = db.begin(ctx); err != nil {
							t.Fatalf("Begin: %v", err)
						}
					}
					if got := f.killAll(mode == "rst"); got != 1 {
						t.Fatalf("killed %d sessions, want 1", got)
					}
					time.Sleep(20 * time.Millisecond) // see TestPoolRunsOnFreshConnsAfterEveryConnWasKilled
					failures := 0
					switch step {
					case "begin":
						if tx0, err := db.begin(ctx); err != nil {
							failures++
						} else {
							// the conn was already dropped (the event loop saw the reset)
							_ = tx0.rollback()
						}
					case "exec":
						if err := tx.exec(ctx); err == nil {
							t.Fatal("a statement in a transaction on a killed conn succeeded")
						}
						_ = tx.rollback()
					case "commit":
						if err := tx.commit(); err == nil {
							t.Fatal("Commit on a killed conn succeeded")
						}
						_ = tx.rollback()
					case "rollback":
						_ = tx.rollback()
					}
					// the next transaction works, on a fresh conn, within one more failure
					// at most (a fin leaves the dead conn to be found by the next read)
					var ok bool
					for attempt := 0; attempt < 2 && !ok; attempt++ {
						tx2, err := db.begin(ctx)
						if err != nil {
							failures++
							continue
						}
						if err := tx2.exec(ctx); err != nil {
							_ = tx2.rollback()
							failures++
							continue
						}
						if err := tx2.commit(); err != nil {
							failures++
							continue
						}
						ok = true
					}
					if !ok {
						t.Fatal("no transaction worked after the conn was killed: the pool kept the dead conn")
					}
					if f.sessions() < 2 {
						t.Fatalf("sessions = %d: no fresh conn was dialled", f.sessions())
					}
					open, idle, inUse := db.stats()
					if inUse != 0 || open > 1 || idle > 1 {
						t.Fatalf("pool after the run: open %d idle %d in use %d (max 1): a slot leaked", open, idle, inUse)
					}
				})
			}
		}
	}
}
