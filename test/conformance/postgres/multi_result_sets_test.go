//go:build postgres

package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	postgres "github.com/goceleris/celeris/driver/postgres"
)

// A query string with several statements of which MORE THAN ONE returns rows
// used to be answered under the LAST statement's RowDescription: the second
// statement's column names, the first one's data decoded with the second one's
// types (strconv.ParseInt: parsing "v1"), the second one's row appended to the
// first set, and under -race a data race on the column slice the caller holds.
// The driver has no multiple result sets, so such a query now fails once the
// whole reply is read, with an error that says the server ran the statements and
// leaves the connection ready for the next query. One row-returning statement
// among others is one result set and is returned as before; Exec accepts any
// number of statements.

const multiSetErr = "multiple result sets are not supported; the server has executed the statements"

// multiSetRounds is how many times each size of the probe runs per mode
// (CELERIS_PG_MULTISET_ROUNDS overrides it: 300 for the full probe).
func multiSetRounds() int {
	if n, err := strconv.Atoi(os.Getenv("CELERIS_PG_MULTISET_ROUNDS")); err == nil && n > 0 {
		return n
	}
	return 10
}

type multiSetOut struct {
	queryErr error
	cols     []string
	rows     [][]string
	rowsErr  error
}

type multiSetMode struct {
	name  string
	query func(ctx context.Context, q string) multiSetOut
	exec  func(ctx context.Context, q string) (int64, error)
	// queryRow is QueryRow(q).Scan into two strings.
	queryRow func(ctx context.Context, q string) error
	// buffered: the mode never streams (the native Pool's direct path), so a
	// rejected query is always the error and no rows.
	buffered bool
}

