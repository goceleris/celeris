//go:build postgres

package postgres_test

// Eviction inside a transaction (found by the review of the first version of the
// fix, which sent an SQL DEALLOCATE: a statement the server no longer has made
// it fail, and that aborted the transaction). The drop of an evicted statement
// must never fail the caller's transaction.

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/driver/postgres"
)

// evictTx abstracts a transaction on the native Pool and on database/sql.
type evictTx interface {
	q(ctx context.Context, q string, args ...any) (int64, error)
	exec(ctx context.Context, q string) error
	commit() error
	rollback() error
}

type evictPoolTx struct{ tx *postgres.Tx }

func (p evictPoolTx) q(ctx context.Context, q string, args ...any) (int64, error) {
	var v int64
	err := p.tx.QueryRow(ctx, q, args...).Scan(&v)
	return v, err
}
func (p evictPoolTx) exec(ctx context.Context, q string) error {
	_, err := p.tx.ExecContext(ctx, q)
	return err
}
func (p evictPoolTx) commit() error   { return p.tx.Commit() }
func (p evictPoolTx) rollback() error { return p.tx.Rollback() }

type evictSQLTx struct{ tx *sql.Tx }

func (s evictSQLTx) q(ctx context.Context, q string, args ...any) (int64, error) {
	var v int64
	err := s.tx.QueryRowContext(ctx, q, args...).Scan(&v)
	return v, err
}
func (s evictSQLTx) exec(ctx context.Context, q string) error {
	_, err := s.tx.ExecContext(ctx, q)
	return err
}
func (s evictSQLTx) commit() error   { return s.tx.Commit() }
func (s evictSQLTx) rollback() error { return s.tx.Rollback() }

type evictTxMode struct {
	name  string
	begin func(t *testing.T, dsn string) (evictTx, func())
}

func evictTxPool(loop bool) func(t *testing.T, dsn string) (evictTx, func()) {
	return func(t *testing.T, dsn string) (evictTx, func()) {
		opts := []postgres.Option{postgres.WithMaxOpen(1)}
		if loop {
			opts = append(opts, postgres.WithEngine(celeris.New(celeris.Config{Addr: "127.0.0.1:0"})))
		}
		p, err := postgres.Open(dsn, opts...)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := p.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		return evictPoolTx{tx}, func() { _ = p.Close() }
	}
}

func evictTxSQL(t *testing.T, dsn string) (evictTx, func()) {
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return evictSQLTx{tx}, func() { _ = db.Close() }
}

var evictTxModes = []evictTxMode{
	{"pool-direct", evictTxPool(false)},
	{"pool-loop", evictTxPool(true)},
	{"database-sql", evictTxSQL},
}

// A cached statement whose server-side statement is gone (DEALLOCATE ALL /
// DISCARD ALL run by the application, or PgBouncer >= 1.21 renaming
// protocol-level statements) is evicted inside an open transaction.
func TestStatementCacheEvictStaleInTransaction(t *testing.T) {
	dsn := withParams(dsnFromEnv(t), "statement_cache_size=2")
	for _, m := range evictTxModes {
		t.Run(m.name, func(t *testing.T) {
			tx, closeFn := m.begin(t, dsn)
			defer closeFn()
			ctx := context.Background()
			runWithDeadline(t, "stale in tx", func() error {
				for i := 0; i < 2; i++ {
					if _, err := tx.q(ctx, fmt.Sprintf("SELECT $1::int8 + %d AS r%d", i, i), 1); err != nil {
						return fmt.Errorf("fill %d: %w", i, err)
					}
				}
				if err := tx.exec(ctx, "DEALLOCATE ALL"); err != nil {
					return fmt.Errorf("deallocate all: %w", err)
				}
				// Evicts r0, whose server statement is gone.
				v, err := tx.q(ctx, "SELECT $1::int8 + 2 AS r2", 1)
				if err != nil {
					return fmt.Errorf("evicting query inside the tx failed: %w", err)
				}
				if v != 3 {
					return fmt.Errorf("got %d", v)
				}
				v, err = tx.q(ctx, "SELECT $1::int8 + 7 AS after", 1)
				if err != nil {
					return fmt.Errorf("query after eviction inside the tx failed: %w", err)
				}
				_ = v
				return tx.commit()
			})
		})
	}
}

// Eviction attempted inside an aborted transaction, then ROLLBACK: the conn must
// be consistent afterwards (each later query gets its own reply).
func TestStatementCacheEvictInAbortedTransaction(t *testing.T) {
	dsn := withParams(dsnFromEnv(t), "statement_cache_size=2")
	for _, m := range evictTxModes {
		t.Run(m.name, func(t *testing.T) {
			tx, closeFn := m.begin(t, dsn)
			defer closeFn()
			ctx := context.Background()
			runWithDeadline(t, "aborted tx", func() error {
				for i := 0; i < 2; i++ {
					if _, err := tx.q(ctx, fmt.Sprintf("SELECT $1::int8 + %d AS a%d", i, i), 1); err != nil {
						return fmt.Errorf("fill %d: %w", i, err)
					}
				}
				if _, err := tx.q(ctx, "SELECT 1/($1::int8 - 1) AS boom", 1); err == nil {
					return fmt.Errorf("want 22012")
				}
				_, err := tx.q(ctx, "SELECT $1::int8 + 2 AS a2", 1)
				if pgCode(err, "25P02") != nil {
					return fmt.Errorf("evicting query in aborted tx: %v (want 25P02)", err)
				}
				if err := tx.rollback(); err != nil {
					return fmt.Errorf("rollback: %w", err)
				}
				return nil
			})
		})
	}
}

// Eviction in an open transaction that later rolls back: the conn keeps working
// and every later statement (evicting or not) gets its own value.
func TestStatementCacheEvictThenRollbackThenReuse(t *testing.T) {
	dsn := withParams(dsnFromEnv(t), "statement_cache_size=2")
	db := openStressPool(false)(t, dsn, 1)
	defer db.Close()
	p := db.(stressPool).p
	ctx := context.Background()
	runWithDeadline(t, "rollback reuse", func() error {
		tx, err := p.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for i := 0; i < 6; i++ {
			var v int64
			if err := tx.QueryRow(ctx, fmt.Sprintf("SELECT $1::int8 + %d AS b%d", i, i), 1).Scan(&v); err != nil {
				return fmt.Errorf("in tx %d: %w", i, err)
			}
		}
		if err := tx.Rollback(); err != nil {
			return err
		}
		for i := 0; i < 12; i++ {
			var v int64
			if err := p.QueryRow(ctx, "SELECT $1::int8 + "+strconv.Itoa(i%7)+" AS b"+strconv.Itoa(i%7), 1).Scan(&v); err != nil {
				return fmt.Errorf("after rollback %d: %w", i, err)
			}
			if v != int64(1+i%7) {
				return fmt.Errorf("after rollback %d: got %d", i, v)
			}
		}
		return nil
	})
}
