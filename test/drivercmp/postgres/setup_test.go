// Package postgres_test benchmarks celeris's PostgreSQL driver against pgx
// and lib/pq. It lives in a dedicated module so it does not force the
// comparison drivers on consumers of celeris.
//
// All benchmarks skip if CELERIS_PG_DSN is unset.
package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const envDSN = "CELERIS_PG_DSN"

// benchTable is the shared schema every benchmark file uses. It is created
// lazily once per process under a sync.Once so that parallel Go test invocations
// on the same pg server (e.g. CI matrix) don't step on each other.
const (
	benchTableName = "celeris_bench_rows"
	benchRowCount  = 1000
)

var (
	setupOnce sync.Once
	setupErr  error
)

// dsn returns the benchmark DSN or skips.
func dsn(b *testing.B) string {
	b.Helper()
	s := strings.TrimSpace(os.Getenv(envDSN))
	if s == "" {
		b.Skipf("skipping: %s not set", envDSN)
	}
	return s
}

// ensureTable idempotently creates benchTableName populated with benchRowCount
// rows. It uses stdlib sql + whatever driver is registered as "postgres" or
// "celeris-postgres" (falls back to pg_isready through lib/pq's driver
// registration in libpq_test.go).
//
// Callers that don't import lib/pq (only celeris + pgx) should use their own
// driver — this helper is a convenience for setup_test itself, which prefers
// celeris-postgres since it's always available in this module.
func ensureTable(b *testing.B) {
	b.Helper()
	setupOnce.Do(func() {
		db, err := sql.Open("celeris-postgres", os.Getenv(envDSN))
		if err != nil {
			setupErr = fmt.Errorf("open: %w", err)
			return
		}
		defer db.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Drop + recreate so each run starts clean.
		if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS "+benchTableName); err != nil {
			setupErr = fmt.Errorf("drop: %w", err)
			return
		}
		_, err = db.ExecContext(ctx, fmt.Sprintf(`CREATE TABLE %s (
			id int primary key,
			label text,
			created_at timestamptz default now()
		)`, benchTableName))
		if err != nil {
			setupErr = fmt.Errorf("create: %w", err)
			return
		}
		_, err = db.ExecContext(ctx, fmt.Sprintf(
			`INSERT INTO %s (id, label) SELECT i, 'row_' || i FROM generate_series(1, %d) AS i`,
			benchTableName, benchRowCount,
		))
		if err != nil {
			setupErr = fmt.Errorf("populate: %w", err)
			return
		}
	})
	if setupErr != nil {
		b.Fatalf("setup: %v", setupErr)
	}
}
