package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/goceleris/celeris/driver/internal/looptest"
	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// A row-returning query whose text holds several statements of which MORE THAN
// ONE returns rows used to be answered under the LAST statement's
// RowDescription: wrong column names, the first set's data decoded with the
// second set's types, the second set's rows appended to the first, and (event
// loop, streaming) a data race on the column slice the caller holds. The driver
// has no multiple result sets, so the query now fails with errMultiResultSets
// once the whole reply is read; a query string with one row-returning statement
// is answered as it always was, and Exec accepts any number of statements.
//
// The fake backend below answers by the text of the query:
//
//	"MULTI n"      set 1: n rows of (first_col_a, first_col_b) text; set 2: one row of
//	               (zz_second_1, zz_second_2, zz_second_3) int8
//	"SAME n"       set 1: n rows of (x) int8; set 2: one row of (x) int8 (same shape)
//	"THREE n"      set 1 as MULTI, then two more sets
//	"MULTIERR n"   set 1 as MULTI, set 2, then an ErrorResponse
//	"ONE:<shape>"  one row-returning statement among others (SET / UPDATE / INSERT)
//	"EXECMULTI"    two SELECTs, tags "SELECT 1" and "SELECT 5" (Exec's RowsAffected)
//	"MARK <n>"     one row, one int8 column: n

const multiErrText = "multiple result sets are not supported; the server has executed the statements"

func fakeMultiBackend(tb testing.TB, accepted *atomic.Int32) func(net.Conn) {
	return func(c net.Conn) {
		accepted.Add(1)
		fakePGTrustStartupB(tb, c, 1, 2, func(c net.Conn) {
			for {
				typ, body, err := readMsg(c)
				if err != nil || typ == protocol.MsgTerminate {
					return
				}
				if typ != protocol.MsgQuery {
					continue
				}
				q := strings.TrimRight(string(body), "\x00")
				if _, err := c.Write(fakeMultiReply(q)); err != nil {
					return
				}
			}
		})
	}
}

func fakeCC(tag string) []byte {
	return pgMsg(protocol.BackendCommandComplete, append([]byte(tag), 0))
}

func fakeSet1(n int) []byte {
	var out []byte
	rd := buildRowDescription([]colSpec{
		{name: "first_col_a", typeOID: protocol.OIDText, typeSize: -1, format: protocol.FormatText},
		{name: "first_col_b", typeOID: protocol.OIDText, typeSize: -1, format: protocol.FormatText},
	})
	out = append(out, pgMsg(protocol.BackendRowDescription, rd)...)
	for i := 1; i <= n; i++ {
		out = append(out, pgMsg(protocol.BackendDataRow, buildDataRow([][]byte{
			[]byte("a" + strconv.Itoa(i)), []byte("v" + strconv.Itoa(i)),
		}))...)
	}
	return append(out, fakeCC("SELECT "+strconv.Itoa(n))...)
}

func fakeSet2() []byte {
	var out []byte
	rd := buildRowDescription([]colSpec{
		{name: "zz_second_1", typeOID: protocol.OIDInt8, typeSize: 8, format: protocol.FormatText},
		{name: "zz_second_2", typeOID: protocol.OIDInt8, typeSize: 8, format: protocol.FormatText},
		{name: "zz_second_3", typeOID: protocol.OIDInt8, typeSize: 8, format: protocol.FormatText},
	})
	out = append(out, pgMsg(protocol.BackendRowDescription, rd)...)
	out = append(out, pgMsg(protocol.BackendDataRow, buildDataRow([][]byte{[]byte("1"), []byte("2"), []byte("3")}))...)
	return append(out, fakeCC("SELECT 1")...)
}

func fakeInt8Set(col string, vals ...string) []byte {
	var out []byte
	rd := buildRowDescription([]colSpec{{name: col, typeOID: protocol.OIDInt8, typeSize: 8, format: protocol.FormatText}})
	out = append(out, pgMsg(protocol.BackendRowDescription, rd)...)
	for _, v := range vals {
		out = append(out, pgMsg(protocol.BackendDataRow, buildDataRow([][]byte{[]byte(v)}))...)
	}
	return append(out, fakeCC("SELECT "+strconv.Itoa(len(vals)))...)
}

