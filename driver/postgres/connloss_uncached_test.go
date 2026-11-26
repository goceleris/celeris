package postgres

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/goceleris/celeris/driver/internal/looptest"
)

// An extended statement that is not cached, cut after its DataRows, ends with the
// loss error and no wrong value. Such a statement (an INSERT/UPDATE/DELETE ...
// RETURNING with arguments, which are not auto-cached; any query with
// auto_cache_statements=false) describes its portal in the same exchange, and the
// Describe's RowDescription reached the request only when the statement ended: a
// request that failAll or driveDirect completed after its DataRows had rows and
// no columns. database/sql then saw Columns() == [] and a Scan error ("expected 0
// destination arguments in Scan, not 1") with rows.Err() == nil, the native Pool
// handed the raw binary bytes of the cells to Scan as values with a nil error, and
// QueryRow returned the scan error, not the loss error.

type uncachedStmt struct {
	name  string
	query string
	dsn   string // extra DSN parameters
}

var uncachedStmts = []uncachedStmt{
	{"update-returning", "UPDATE t SET x = 1 WHERE x > $1 RETURNING g", ""},
	{"insert-returning", "INSERT INTO t(x) VALUES($1) RETURNING g", ""},
	{"delete-returning", "DELETE FROM t WHERE x > $1 RETURNING g", ""},
	{"select-no-autocache", "SELECT g FROM t WHERE x > $1", "&auto_cache_statements=false"},
}

type uncachedEnd struct {
	kind   lossKind // where the fake backend cuts
	expect lossKind // what checkLoss expects of the error
}

func uncachedEnds() []uncachedEnd {
	return []uncachedEnd{
		{lossAfterLast, lossAfterLast},
		{lossMid, lossMid},
		{lossAfterC, lossAfterC},
		{lossInsideCC, lossAfterLast},
		{lossReadyMinus1, lossAfterC},
		{lossReadyHeader, lossAfterC},
		{lossFatal, lossFatal},
	}
}

// uncachedCarrier runs the statement with one int64 argument and reads its rows
// (marker column g = 1, 2, 3 ...), or QueryRow's first.
type uncachedCarrier struct {
	name     string
	directOK bool
	run      func(t *testing.T, addr string, direct bool, st uncachedStmt) (rows []int64, cols []string, err error)
}

func uncachedDSN(addr string, st uncachedStmt) string { return lossDSN(addr) + st.dsn }

