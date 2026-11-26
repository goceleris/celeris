package postgres

import (
	"fmt"
	"testing"
)

// Rows.Close returns the error the result ended with. A caller that stops after
// the rows it wants never sees Next's error, so Close is where a result cut off by
// the connection shows: database/sql's Row.Scan returns rows.Close() after it
// scanned the row. Before, streamRows.Close and pgRows.Close both returned nil.

// firstRowThenClose reads one row and closes, as Row.Scan does, and returns the
// error of Close, or of the call that opened the rows.
type closeCarrier struct {
	name     string
	directOK bool
	run      func(t *testing.T, addr string, direct, ext bool) error
}

func closeCarriers() []closeCarrier {
	return []closeCarrier{
		{"sqlDB", false, func(t *testing.T, addr string, _, ext bool) error {
			db := lossSQL(t, addr)
			q, args := lossQuery(false, ext)
			rows, err := db.QueryContext(tctx(t), q, args...)
			if err != nil {
				return err
			}
			rows.Next()
			return rows.Close()
		}},
		{"sqlTx", false, func(t *testing.T, addr string, _, ext bool) error {
			db := lossSQL(t, addr)
			q, args := lossQuery(false, ext)
			ctx := tctx(t)
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			rows, err := tx.QueryContext(ctx, q, args...)
			if err != nil {
				return err
			}
			rows.Next()
			return rows.Close()
		}},
		{"poolQuery", true, func(t *testing.T, addr string, direct, ext bool) error {
			p := lossPool(t, addr, direct)
			q, args := lossQuery(false, ext)
			rows, err := p.QueryContext(tctx(t), q, args...)
			if err != nil {
				return err
			}
			rows.Next()
			return rows.Close()
		}},
		{"poolTx", true, func(t *testing.T, addr string, direct, ext bool) error {
			p := lossPool(t, addr, direct)
			q, args := lossQuery(false, ext)
			ctx := tctx(t)
			tx, err := p.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			rows, err := tx.QueryContext(ctx, q, args...)
			if err != nil {
				return err
			}
			rows.Next()
			return rows.Close()
		}},
	}
}

func TestConnLossCloseReturnsError(t *testing.T) {
	for _, carrier := range closeCarriers() {
		for _, engine := range []string{"loop", "direct"} {
			if engine == "direct" && !carrier.directOK {
				continue
			}
			for _, ext := range []bool{false, true} {
				for _, n := range []int{50, 500} {
					for _, kind := range allLossKinds {
						// Before the first row there is no row to stop after: Query itself fails.
						cfg := lossCfg{kind: kind, n: n}
						name := fmt.Sprintf("%s/%s/ext=%v/rows%d/%s", carrier.name, engine, ext, n, kind)
						t.Run(name, func(t *testing.T) {
							t.Parallel()
							addr := startFakePG(t, lossServer(t, cfg))
							err := carrier.run(t, addr, engine == "direct", ext)
							if kind == lossComplete {
								if err != nil {
									t.Errorf("Close after a complete result returned %v", err)
								}
								return
							}
							if err == nil {
								t.Fatalf("Close returned nil after the result was cut off (%s)", kind)
							}
							checkLoss(t, cfg, 0, err)
						})
					}
				}
			}
		}
	}
}

// queryRowCarrier runs QueryRow(...).Scan on one carrier and returns Scan's error
// and the value it scanned.
type queryRowCarrier struct {
	name     string
	directOK bool
	run      func(t *testing.T, addr string, direct, ext bool) (int64, error)
}

func queryRowCarriers() []queryRowCarrier {
	return []queryRowCarrier{
		{"poolQueryRow", true, func(t *testing.T, addr string, direct, ext bool) (int64, error) {
			p := lossPool(t, addr, direct)
			q, args := lossQuery(false, ext)
			var v int64
			err := p.QueryRow(tctx(t), q, args...).Scan(&v)
			return v, err
		}},
		{"poolTxQueryRow", true, func(t *testing.T, addr string, direct, ext bool) (int64, error) {
			p := lossPool(t, addr, direct)
			q, args := lossQuery(false, ext)
			ctx := tctx(t)
			tx, err := p.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			var v int64
			err = tx.QueryRow(ctx, q, args...).Scan(&v)
			return v, err
		}},
		{"sqlDB", false, func(t *testing.T, addr string, _, ext bool) (int64, error) {
			db := lossSQL(t, addr)
			q, args := lossQuery(false, ext)
			var v int64
			err := db.QueryRowContext(tctx(t), q, args...).Scan(&v)
			return v, err
		}},
		{"sqlConn", false, func(t *testing.T, addr string, _, ext bool) (int64, error) {
			db := lossSQL(t, addr)
			q, args := lossQuery(false, ext)
			ctx := tctx(t)
			conn, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			var v int64
			err = conn.QueryRowContext(ctx, q, args...).Scan(&v)
			return v, err
		}},
		{"sqlTx", false, func(t *testing.T, addr string, _, ext bool) (int64, error) {
			db := lossSQL(t, addr)
			q, args := lossQuery(false, ext)
			ctx := tctx(t)
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			var v int64
			err = tx.QueryRowContext(ctx, q, args...).Scan(&v)
			return v, err
		}},
	}
}

// TestConnLossQueryRowIsNotSuccess: QueryRow returns the row only when the result
// completed. A connection that died after the row and before the
// ReadyForQuery means the statement (an INSERT ... RETURNING in autocommit
// commits at Sync, after the DataRow) may not have run: Scan returns an error, not
// the row, and never sql.ErrNoRows.
func TestConnLossQueryRowIsNotSuccess(t *testing.T) {
	for _, carrier := range queryRowCarriers() {
		for _, engine := range []string{"loop", "direct"} {
			if engine == "direct" && !carrier.directOK {
				continue
			}
			for _, ext := range []bool{false, true} {
				for _, n := range []int{1, 50, 500} {
					for _, kind := range allLossKinds {
						cfg := lossCfg{kind: kind, n: n}
						name := fmt.Sprintf("%s/%s/ext=%v/rows%d/%s", carrier.name, engine, ext, n, kind)
						t.Run(name, func(t *testing.T) {
							t.Parallel()
							addr := startFakePG(t, lossServer(t, cfg))
							v, err := carrier.run(t, addr, engine == "direct", ext)
							if kind == lossComplete {
								if err != nil || v != 1 {
									t.Errorf("Scan after a complete result: v=%d err=%v", v, err)
								}
								return
							}
							if err == nil {
								t.Fatalf("Scan returned the row (v=%d) with a nil error, but the statement was cut off (%s)", v, kind)
							}
							checkLoss(t, cfg, 0, err)
						})
					}
				}
			}
		}
	}
}