func fakeMultiReply(q string) []byte {
	verb, arg, _ := strings.Cut(q, " ")
	n, _ := strconv.Atoi(arg)
	var out []byte
	switch verb {
	case "MULTI":
		out = append(fakeSet1(n), fakeSet2()...)
	case "SAME":
		vals := make([]string, n)
		for i := range vals {
			vals[i] = strconv.Itoa(i + 1)
		}
		out = append(fakeInt8Set("x", vals...), fakeInt8Set("x", "1")...)
	case "THREE":
		out = append(fakeSet1(n), fakeSet2()...)
		out = append(out, fakeInt8Set("t3", "9", "8")...)
	case "MULTIERR":
		out = append(fakeSet1(n), fakeSet2()...)
		out = append(out, pgMsg(protocol.BackendErrorResponse, []byte("SERROR\x00C22012\x00Mboom after the second set\x00\x00"))...)
	case "ONE:SET;SELECT":
		out = append(fakeCC("SET"), fakeSet1(n)...)
	case "ONE:SELECT;UPDATE":
		out = append(fakeSet1(n), fakeCC("UPDATE 3")...)
	case "ONE:UPDATE;SELECT":
		out = append(fakeCC("UPDATE 3"), fakeSet1(n)...)
	case "ONE:INSERT;SELECT":
		out = append(fakeCC("INSERT 0 1"), fakeSet1(n)...)
	case "ONE:SELECT;INSERT":
		out = append(fakeSet1(n), fakeCC("INSERT 0 1")...)
	case "ONE:SET;SELECT;UPDATE":
		out = append(fakeCC("SET"), fakeSet1(n)...)
		out = append(out, fakeCC("UPDATE 3")...)
	case "EXECMULTI":
		out = append(fakeInt8Set("a", "1"), fakeInt8Set("b", "1", "2", "3", "4", "5")...)
	case "MARK":
		out = fakeInt8Set("n", arg)
	default:
		out = fakeCC("SELECT 0")
	}
	return append(out, pgMsg(protocol.BackendReadyForQuery, []byte{'I'})...)
}

// multiResult is what one query returned, as seen by the caller.
type multiResult struct {
	queryErr error
	cols     []string
	rows     [][]string
	rowsErr  error
}

// multiRunner runs a query through one of the public surfaces (native Pool,
// database/sql) in one mode, and a marker query on the same connection.
type multiRunner struct {
	name     string
	loop     bool // the event-loop path, whose result can stream
	accepted *atomic.Int32
	query    func(ctx context.Context, q string) multiResult
	exec     func(ctx context.Context, q string) (int64, error)
	queryRow func(ctx context.Context, q string) error // QueryRow(q).Scan into two strings
}

func fakeMultiDSN(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := new(atomic.Int32)
	h := fakeMultiBackend(t, accepted)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go h(c)
		}
	}()
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	return "postgres://u@" + host + ":" + port + "/d?sslmode=disable", accepted
}