func uncachedCarriers() []uncachedCarrier {
	openSQL := func(t *testing.T, addr string, st uncachedStmt) *sql.DB {
		db, err := sql.Open(DriverName, uncachedDSN(addr, st))
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	openPool := func(t *testing.T, addr string, direct bool, st uncachedStmt) *Pool {
		opts := []Option{WithMaxOpen(1)}
		if !direct {
			opts = append(opts, WithEngine(looptest.Unstarted()))
		}
		p, err := Open(uncachedDSN(addr, st), opts...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = p.Close() })
		return p
	}
	readSQL := func(rows *sql.Rows) ([]int64, []string, error) {
		cols, _ := rows.Columns()
		var got []int64
		for rows.Next() {
			var g int64
			if err := rows.Scan(&g); err != nil {
				_ = rows.Close()
				return got, cols, err
			}
			got = append(got, g)
		}
		err := rows.Err()
		if cerr := rows.Close(); err == nil {
			err = cerr
		}
		return got, cols, err
	}
	readPool := func(rows *Rows) ([]int64, []string, error) {
		cols := rows.Columns()
		var got []int64
		for rows.Next() {
			var g int64
			if err := rows.Scan(&g); err != nil {
				_ = rows.Close()
				return got, cols, err
			}
			got = append(got, g)
		}
		err := rows.Err()
		if cerr := rows.Close(); err == nil {
			err = cerr
		}
		return got, cols, err
	}
	return []uncachedCarrier{
		{"sqlDB-query", false, func(t *testing.T, addr string, _ bool, st uncachedStmt) ([]int64, []string, error) {
			rows, err := openSQL(t, addr, st).QueryContext(tctx(t), st.query, int64(0))
			if err != nil {
				return nil, nil, err
			}
			return readSQL(rows)
		}},
		{"sqlDB-queryRow", false, func(t *testing.T, addr string, _ bool, st uncachedStmt) ([]int64, []string, error) {
			var g int64
			err := openSQL(t, addr, st).QueryRowContext(tctx(t), st.query, int64(0)).Scan(&g)
			if err != nil {
				return nil, nil, err
			}
			return []int64{g}, nil, nil
		}},
		{"sqlTx-query", false, func(t *testing.T, addr string, _ bool, st uncachedStmt) ([]int64, []string, error) {
			ctx := tctx(t)
			tx, err := openSQL(t, addr, st).BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			rows, err := tx.QueryContext(ctx, st.query, int64(0))
			if err != nil {
				return nil, nil, err
			}
			return readSQL(rows)
		}},
		{"poolQuery", true, func(t *testing.T, addr string, direct bool, st uncachedStmt) ([]int64, []string, error) {
			rows, err := openPool(t, addr, direct, st).QueryContext(tctx(t), st.query, int64(0))
			if err != nil {
				return nil, nil, err
			}
			return readPool(rows)
		}},
		{"poolQueryRow", true, func(t *testing.T, addr string, direct bool, st uncachedStmt) ([]int64, []string, error) {
			var g int64
			err := openPool(t, addr, direct, st).QueryRow(tctx(t), st.query, int64(0)).Scan(&g)
			if err != nil {
				return nil, nil, err
			}
			return []int64{g}, nil, nil
		}},
		{"poolTx-query", true, func(t *testing.T, addr string, direct bool, st uncachedStmt) ([]int64, []string, error) {
			ctx := tctx(t)
			tx, err := openPool(t, addr, direct, st).BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			rows, err := tx.QueryContext(ctx, st.query, int64(0))
			if err != nil {
				return nil, nil, err
			}
			return readPool(rows)
		}},
	}
}

func TestConnLossUncachedExtendedCut(t *testing.T) {
	const n = 6
	for _, car := range uncachedCarriers() {
		for _, engine := range []string{"loop", "direct"} {
			if engine == "direct" && !car.directOK {
				continue
			}
			for _, st := range uncachedStmts {
				for _, end := range uncachedEnds() {
					for _, rst := range []bool{false, true} {
						cut := "FIN"
						if rst {
							cut = "RST"
						}
						name := fmt.Sprintf("%s/%s/%s/%s/%s", car.name, engine, st.name, end.kind, cut)
						t.Run(name, func(t *testing.T) {
							t.Parallel()
							addr := startFakePG(t, lossServer(t, lossCfg{kind: end.kind, n: n, rst: rst}))
							rows, cols, err := car.run(t, addr, engine == "direct", st)
							// Never a wrong value: every row that came is the marker of its place.
							for i, g := range rows {
								if g != int64(i+1) {
									t.Fatalf("row %d carries %d: a wrong value reached the caller (err %v)", i+1, g, err)
								}
							}
							if cols != nil && len(rows) > 0 && (len(cols) != 1 || cols[0] != "g") {
								t.Errorf("Columns() = %v, want [g]", cols)
							}
							cfg := lossCfg{kind: end.expect, n: n, rst: rst}
							if err != nil {
								// A scan error is never the answer: the loss error is.
								for _, bad := range []string{"destination arguments", "cannot convert", "unsupported Scan"} {
									if strings.Contains(err.Error(), bad) {
										t.Fatalf("a scan error instead of the loss error: %v", err)
									}
								}
							}
							checkLoss(t, cfg, len(rows), err)
						})
					}
				}
			}
		}
	}
}