// multiSetModes opens the four modes of the report on one connection each: the
// native Pool under the CELERIS_CONFORMANCE_ENGINE of the run and on an event
// loop explicitly (WithEngine on a server that has not started), and
// database/sql the same two ways. One connection each, so that what runs after
// a rejected query runs on the connection that rejected it.
func multiSetModes(t *testing.T) []*multiSetMode {
	t.Helper()
	dsn := dsnFromEnv(t)
	var modes []*multiSetMode

	addPool := func(name string, p *postgres.Pool, buffered bool) {
		t.Cleanup(func() { _ = p.Close() })
		modes = append(modes, &multiSetMode{
			name: name, buffered: buffered,
			query: func(ctx context.Context, q string) multiSetOut {
				var out multiSetOut
				rows, err := p.QueryContext(ctx, q)
				if err != nil {
					out.queryErr = err
					return out
				}
				defer func() { _ = rows.Close() }()
				out.cols = rows.Columns()
				for rows.Next() {
					cur := rows.Columns() // read while the worker may still be parsing
					vals := make([]string, len(cur))
					dest := make([]any, len(cur))
					for i := range dest {
						dest[i] = &vals[i]
					}
					if err := rows.Scan(dest...); err != nil {
						out.rowsErr = fmt.Errorf("scan: %w", err)
						return out
					}
					out.rows = append(out.rows, vals)
				}
				out.rowsErr = rows.Err()
				return out
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
	addSQL := func(name string, db *sql.DB) {
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = db.Close() })
		modes = append(modes, &multiSetMode{
			name: name,
			query: func(ctx context.Context, q string) multiSetOut {
				var out multiSetOut
				rows, err := db.QueryContext(ctx, q)
				if err != nil {
					out.queryErr = err
					return out
				}
				defer func() { _ = rows.Close() }()
				out.cols, _ = rows.Columns()
				for rows.Next() {
					cur, _ := rows.Columns()
					vals := make([]string, len(cur))
					dest := make([]any, len(cur))
					for i := range dest {
						dest[i] = &vals[i]
					}
					if err := rows.Scan(dest...); err != nil {
						out.rowsErr = fmt.Errorf("scan: %w", err)
						return out
					}
					out.rows = append(out.rows, vals)
				}
				out.rowsErr = rows.Err()
				return out
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
	}

	envPool, err := openPoolEngine(dsn, postgres.WithMaxOpen(1))
	if err != nil {
		t.Fatalf("postgres.Open: %v", err)
	}
	// With no engine the Pool is on its direct path, which buffers every reply.
	addPool("pool-env", envPool, os.Getenv("CELERIS_CONFORMANCE_ENGINE") == "" || os.Getenv("CELERIS_CONFORMANCE_ENGINE") == "none")

	loopPool, err := postgres.Open(dsn, postgres.WithMaxOpen(1),
		postgres.WithEngine(celeris.New(celeris.Config{Addr: "127.0.0.1:0", Engine: celeris.Epoll})))
	if err != nil {
		t.Fatalf("postgres.Open (loop): %v", err)
	}
	addPool("pool-loop", loopPool, false)

	envDB, err := openSQLEngine(dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	addSQL("sql-env", envDB)

	conn, err := postgres.NewConnector(dsn)
	if err != nil {
		t.Fatalf("NewConnector: %v", err)
	}
	addSQL("sql-loop", sql.OpenDB(conn.WithEngine(celeris.New(celeris.Config{Addr: "127.0.0.1:0", Engine: celeris.Epoll}))))
	return modes
}

func multiSetCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func multiSetOne(ctx context.Context, t *testing.T, m *multiSetMode, q string) string {
	t.Helper()
	out := m.query(ctx, q)
	if out.queryErr != nil || out.rowsErr != nil || len(out.rows) != 1 || len(out.rows[0]) != 1 {
		t.Fatalf("%s: %q = %+v", m.name, q, out)
	}
	return out.rows[0][0]
}

const multiSetSecond = "SELECT 1::int8 AS zz_second_1, 2::int8 AS zz_second_2, 3::int8 AS zz_second_3"

// TestMultiStatementTwoResultSetsRejected is the probe: a small first set
// (buffered) and one that streams, then a second statement with other columns.
// Each run is either the error with no rows, or the first set (its names, its
// data, no row of the second) and then the error from Rows.Err; never anything
// else, and the next query on the same backend runs clean.
func TestMultiStatementTwoResultSetsRejected(t *testing.T) {
	ctx := multiSetCtx(t)
	for _, m := range multiSetModes(t) {
		t.Run(m.name, func(t *testing.T) {
			pid := multiSetOne(ctx, t, m, "SELECT pg_backend_pid()")
			for _, n := range []int{10, 63, 64, 65, 2000} {
				q := fmt.Sprintf("SELECT g AS first_col_a, 'v'||g AS first_col_b FROM generate_series(1,%d) g; %s", n, multiSetSecond)
				wrong, first := 0, ""
				for i := 0; i < multiSetRounds(); i++ {
					if why := multiSetRejected(m, m.query(ctx, q), n); why != "" {
						if wrong == 0 {
							first = why
						}
						wrong++
					}
				}
				if wrong != 0 {
					t.Errorf("%s N=%d: %d of %d runs wrong; first: %s", m.name, n, wrong, multiSetRounds(), first)
				}
				if got := multiSetOne(ctx, t, m, "SELECT pg_backend_pid()"); got != pid {
					t.Fatalf("%s N=%d: the next query ran on backend %s, not %s", m.name, n, got, pid)
				}
				if got := multiSetOne(ctx, t, m, "SELECT 40+2"); got != "42" {
					t.Fatalf("%s N=%d: the next query returned %q", m.name, n, got)
				}
			}
		})
	}
}

// multiSetRejected returns "" when out is one of the two allowed shapes of a
// rejected query, and what is wrong with it otherwise.
func multiSetRejected(m *multiSetMode, out multiSetOut, n int) string {
	if out.queryErr != nil {
		if !strings.Contains(out.queryErr.Error(), multiSetErr) {
			return fmt.Sprintf("Query error = %v", out.queryErr)
		}
		if len(out.rows) != 0 {
			return "rows with a Query error"
		}
		return ""
	}
	if m.buffered {
		return fmt.Sprintf("a buffered reply returned rows (%d) instead of the error", len(out.rows))
	}
	if strings.Join(out.cols, ",") != "first_col_a,first_col_b" {
		return fmt.Sprintf("Columns = %v, the first set's are first_col_a, first_col_b (%d rows, Err %v)", out.cols, len(out.rows), out.rowsErr)
	}
	if len(out.rows) != n {
		return fmt.Sprintf("%d rows, want the first set's %d (Err %v)", len(out.rows), n, out.rowsErr)
	}
	for i, r := range out.rows {
		if r[0] != strconv.Itoa(i+1) || r[1] != "v"+strconv.Itoa(i+1) {
			return fmt.Sprintf("row %d = %v", i, r)
		}
	}
	if out.rowsErr == nil || !strings.Contains(out.rowsErr.Error(), multiSetErr) {
		return fmt.Sprintf("Rows.Err = %v", out.rowsErr)
	}
	return ""
}

// QueryRow takes the first row: a multi-result-set query is the error there too,
// whatever the size of the first set (a streamed set's first row is scanned
// before the error is known, which is then the error of Close).
func TestMultiStatementQueryRowRejected(t *testing.T) {
	ctx := multiSetCtx(t)
	for _, m := range multiSetModes(t) {
		t.Run(m.name, func(t *testing.T) {
			for _, n := range []int{10, 63, 64, 65, 2000} {
				q := fmt.Sprintf("SELECT g AS first_col_a, 'v'||g AS first_col_b FROM generate_series(1,%d) g; %s", n, multiSetSecond)
				for i := 0; i < multiSetRounds(); i++ {
					err := m.queryRow(ctx, q)
					if err == nil || !strings.Contains(err.Error(), multiSetErr) {
						t.Fatalf("%s N=%d: QueryRow = %v, want %q", m.name, n, err, multiSetErr)
					}
				}
				q = fmt.Sprintf("SET application_name = 'multiset'; SELECT g AS first_col_a, 'v'||g AS first_col_b FROM generate_series(1,%d) g", n)
				if err := m.queryRow(ctx, q); err != nil {
					t.Fatalf("%s N=%d: QueryRow of one result set = %v", m.name, n, err)
				}
			}
			if got := multiSetOne(ctx, t, m, "SELECT 40+2"); got != "42" {
				t.Fatalf("%s: next query = %q", m.name, got)
			}
		})
	}
}

// The same shape in both sets was concatenated silently; it is the same defect.
func TestMultiStatementSameShapeRejected(t *testing.T) {
	ctx := multiSetCtx(t)
	for _, m := range multiSetModes(t) {
		t.Run(m.name, func(t *testing.T) {
			for i := 0; i < multiSetRounds(); i++ {
				out := m.query(ctx, "SELECT 1 AS x; SELECT 2 AS x")
				if out.queryErr == nil && (len(out.rows) != 1 || out.rowsErr == nil) {
					t.Fatalf("%s: SELECT 1 AS x; SELECT 2 AS x = %+v", m.name, out)
				}
				if out.queryErr != nil && !strings.Contains(out.queryErr.Error(), multiSetErr) {
					t.Fatalf("%s: %v", m.name, out.queryErr)
				}
			}
			if got := multiSetOne(ctx, t, m, "SELECT 7"); got != "7" {
				t.Fatalf("%s: next query = %q", m.name, got)
			}
		})
	}
}

// The error says the server executed the statements, and it did: the effects of
// a statement of the string are there after the error.
func TestMultiStatementRejectedErrorIsTruthful(t *testing.T) {
	ctx := multiSetCtx(t)
	for _, m := range multiSetModes(t) {
		t.Run(m.name, func(t *testing.T) {
			tbl := uniqueTableName(t, "multiset")
			if _, err := m.exec(ctx, "CREATE TABLE "+tbl+" (v int)"); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = m.exec(context.Background(), "DROP TABLE IF EXISTS "+tbl) })
			out := m.query(ctx, "INSERT INTO "+tbl+" VALUES (1); SELECT 1 AS a; SELECT 2 AS b")
			if err := firstErr(out); err == nil || !strings.Contains(err.Error(), multiSetErr) {
				t.Fatalf("%s: %+v", m.name, out)
			}
			if got := multiSetOne(ctx, t, m, "SELECT count(*) FROM "+tbl); got != "1" {
				t.Fatalf("%s: count = %s, the INSERT of the rejected query ran once", m.name, got)
			}
		})
	}
}

func firstErr(o multiSetOut) error {
	if o.queryErr != nil {
		return o.queryErr
	}
	return o.rowsErr
}

// A statement that fails after the first two result sets is the server's error.
func TestMultiStatementLaterServerError(t *testing.T) {
	ctx := multiSetCtx(t)
	for _, m := range multiSetModes(t) {
		t.Run(m.name, func(t *testing.T) {
			out := m.query(ctx, "SELECT 1 AS a; SELECT 2 AS b; SELECT 1/0 AS c")
			if err := firstErr(out); err == nil || !strings.Contains(err.Error(), "division by zero") {
				t.Fatalf("%s: %+v", m.name, out)
			}
			if got := multiSetOne(ctx, t, m, "SELECT 9"); got != "9" {
				t.Fatalf("%s: next query = %q", m.name, got)
			}
		})
	}
}

// One row-returning statement among others is not rejected: each shape returns
// that one result set, under its own columns.
func TestMultiStatementOneResultSetNotRejected(t *testing.T) {
	ctx := multiSetCtx(t)
	for _, m := range multiSetModes(t) {
		t.Run(m.name, func(t *testing.T) {
			tbl := uniqueTableName(t, "multiset_one")
			if _, err := m.exec(ctx, "CREATE TABLE "+tbl+" (v int)"); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = m.exec(context.Background(), "DROP TABLE IF EXISTS "+tbl) })
			for _, tc := range []struct {
				name, q string
				want    []string
			}{
				{"SET;SELECT", "SET application_name = 'multiset'; SELECT current_setting('application_name') AS k", []string{"multiset"}},
				{"SELECT;UPDATE", "SELECT 42 AS k; UPDATE " + tbl + " SET v = v", []string{"42"}},
				{"UPDATE;SELECT", "UPDATE " + tbl + " SET v = v; SELECT 43 AS k", []string{"43"}},
				{"INSERT;SELECT", "INSERT INTO " + tbl + " VALUES (1); SELECT count(*) AS k FROM " + tbl, nil},
				{"SELECT;INSERT", "SELECT 44 AS k; INSERT INTO " + tbl + " VALUES (2)", []string{"44"}},
				{"BEGIN;SELECT;COMMIT", "BEGIN; SELECT 45 AS k; COMMIT", []string{"45"}},
			} {
				out := m.query(ctx, tc.q)
				if out.queryErr != nil || out.rowsErr != nil || len(out.rows) != 1 || len(out.cols) != 1 || out.cols[0] != "k" {
					t.Fatalf("%s %s: %+v", m.name, tc.name, out)
				}
				if tc.want != nil && out.rows[0][0] != tc.want[0] {
					t.Fatalf("%s %s: row = %v, want %v", m.name, tc.name, out.rows[0], tc.want)
				}
			}
			// A larger first set that streams, with a statement after it.
			out := m.query(ctx, "SELECT g AS k FROM generate_series(1,2000) g; UPDATE "+tbl+" SET v = v")
			if out.queryErr != nil || out.rowsErr != nil || len(out.rows) != 2000 {
				t.Fatalf("%s streaming SELECT;UPDATE: %d rows, %v, %v", m.name, len(out.rows), out.queryErr, out.rowsErr)
			}
		})
	}
}

// Exec keeps accepting several statements, SELECTs among them (a migration's
// SELECT setval(...); SELECT setval(...)), with the count Exec gave before: the
// command tag of the last statement.
func TestMultiStatementExecUnchanged(t *testing.T) {
	ctx := multiSetCtx(t)
	for _, m := range multiSetModes(t) {
		t.Run(m.name, func(t *testing.T) {
			seq := uniqueTableName(t, "multiset_seq")
			if _, err := m.exec(ctx, "CREATE SEQUENCE "+seq); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = m.exec(context.Background(), "DROP SEQUENCE IF EXISTS "+seq) })
			n, err := m.exec(ctx, fmt.Sprintf("SELECT setval('%s', 5); SELECT setval('%s', 9)", seq, seq))
			if err != nil {
				t.Fatalf("%s: Exec: %v", m.name, err)
			}
			t.Logf("%s: RowsAffected of SELECT setval; SELECT setval = %d", m.name, n)
			if n != 1 {
				t.Errorf("%s: RowsAffected = %d, want 1 (the tag of the last SELECT, as in v1.5.11)", m.name, n)
			}
			n, err = m.exec(ctx, "SELECT 1 AS a; SELECT generate_series(1,5) AS b; CREATE TEMP TABLE multiset_exec (v int)")
			if err != nil {
				t.Fatalf("%s: Exec: %v", m.name, err)
			}
			t.Logf("%s: RowsAffected of SELECT; SELECT; CREATE = %d", m.name, n)
			if got := multiSetOne(ctx, t, m, "SELECT 5"); got != "5" {
				t.Fatalf("%s: next query = %q", m.name, got)
			}
		})
	}
}