// multiRunners are the four modes the bug was reported on: a native Pool on its
// direct path (no engine), a native Pool on the event loop (WithEngine on a
// server that has not started), and database/sql through the Connector, which
// is always on the event loop. Each holds exactly one connection, so that the
// marker query after a failed one runs on the connection that failed.
func multiRunners(t *testing.T) []*multiRunner {
	t.Helper()
	var out []*multiRunner
	for _, loop := range []bool{false, true} {
		dsn, accepted := fakeMultiDSN(t)
		var opts []Option
		name := "pool-direct"
		if loop {
			opts = append(opts, WithEngine(looptest.Unstarted()))
			name = "pool-loop"
		}
		p, err := Open(dsn, append(opts, WithMaxOpen(1))...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = p.Close() })
		out = append(out, &multiRunner{
			name: name, loop: loop, accepted: accepted,
			query: func(ctx context.Context, q string) multiResult {
				var res multiResult
				rows, err := p.QueryContext(ctx, q)
				if err != nil {
					res.queryErr = err
					return res
				}
				defer func() { _ = rows.Close() }()
				res.cols = rows.Columns()
				for rows.Next() {
					cur := rows.Columns() // read while the worker may still be parsing
					dest := make([]any, len(cur))
					vals := make([]string, len(cur))
					for i := range dest {
						dest[i] = &vals[i]
					}
					if err := rows.Scan(dest...); err != nil {
						res.rowsErr = fmt.Errorf("scan: %w", err)
						return res
					}
					res.rows = append(res.rows, vals)
				}
				res.rowsErr = rows.Err()
				return res
			},
			exec: func(ctx context.Context, q string) (int64, error) {
				r, err := p.ExecContext(ctx, q)
				if err != nil {
					return 0, err
				}
				return r.RowsAffected()
			},
			queryRow: func(ctx context.Context, q string) error {
				var a, b string
				return p.QueryRow(ctx, q).Scan(&a, &b)
			},
		})
	}
	dsn, accepted := fakeMultiDSN(t)
	conn, err := NewConnector(dsn)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(conn.WithEngine(looptest.Unstarted()))
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	out = append(out, &multiRunner{
		name: "sql-loop", loop: true, accepted: accepted,
		query: func(ctx context.Context, q string) multiResult {
			var res multiResult
			rows, err := db.QueryContext(ctx, q)
			if err != nil {
				res.queryErr = err
				return res
			}
			defer func() { _ = rows.Close() }()
			res.cols, _ = rows.Columns()
			for rows.Next() {
				cur, _ := rows.Columns()
				dest := make([]any, len(cur))
				vals := make([]string, len(cur))
				for i := range dest {
					dest[i] = &vals[i]
				}
				if err := rows.Scan(dest...); err != nil {
					res.rowsErr = fmt.Errorf("scan: %w", err)
					return res
				}
				res.rows = append(res.rows, vals)
			}
			res.rowsErr = rows.Err()
			return res
		},
		exec: func(ctx context.Context, q string) (int64, error) {
			r, err := db.ExecContext(ctx, q)
			if err != nil {
				return 0, err
			}
			return r.RowsAffected()
		},
		queryRow: func(ctx context.Context, q string) error {
			var a, b string
			return db.QueryRowContext(ctx, q).Scan(&a, &b)
		},
	})
	return out
}

// wantFirstSet checks res is the first set of a MULTI n query: its columns, its
// n rows in order and none of the second set's.
func wantFirstSet(t *testing.T, res multiResult, n int) {
	t.Helper()
	want := []string{"first_col_a", "first_col_b"}
	if strings.Join(res.cols, ",") != strings.Join(want, ",") {
		t.Errorf("Columns = %v, want %v (the first set's)", res.cols, want)
	}
	if len(res.rows) != n {
		t.Errorf("%d rows, want %d", len(res.rows), n)
	}
	for i, r := range res.rows {
		if len(r) != 2 || r[0] != "a"+strconv.Itoa(i+1) || r[1] != "v"+strconv.Itoa(i+1) {
			t.Errorf("row %d = %v, want [a%d v%d]", i, r, i+1, i+1)
			return
		}
	}
}

// checkMultiRejected asserts the contract of a rejected multi-set query: the
// error carries the text, and never a wrong answer. A buffered result (the
// direct path, a small set, the sync fast path) is the error and no rows; a
// streamed one is the first set's n rows and then the error from Rows.Err.
func checkMultiRejected(t *testing.T, r *multiRunner, res multiResult, n int) {
	t.Helper()
	if res.queryErr != nil {
		if !strings.Contains(res.queryErr.Error(), multiErrText) {
			t.Errorf("%s: Query error = %v, want %q", r.name, res.queryErr, multiErrText)
		}
		if res.rows != nil {
			t.Errorf("%s: rows with a Query error", r.name)
		}
		return
	}
	if !r.loop {
		t.Errorf("%s: the buffered path returned rows (%d), want the error and no rows", r.name, len(res.rows))
	}
	wantFirstSet(t, res, n)
	if res.rowsErr == nil || !strings.Contains(res.rowsErr.Error(), multiErrText) {
		t.Errorf("%s: Rows.Err = %v, want %q", r.name, res.rowsErr, multiErrText)
	}
}

func checkMarker(ctx context.Context, t *testing.T, r *multiRunner, v int) {
	t.Helper()
	res := r.query(ctx, "MARK "+strconv.Itoa(v))
	if res.queryErr != nil || res.rowsErr != nil || len(res.rows) != 1 || res.rows[0][0] != strconv.Itoa(v) {
		t.Fatalf("%s: marker %d after the multi-set query = %+v", r.name, v, res)
	}
	if got := r.accepted.Load(); got != 1 {
		t.Fatalf("%s: %d connections were opened, want the one the query ran on", r.name, got)
	}
}

