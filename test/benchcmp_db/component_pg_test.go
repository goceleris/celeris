package benchcmp_db

// Component-level head-to-head: Postgres client SELECT throughput.
// One row, prepared-statement cached on both drivers. Framework
// overhead factored out.

import (
	"context"
	"testing"

	celpostgres "github.com/goceleris/celeris/driver/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

func BenchmarkPGClient_Celeris(b *testing.B) {
	dsn := skipIfNoPG(b)
	pool, err := celpostgres.Open(dsn)
	if err != nil {
		b.Fatalf("celeris pg: %v", err)
	}
	defer func() { _ = pool.Close() }()

	ctx := context.Background()
	// Warm prepared statement cache on one conn.
	for i := 0; i < 10; i++ {
		rows, _ := pool.QueryContext(ctx, "SELECT id, name FROM bench_users WHERE id=$1", 1)
		for rows.Next() {
			var id int64
			var name string
			_ = rows.Scan(&id, &name)
		}
		_ = rows.Close()
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		rows, err := pool.QueryContext(ctx, "SELECT id, name FROM bench_users WHERE id=$1", 1)
		if err != nil {
			b.Fatalf("Query: %v", err)
		}
		var id int64
		var name string
		if rows.Next() {
			_ = rows.Scan(&id, &name)
		}
		_ = rows.Close()
	}
}

func BenchmarkPGClient_Pgx(b *testing.B) {
	dsn := skipIfNoPG(b)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		b.Fatalf("pgx: %v", err)
	}
	defer pool.Close()

	// Warm statement cache.
	for i := 0; i < 10; i++ {
		var id int64
		var name string
		_ = pool.QueryRow(ctx, "SELECT id, name FROM bench_users WHERE id=$1", 1).Scan(&id, &name)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		var id int64
		var name string
		if err := pool.QueryRow(ctx, "SELECT id, name FROM bench_users WHERE id=$1", 1).Scan(&id, &name); err != nil {
			b.Fatalf("QueryRow: %v", err)
		}
	}
}
