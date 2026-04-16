package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	_ "github.com/lib/pq"
)

const libpqDriverName = "postgres"

func openLibpq(b *testing.B) *sql.DB {
	b.Helper()
	db, err := sql.Open(libpqDriverName, dsn(b))
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(64)
	db.SetMaxIdleConns(32)
	b.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(context.Background()); err != nil {
		b.Skipf("ping: %v", err)
	}
	return db
}

func BenchmarkSelectOne_LibPQ(b *testing.B) {
	db := openLibpq(b)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		var n int
		_ = db.QueryRowContext(ctx, "SELECT 1").Scan(&n)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&n); err != nil {
			b.Fatalf("scan: %v", err)
		}
	}
}

func BenchmarkSelect1000Rows_Text_LibPQ(b *testing.B) {
	ensureTable(b)
	db := openLibpq(b)
	ctx := context.Background()
	query := fmt.Sprintf("SELECT id, label FROM %s", benchTableName)
	for i := 0; i < 2; i++ {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			b.Fatalf("warmup: %v", err)
		}
		drainPQ(b, rows)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			b.Fatalf("query: %v", err)
		}
		drainPQ(b, rows)
	}
}

func BenchmarkSelect1000Rows_Binary_LibPQ(b *testing.B) {
	// lib/pq always uses text format over database/sql; this bench is
	// identical to the Text variant and exists only so the benchmark name
	// matrix is symmetric with celeris/pgx.
	ensureTable(b)
	db := openLibpq(b)
	ctx := context.Background()
	query := fmt.Sprintf("SELECT id, label FROM %s WHERE id <= $1", benchTableName)
	for i := 0; i < 2; i++ {
		rows, err := db.QueryContext(ctx, query, benchRowCount)
		if err != nil {
			b.Fatalf("warmup: %v", err)
		}
		drainPQ(b, rows)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := db.QueryContext(ctx, query, benchRowCount)
		if err != nil {
			b.Fatalf("query: %v", err)
		}
		drainPQ(b, rows)
	}
}

func BenchmarkInsertPrepared_LibPQ(b *testing.B) {
	db := openLibpq(b)
	ctx := context.Background()
	tbl := "libpq_bench_insert"
	if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS "+tbl); err != nil {
		b.Fatalf("drop: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE "+tbl+" (id int primary key, label text, n int)"); err != nil {
		b.Fatalf("create: %v", err)
	}
	b.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), "DROP TABLE "+tbl)
	})
	stmt, err := db.PrepareContext(ctx, "INSERT INTO "+tbl+" (id,label,n) VALUES ($1,$2,$3)")
	if err != nil {
		b.Fatalf("prepare: %v", err)
	}
	defer stmt.Close()
	for i := 0; i < 5; i++ {
		if _, err := stmt.ExecContext(ctx, i, "warmup", i); err != nil {
			b.Fatalf("warmup: %v", err)
		}
	}
	if _, err := db.ExecContext(ctx, "TRUNCATE "+tbl); err != nil {
		b.Fatalf("truncate: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := stmt.ExecContext(ctx, i, "row", i*2); err != nil {
			b.Fatalf("exec: %v", err)
		}
	}
}

func BenchmarkTransactionRoundtrip_LibPQ(b *testing.B) {
	db := openLibpq(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			b.Fatalf("begin: %v", err)
		}
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT 1").Scan(&n); err != nil {
			b.Fatalf("scan: %v", err)
		}
		if err := tx.Commit(); err != nil {
			b.Fatalf("commit: %v", err)
		}
	}
}

func BenchmarkParallel_QueryContext_LibPQ(b *testing.B) {
	db := openLibpq(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			var n int
			if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&n); err != nil {
				b.Fatalf("scan: %v", err)
			}
		}
	})
}

func BenchmarkPoolContention_LibPQ(b *testing.B) {
	BenchmarkParallel_QueryContext_LibPQ(b)
}

// BenchmarkCopyIn_1M_Rows_LibPQ skipped: lib/pq's CopyIn API exists but is
// meaningfully different from pgx/celeris (requires Stmt.Exec in a loop), so
// a direct comparison is not apples-to-apples.
func BenchmarkCopyIn_1M_Rows_LibPQ(b *testing.B) {
	b.Skip("lib/pq COPY API shape differs from pgx/celeris; see README")
}

func drainPQ(b *testing.B, rows *sql.Rows) {
	b.Helper()
	defer rows.Close()
	for rows.Next() {
		var id int
		var label string
		if err := rows.Scan(&id, &label); err != nil {
			b.Fatalf("scan: %v", err)
		}
	}
	if err := rows.Err(); err != nil {
		b.Fatalf("err: %v", err)
	}
}