func TestMultiResultSetsRejected(t *testing.T) {
	ctx := tctx(t)
	for _, r := range multiRunners(t) {
		t.Run(r.name, func(t *testing.T) {
			// 64 is streamThreshold: the first set promotes to streaming on its
			// last row and the whole reply can be ready before the caller looks.
			for _, n := range []int{1, 10, 63, 64, 65, 200, 2000} {
				for round := 0; round < 20; round++ {
					res := r.query(ctx, "MULTI "+strconv.Itoa(n))
					checkMultiRejected(t, r, res, n)
					if t.Failed() {
						return
					}
				}
				checkMarker(ctx, t, r, n)
			}
		})
	}
}

// QueryRow takes the first row of a query: a multi-result-set query must be the
// error there too, however many rows the first set has (a streamed set is read
// by Scan before the error is known, which is then the error of Close).
func TestMultiResultSetsQueryRowRejected(t *testing.T) {
	ctx := tctx(t)
	for _, r := range multiRunners(t) {
		t.Run(r.name, func(t *testing.T) {
			for _, n := range []int{1, 10, 63, 64, 65, 2000} {
				for round := 0; round < 20; round++ {
					err := r.queryRow(ctx, "MULTI "+strconv.Itoa(n))
					if err == nil || !strings.Contains(err.Error(), multiErrText) {
						t.Fatalf("%s: QueryRow(MULTI %d) = %v, want %q", r.name, n, err, multiErrText)
					}
				}
				err := r.queryRow(ctx, "ONE:SET;SELECT "+strconv.Itoa(n))
				if err != nil {
					t.Fatalf("%s: QueryRow of one result set = %v", r.name, err)
				}
				checkMarker(ctx, t, r, n)
			}
		})
	}
}

// The same shape in both sets (the second set's rows were appended to the
// first, with no error) and a third set are the same defect.
func TestMultiResultSetsSameShapeAndThree(t *testing.T) {
	ctx := tctx(t)
	for _, r := range multiRunners(t) {
		t.Run(r.name, func(t *testing.T) {
			for round := 0; round < 20; round++ {
				for _, n := range []int{3, 100} {
					res := r.query(ctx, "SAME "+strconv.Itoa(n))
					if res.queryErr == nil && !r.loop {
						t.Fatalf("SAME %d: no error: cols=%v rows=%d", n, res.cols, len(res.rows))
					}
					if res.queryErr != nil {
						if !strings.Contains(res.queryErr.Error(), multiErrText) {
							t.Fatalf("SAME %d: %v", n, res.queryErr)
						}
						continue
					}
					if len(res.rows) != n {
						t.Fatalf("SAME %d: %d rows, want the first set's %d only", n, len(res.rows), n)
					}
					if res.rowsErr == nil || !strings.Contains(res.rowsErr.Error(), multiErrText) {
						t.Fatalf("SAME %d: Rows.Err = %v", n, res.rowsErr)
					}
				}
				res := r.query(ctx, "THREE 5")
				checkMultiRejected(t, r, res, 5)
				res = r.query(ctx, "THREE 500")
				checkMultiRejected(t, r, res, 500)
			}
			checkMarker(ctx, t, r, 1)
		})
	}
}

// A statement that fails after the second set is reported as the server's error,
// as any failed query is; the connection is clean after it.
func TestMultiResultSetsLaterErrorWins(t *testing.T) {
	ctx := tctx(t)
	for _, r := range multiRunners(t) {
		t.Run(r.name, func(t *testing.T) {
			for _, n := range []int{5, 300} {
				res := r.query(ctx, "MULTIERR "+strconv.Itoa(n))
				err := res.queryErr
				if err == nil {
					err = res.rowsErr
				}
				if err == nil || !strings.Contains(err.Error(), "boom after the second set") {
					t.Fatalf("MULTIERR %d: err = %v (query %v, rows %v)", n, err, res.queryErr, res.rowsErr)
				}
			}
			checkMarker(ctx, t, r, 3)
		})
	}
}

