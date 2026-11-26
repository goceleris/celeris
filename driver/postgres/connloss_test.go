package postgres

import (
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/looptest"
	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// A result that the connection's end cut off is an error, never a clean end.
//
// The event loop reports the peer's FIN to onClose as nil, and pgConn.onClose
// used to turn that into io.EOF and hand it to the request in flight. A bare
// io.EOF is what database/sql and Rows.Next read as the end of the rows, so a
// backend that died mid-result (pg_terminate_backend, a crash, a peer that
// closed) ended the Rows with Err() == nil after fewer rows than the query
// produces. These tests run a scripted fake backend that sends part of a reply
// and closes, on every carrier (database/sql DB, Conn and Tx, native Pool and Tx),
// on both engines (the event loop and direct mode), simple and extended
// protocol, below and above the streaming threshold.

// lossKind is where the fake backend ends the statement under test.
type lossKind string

const (
	// lossComplete sends the whole reply, CommandComplete and ReadyForQuery,
	// and closes the connection after it: the result is complete, no error.
	lossComplete lossKind = "closeAfterReady"
	// lossAfterC sends the rows and CommandComplete, then closes: no
	// ReadyForQuery, so the statement is not known to be over (an implicit
	// commit happens at Sync).
	lossAfterC lossKind = "closeBeforeReady"
	// lossAfterLast sends every DataRow and closes before CommandComplete.
	lossAfterLast lossKind = "afterLastRow"
	// lossMid sends half the rows and closes.
	lossMid lossKind = "midStream"
	// lossBeforeFirst sends the RowDescription and closes before any DataRow.
	lossBeforeFirst lossKind = "beforeFirstRow"
	// lossNothing closes the connection without answering.
	lossNothing lossKind = "noReply"
	// lossFatal sends half the rows, then a FATAL 57P01 ErrorResponse (what
	// pg_terminate_backend makes the backend send), then closes.
	lossFatal lossKind = "fatal57P01"
	// lossRandomCut sends the whole reply cut at a random byte, in the middle of a
	// message as often as not, and closes at once (use with noPause).
	lossRandomCut lossKind = "randomByte"
	// lossInsideCC sends every DataRow and half of CommandComplete, then closes.
	lossInsideCC lossKind = "insideCommandComplete"
	// lossReadyMinus1 sends the rows, CommandComplete and ReadyForQuery minus its
	// status byte, then closes.
	lossReadyMinus1 lossKind = "readyMinusOneByte"
	// lossReadyHeader sends the rows, CommandComplete and the 'Z' byte of
	// ReadyForQuery only, then closes.
	lossReadyHeader lossKind = "readyHeaderOnly"
	// lossStall sends half the rows, a pause and ten more, then holds the connection open, reading
	// whatever the client sends, until the client closes it.
	lossStall lossKind = "stall"
)

var allLossKinds = []lossKind{lossComplete, lossAfterC, lossAfterLast, lossMid, lossBeforeFirst, lossNothing, lossFatal}

// lossCfg scripts the fake backend.
type lossCfg struct {
	kind lossKind
	n    int  // rows the statement produces
	rst  bool // close with a reset instead of a FIN
	exec bool // the statement is a command with no rows (CommandComplete UPDATE n)
	// noPause closes the connection right after the last byte of the cut reply, with
	// no pause: the reader finds the data and the end of the connection together.
	noPause bool
}

func (cfg lossCfg) cut(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok && cfg.rst {
		_ = tc.SetLinger(0)
	}
	_ = c.Close()
}

// lossServer is a fake backend for the simple and the extended protocol: every
// Query ('Q') whose text starts with "SELECT g" or "UPDATE" and every Execute
// is answered by the script; BEGIN, COMMIT and ROLLBACK and anything else are
// answered with a CommandComplete and a ReadyForQuery.
func lossServer(tb testing.TB, cfg lossCfg) func(net.Conn) {
	cols := buildRowDescription([]colSpec{{name: "g", typeOID: protocol.OIDInt8, typeSize: 8, format: protocol.FormatText}})
	// An extended statement asks for binary results of the types it knows.
	colsBin := buildRowDescription([]colSpec{{name: "g", typeOID: protocol.OIDInt8, typeSize: 8, format: protocol.FormatBinary}})
	return func(c net.Conn) {
		fakePGTrustStartupB(tb, c, 1, 2, func(c net.Conn) {
			status := byte('I')
			var buf []byte
			add := func(typ byte, payload []byte) { buf = append(buf, pgMsg(typ, payload)...) }
			flush := func() bool {
				if len(buf) == 0 {
					return true
				}
				_, err := c.Write(buf)
				buf = buf[:0]
				return err == nil
			}
			// script appends the statement under test's reply: rows, then the end.
			// It returns true when the script closed the connection.
			script := func(extended bool) bool {
				rows := func(from, to int) {
					for i := from; i < to; i++ {
						v := []byte(strconv.Itoa(i + 1))
						if extended {
							v = binary.BigEndian.AppendUint64(nil, uint64(i+1))
						}
						add(protocol.BackendDataRow, buildDataRow([][]byte{v}))
					}
				}
				tag := "SELECT " + strconv.Itoa(cfg.n)
				if cfg.exec {
					tag = "UPDATE " + strconv.Itoa(cfg.n)
				}
				header := func() {
					// An extended statement's RowDescription is the answer of its Describe.
					if !cfg.exec && !extended {
						add(protocol.BackendRowDescription, cols)
					}
				}
				n := cfg.n
				if cfg.exec {
					n = 0
				}
				switch cfg.kind {
				case lossComplete:
					header()
					rows(0, n)
					add(protocol.BackendCommandComplete, append([]byte(tag), 0))
					add(protocol.BackendReadyForQuery, []byte{status})
					flush()
					// The connection closes at once after the reply, FIN or RST, with no
					// pause: the result is complete (ReadyForQuery was sent) and stays so.
					cfg.cut(c)
					return true
				case lossAfterC:
					header()
					rows(0, n)
					add(protocol.BackendCommandComplete, append([]byte(tag), 0))
				case lossAfterLast:
					header()
					rows(0, n)
				case lossRandomCut:
					header()
					rows(0, n)
					add(protocol.BackendCommandComplete, append([]byte(tag), 0))
					add(protocol.BackendReadyForQuery, []byte{status})
					buf = buf[:rand.IntN(len(buf)-1)]
				case lossInsideCC:
					header()
					rows(0, n)
					cc := pgMsg(protocol.BackendCommandComplete, append([]byte(tag), 0))
					buf = append(buf, cc[:len(cc)/2]...)
				case lossReadyMinus1, lossReadyHeader:
					header()
					rows(0, n)
					add(protocol.BackendCommandComplete, append([]byte(tag), 0))
					z := pgMsg(protocol.BackendReadyForQuery, []byte{status})
					if cfg.kind == lossReadyMinus1 {
						buf = append(buf, z[:len(z)-1]...)
					} else {
						buf = append(buf, z[0])
					}
				case lossMid:
					header()
					rows(0, n/2)
				case lossBeforeFirst:
					header()
				case lossNothing:
				case lossStall:
					header()
					rows(0, n/2)
					flush()
					// More rows after the first poll returned: a request that buffered the
					// first batch on the sync path is promoted to a stream by the next one.
					time.Sleep(150 * time.Millisecond)
					rows(n/2, n/2+10)
					flush()
					for {
						if _, _, err := readMsg(c); err != nil {
							return true
						}
					}
				case lossFatal:
					header()
					rows(0, n/2)
					add(protocol.BackendErrorResponse, []byte("SFATAL\x00VFATAL\x00C57P01\x00Mterminating connection due to administrator command\x00\x00"))
				}
				flush()
				if !cfg.noPause {
					time.Sleep(20 * time.Millisecond)
				}
				cfg.cut(c)
				return true
			}
			plain := func(q string) {
				switch q {
				case "BEGIN":
					status = 'T'
					add(protocol.BackendCommandComplete, append([]byte("BEGIN"), 0))
				case "COMMIT", "ROLLBACK":
					status = 'I'
					add(protocol.BackendCommandComplete, append([]byte(q), 0))
				default:
					add(protocol.BackendCommandComplete, append([]byte("SELECT 0"), 0))
				}
				add(protocol.BackendReadyForQuery, []byte{status})
				flush()
			}
			executed := false
			for {
				typ, body, err := readMsg(c)
				if err != nil {
					return
				}
				switch typ {
				case protocol.MsgTerminate:
					return
				case protocol.MsgQuery:
					q := string(body[:len(body)-1])
					if strings.HasPrefix(q, "SELECT g") || strings.HasPrefix(q, "UPDATE") {
						script(false)
						return
					}
					plain(q)
				case protocol.MsgParse:
					add(protocol.BackendParseComplete, nil)
				case protocol.MsgDescribe:
					if len(body) > 0 && body[0] == 'S' {
						add(protocol.BackendParameterDesc, buildParameterDescription([]uint32{protocol.OIDInt8}))
					}
					if cfg.exec {
						add(protocol.BackendNoData, nil)
					} else {
						add(protocol.BackendRowDescription, colsBin)
					}
				case protocol.MsgBind:
					add(protocol.BackendBindComplete, nil)
				case protocol.MsgExecute:
					executed = true
				case protocol.MsgSync:
					if !executed {
						add(protocol.BackendReadyForQuery, []byte{status})
						flush()
						break
					}
					// The Sync of the statement under test: its rows and its end. Every
					// byte of the client is read by now, so a close is a FIN, not a reset.
					if script(true) {
						return
					}
				case protocol.MsgClose:
					add(protocol.BackendCloseComplete, nil)
					flush()
				}
			}
		})
	}
}

// lossCarrier runs the statement under test and reports how many rows it read,
// and the error it ended with. query is the statement; ext makes it an
// extended-protocol statement (an argument); loop selects the engine.
type lossCarrier struct {
	name string
	// directOK is false for a carrier that exists only on the event loop.
	directOK bool
	run      func(t *testing.T, addr string, direct bool, ext, exec bool) (rows int, err error)
}

func lossDSN(addr string) string {
	host, port, _ := net.SplitHostPort(addr)
	return "postgres://u@" + host + ":" + port + "/d?sslmode=disable"
}

func lossPool(t *testing.T, addr string, direct bool) *Pool {
	t.Helper()
	opts := []Option{WithMaxOpen(1)}
	if !direct {
		opts = append(opts, WithEngine(looptest.Unstarted()))
	}
	p, err := Open(lossDSN(addr), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func lossSQL(t *testing.T, addr string) *sql.DB {
	t.Helper()
	db, err := sql.Open(DriverName, lossDSN(addr))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func lossQuery(exec bool, ext bool) (string, []any) {
	var q string
	if exec {
		q = "UPDATE t SET x = 1"
	} else {
		q = "SELECT g FROM t"
	}
	if ext {
		return q + " WHERE x > $1", []any{int64(0)}
	}
	return q, nil
}

// readSQLRows drains database/sql rows, checking the marker column g = 1, 2, 3...
func readSQLRows(rows *sql.Rows) (int, error) {
	n := 0
	for rows.Next() {
		var g int64
		if err := rows.Scan(&g); err != nil {
			_ = rows.Close()
			return n, err
		}
		if g != int64(n+1) {
			_ = rows.Close()
			return n, fmt.Errorf("row %d carries marker %d", n+1, g)
		}
		n++
	}
	err := rows.Err()
	_ = rows.Close()
	return n, err
}

func readPoolRows(rows *Rows) (int, error) {
	n := 0
	for rows.Next() {
		var g int64
		if err := rows.Scan(&g); err != nil {
			_ = rows.Close()
			return n, err
		}
		if g != int64(n+1) {
			_ = rows.Close()
			return n, fmt.Errorf("row %d carries marker %d", n+1, g)
		}
		n++
	}
	err := rows.Err()
	_ = rows.Close()
	return n, err
}

func lossCarriers() []lossCarrier {
	return []lossCarrier{
		{"sqlDB", false, func(t *testing.T, addr string, _ bool, ext, exec bool) (int, error) {
			db := lossSQL(t, addr)
			q, args := lossQuery(exec, ext)
			ctx := tctx(t)
			if exec {
				_, err := db.ExecContext(ctx, q, args...)
				return 0, err
			}
			rows, err := db.QueryContext(ctx, q, args...)
			if err != nil {
				return 0, err
			}
			return readSQLRows(rows)
		}},
		{"sqlConn", false, func(t *testing.T, addr string, _ bool, ext, exec bool) (int, error) {
			db := lossSQL(t, addr)
			q, args := lossQuery(exec, ext)
			ctx := tctx(t)
			conn, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			if exec {
				_, err := conn.ExecContext(ctx, q, args...)
				return 0, err
			}
			rows, err := conn.QueryContext(ctx, q, args...)
			if err != nil {
				return 0, err
			}
			return readSQLRows(rows)
		}},
		{"sqlTx", false, func(t *testing.T, addr string, _ bool, ext, exec bool) (int, error) {
			db := lossSQL(t, addr)
			q, args := lossQuery(exec, ext)
			ctx := tctx(t)
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			if exec {
				_, err := tx.ExecContext(ctx, q, args...)
				return 0, err
			}
			rows, err := tx.QueryContext(ctx, q, args...)
			if err != nil {
				return 0, err
			}
			return readSQLRows(rows)
		}},
		{"poolQuery", true, func(t *testing.T, addr string, direct bool, ext, exec bool) (int, error) {
			p := lossPool(t, addr, direct)
			q, args := lossQuery(exec, ext)
			ctx := tctx(t)
			if exec {
				_, err := p.ExecContext(ctx, q, args...)
				return 0, err
			}
			rows, err := p.QueryContext(ctx, q, args...)
			if err != nil {
				return 0, err
			}
			return readPoolRows(rows)
		}},
		{"poolTx", true, func(t *testing.T, addr string, direct bool, ext, exec bool) (int, error) {
			p := lossPool(t, addr, direct)
			q, args := lossQuery(exec, ext)
			ctx := tctx(t)
			tx, err := p.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			if exec {
				_, err := tx.ExecContext(ctx, q, args...)
				return 0, err
			}
			rows, err := tx.QueryContext(ctx, q, args...)
			if err != nil {
				return 0, err
			}
			return readPoolRows(rows)
		}},
	}
}

// checkLoss is the assertion of every cell: a clean end for lossComplete, and an
// error for every cut: one that matches ErrClosed and io.ErrUnexpectedEOF and
// neither io.EOF nor driver.ErrBadConn, or the server's own *PGError when it sent
// one.
func checkLoss(t *testing.T, cfg lossCfg, rows int, err error) {
	t.Helper()
	if cfg.kind == lossComplete {
		if err != nil {
			t.Errorf("a result complete before the close ended with error %v", err)
		}
		if !cfg.exec && rows != cfg.n {
			t.Errorf("a result complete before the close read %d of %d rows", rows, cfg.n)
		}
		return
	}
	if err == nil {
		t.Fatalf("Err() == nil after %d of %d rows: a result cut off by the connection ended as if complete", rows, cfg.n)
	}
	if rows > cfg.n {
		t.Errorf("read %d rows of %d", rows, cfg.n)
	}
	if errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("error %v is a bare io.EOF", err)
	}
	if err == io.EOF {
		t.Errorf("error is io.EOF itself")
	}
	if errors.Is(err, driver.ErrBadConn) {
		t.Errorf("error %v matches driver.ErrBadConn: database/sql would retry the statement", err)
	}
	if errors.Is(err, sql.ErrNoRows) {
		t.Errorf("error %v matches sql.ErrNoRows", err)
	}
	if cfg.kind == lossFatal && !cfg.exec {
		// With a FIN the ErrorResponse is read before the close. A reset may drop
		// bytes the application has not read yet, so then the error is the server's
		// or the loss error.
		var pe *PGError
		if errors.As(err, &pe) {
			if pe.Code != "57P01" {
				t.Errorf("error %v: want 57P01", pe)
			}
			return
		}
		if !cfg.rst {
			t.Errorf("error %v: want the server's *PGError 57P01 (it arrived before the close)", err)
			return
		}
	}
	if cfg.kind == lossFatal && cfg.exec && !cfg.rst {
		return
	}
	if !errors.Is(err, ErrClosed) {
		t.Errorf("error %v does not match ErrClosed", err)
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("error %v does not match io.ErrUnexpectedEOF", err)
	}
}

func TestConnLossIsNotEOF(t *testing.T) {
	for _, carrier := range lossCarriers() {
		for _, engine := range []string{"loop", "direct"} {
			if engine == "direct" && !carrier.directOK {
				continue
			}
			for _, ext := range []bool{false, true} {
				for _, n := range []int{50, 500} {
					for _, kind := range allLossKinds {
						for _, rst := range []bool{false, true} {
							if rst && kind == lossComplete && !rstAfterCompleteIsClean {
								continue
							}
							proto := "simple"
							if ext {
								proto = "extended"
							}
							cut := "FIN"
							if rst {
								cut = "RST"
							}
							cfg := lossCfg{kind: kind, n: n, rst: rst}
							name := fmt.Sprintf("%s/%s/%s/rows%d/%s/%s", carrier.name, engine, proto, n, kind, cut)
							t.Run(name, func(t *testing.T) {
								t.Parallel()
								addr := startFakePG(t, lossServer(t, cfg))
								rows, err := carrier.run(t, addr, engine == "direct", ext, false)
								checkLoss(t, cfg, rows, err)
							})
						}
					}
				}
			}
		}
	}
}

// TestConnLossExecIsNotSuccess: a command whose CommandComplete or ReadyForQuery
// never arrived did not finish as far as the caller can tell: Exec returns an
// error.
func TestConnLossExecIsNotSuccess(t *testing.T) {
	for _, carrier := range lossCarriers() {
		for _, engine := range []string{"loop", "direct"} {
			if engine == "direct" && !carrier.directOK {
				continue
			}
			for _, ext := range []bool{false, true} {
				for _, kind := range []lossKind{lossComplete, lossAfterC, lossAfterLast, lossBeforeFirst, lossNothing, lossFatal} {
					cfg := lossCfg{kind: kind, n: 1, exec: true}
					name := fmt.Sprintf("%s/%s/ext=%v/%s", carrier.name, engine, ext, kind)
					t.Run(name, func(t *testing.T) {
						t.Parallel()
						addr := startFakePG(t, lossServer(t, cfg))
						_, err := carrier.run(t, addr, engine == "direct", ext, true)
						checkLoss(t, cfg, 0, err)
					})
				}
			}
		}
	}
}

// TestConnLossErrorShape pins what the loss error matches.
func TestConnLossErrorShape(t *testing.T) {
	for _, cause := range []error{nil, io.EOF, io.ErrUnexpectedEOF, ErrClosed, io.EOF, errors.New("boom"), fmt.Errorf("read: %w", io.EOF)} {
		err := lostError(cause)
		if !errors.Is(err, ErrClosed) || !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("lostError(%v) = %v: must match ErrClosed and io.ErrUnexpectedEOF", cause, err)
		}
		if errors.Is(err, io.EOF) || errors.Is(err, driver.ErrBadConn) {
			t.Errorf("lostError(%v) = %v: must not match io.EOF or driver.ErrBadConn", cause, err)
		}
	}
	boom := errors.New("boom")
	if !errors.Is(lostError(boom), boom) {
		t.Errorf("lostError(boom) does not keep its cause")
	}
}