// One row-returning statement among others is one result set and is returned
// as it always was.
func TestMultiStatementOneResultSetIsNotRejected(t *testing.T) {
	ctx := tctx(t)
	for _, r := range multiRunners(t) {
		t.Run(r.name, func(t *testing.T) {
			for _, shape := range []string{"SET;SELECT", "SELECT;UPDATE", "UPDATE;SELECT", "INSERT;SELECT", "SELECT;INSERT", "SET;SELECT;UPDATE"} {
				for _, n := range []int{3, 500} {
					res := r.query(ctx, "ONE:"+shape+" "+strconv.Itoa(n))
					if res.queryErr != nil || res.rowsErr != nil {
						t.Fatalf("%s n=%d: query %v, rows %v", shape, n, res.queryErr, res.rowsErr)
					}
					wantFirstSet(t, res, n)
				}
			}
			checkMarker(ctx, t, r, 2)
		})
	}
}

// Exec keeps accepting several statements, SELECTs included (a migration's
// SELECT setval(...); SELECT setval(...)), and reports the last statement's
// count, as it did.
func TestMultiStatementExecUnchanged(t *testing.T) {
	ctx := tctx(t)
	for _, r := range multiRunners(t) {
		t.Run(r.name, func(t *testing.T) {
			for i := 0; i < 3; i++ {
				n, err := r.exec(ctx, "EXECMULTI")
				if err != nil {
					t.Fatalf("Exec: %v", err)
				}
				if n != 5 {
					t.Fatalf("RowsAffected = %d, want 5 (the last statement's count)", n)
				}
			}
			checkMarker(ctx, t, r, 4)
		})
	}
}

// State-machine level: the observer is called once, with the first
// RowDescription, and the columns it was given are never written again.
func TestSimpleQueryStateRejectMultiSet(t *testing.T) {
	feed := func(q *protocol.SimpleQueryState, obs protocol.SimpleQueryObserver, frames [][]byte) (bool, error) {
		var done bool
		var err error
		for _, f := range frames {
			done, err = q.Handle(f[0], f[5:], obs)
			if err != nil {
				return done, err
			}
		}
		return done, nil
	}
	rd1 := pgMsg(protocol.BackendRowDescription, buildRowDescription([]colSpec{{name: "one", typeOID: protocol.OIDText, typeSize: -1}}))
	rd2 := pgMsg(protocol.BackendRowDescription, buildRowDescription([]colSpec{{name: "two", typeOID: protocol.OIDInt8, typeSize: 8}, {name: "three", typeOID: protocol.OIDInt8, typeSize: 8}}))
	row1 := pgMsg(protocol.BackendDataRow, buildDataRow([][]byte{[]byte("x")}))
	row2 := pgMsg(protocol.BackendDataRow, buildDataRow([][]byte{[]byte("1"), []byte("2")}))
	rfq := pgMsg(protocol.BackendReadyForQuery, []byte{'I'})
	frames := [][]byte{rd1, row1, fakeCC("SELECT 1"), rd2, row2, row2, fakeCC("SELECT 2"), rfq}

	for _, reject := range []bool{true, false} {
		var q protocol.SimpleQueryState
		q.RejectMultiSet = reject
		var descs, rows int
		var held []protocol.ColumnDesc
		obs := protocol.HandleCallbacks{
			OnRowDescFn: func(cols []protocol.ColumnDesc) {
				descs++
				if held == nil {
					held = cols
				}
			},
			OnRowFn: func([][]byte) { rows++ },
		}
		done, err := feed(&q, obs, frames)
		if err != nil || !done {
			t.Fatalf("reject=%v: done=%v err=%v", reject, done, err)
		}
		if reject {
			if descs != 1 || rows != 1 || !q.MultiSet {
				t.Errorf("reject: descs=%d rows=%d MultiSet=%v, want 1, 1, true", descs, rows, q.MultiSet)
			}
			if len(held) != 1 || held[0].Name != "one" {
				t.Errorf("reject: the held columns were rewritten: %+v", held)
			}
		} else if descs != 2 || rows != 3 || q.MultiSet {
			t.Errorf("no reject: descs=%d rows=%d MultiSet=%v, want 2, 3, false (Exec's behaviour)", descs, rows, q.MultiSet)
		}
	}
}
