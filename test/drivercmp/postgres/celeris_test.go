package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/goceleris/celeris/driver/postgres"
)

const celerisDriverName = "celeris-postgres"

// openCeleris opens a *sql.DB backed by the celeris driver and sizes the pool
// to match the benchmark's parallelism.
func openCeleris(b *testing.B) *sql.DB {
	b.Helper()
	db, err := sql.Open(celerisDriverName, dsn(b))
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

// openCelerisPool returns a *postgres.Pool that bypasses database/sql entirely.
func openCelerisPool(b *testing.B) *postgres.Pool {
	b.Helper()
	pool, err := postgres.Open(dsn(b), postgres.WithMaxOpen(64))
	if err != nil {
		b.Fatalf("postgres.Open: %v", err)
	}
	b.Cleanup(func() { _ = pool.Close() })
	if err := pool.Ping(context.Background()); err != nil {
		b.Skipf("ping: %v", err)
	}
	return pool
}

func BenchmarkSelectOne_Celeris(b *testing.B) {
	db := openCeleris(b)
	ctx := context.Background()
	// Warmup.
	for i := 0; i < 5; i++ {
		var n int
		_ = db.QueryRowContext(ctx, "SELECT 1").Scan(&n)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&n); err != nil {
			b.Fatalf("Scan: %v", err)
		}
	}
}

func BenchmarkSelect1000Rows_Text_Celeris(b *testing.B) {
	ensureTable(b)
	db := openCeleris(b)
	ctx := context.Background()
	query := fmt.Sprintf("SELECT id, label FROM %s", benchTableName)
	// Warmup.
	for i := 0; i < 2; i++ {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			b.Fatalf("warmup: %v", err)
		}
		drain(b, rows)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			b.Fatalf("Query: %v", err)
		}
		drain(b, rows)
	}
}

// BenchmarkSelect1000Rows_Binary_Celeris is the same shape as the Text
// variant — the celeris driver auto-requests binary when args are present.
// The differentiating argumented query forces the binary format path.
func BenchmarkSelect1000Rows_Binary_Celeris(b *testing.B) {
	ensureTable(b)
	db := openCeleris(b)
	ctx := context.Background()
	query := fmt.Sprintf("SELECT id, label FROM %s WHERE id <= $1", benchTableName)
	// Warmup.
	for i := 0; i < 2; i++ {
		rows, err := db.QueryContext(ctx, query, benchRowCount)
		if err != nil {
			b.Fatalf("warmup: %v", err)
		}
		drain(b, rows)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := db.QueryContext(ctx, query, benchRowCount)
		if err != nil {
			b.Fatalf("Query: %v", err)
		}
		drain(b, rows)
	}
}

func BenchmarkInsertPrepared_Celeris(b *testing.B) {
	db := openCeleris(b)
	ctx := context.Background()
	tbl := "celeris_bench_insert"
	if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS "+tbl); err != nil {
		b.Fatalf("drop: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE "+tbl+" (id int primary key, label text, n int)"); err != nil {
		b.Fatalf("create: %v", err)
	}
	b.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), "DROP TABLE "+tbl)
	})
	stmt, err := db.PrepareContext(ctx, "INSERT INTO "+tbl+" (id, label, n) VALUES ($1,$2,$3)")
	if err != nil {
		b.Fatalf("prepare: %v", err)
	}
	defer stmt.Close()
	// Warmup.
	for i := 0; i < 5; i++ {
		if _, err := stmt.ExecContext(ctx, i, "warmup", i); err != nil {
			b.Fatalf("warmup: %v", err)
		}
	}
	// Reset rows so PK doesn't collide in the measured loop.
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

func BenchmarkTransactionRoundtrip_Celeris(b *testing.B) {
	db := openCeleris(b)
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

func BenchmarkParallel_QueryContext_Celeris(b *testing.B) {
	db := openCeleris(b)
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

// BenchmarkPoolContention runs SELECT 1 under b.RunParallel with the pool
// sized equal to the outer parallelism. The four ALL-CAPS subtests document
// intended GOMAXPROCS targets; call `go test -cpu=1,8,64,512 -bench=...`
// to exercise them.
func BenchmarkPoolContention_Celeris(b *testing.B) {
	db := openCeleris(b)
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

// BenchmarkCopyIn_1M_Rows_Celeris is a placeholder until the celeris Pool
// exposes a streaming COPY API. The underlying protocol primitives exist
// (CopyInState, WriteCopyData) but there is no public driver method to invoke
// them from application code yet.
func BenchmarkCopyIn_1M_Rows_Celeris(b *testing.B) {
	b.Skip("celeris-postgres does not expose a public COPY API yet (v1.4.0); see #131 follow-up")
}

// ---------------------------------------------------------------------------
// Direct Pool benchmarks — bypass database/sql
// ---------------------------------------------------------------------------

// BenchmarkSelectOne_CelerisPool uses postgres.Pool directly, avoiding
// database/sql's per-query allocations (Rows/Row/Scan internals) and lock
// contention in sql.DB's conn pool.
func BenchmarkSelectOne_CelerisPool(b *testing.B) {
	pool := openCelerisPool(b)
	ctx := context.Background()
	// Warmup.
	for i := 0; i < 5; i++ {
		rows, err := pool.QueryContext(ctx, "SELECT 1")
		if err != nil {
			b.Fatalf("warmup: %v", err)
		}
		rows.Close()
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := pool.QueryContext(ctx, "SELECT 1")
		if err != nil {
			b.Fatalf("QueryContext: %v", err)
		}
		if !rows.Next() {
			rows.Close()
			b.Fatal("no row")
		}
		rows.Close()
	}
}

func BenchmarkSelect1000Rows_Text_CelerisPool(b *testing.B) {
	ensureTable(b)
	pool := openCelerisPool(b)
	ctx := context.Background()
	query := fmt.Sprintf("SELECT id, label FROM %s", benchTableName)
	for i := 0; i < 2; i++ {
		drainPool(b, pool, ctx, query)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		drainPool(b, pool, ctx, query)
	}
}

func BenchmarkSelect1000Rows_Binary_CelerisPool(b *testing.B) {
	ensureTable(b)
	pool := openCelerisPool(b)
	ctx := context.Background()
	query := fmt.Sprintf("SELECT id, label FROM %s WHERE id <= $1", benchTableName)
	for i := 0; i < 2; i++ {
		drainPoolArg(b, pool, ctx, query, benchRowCount)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		drainPoolArg(b, pool, ctx, query, benchRowCount)
	}
}

func BenchmarkInsertPrepared_CelerisPool(b *testing.B) {
	pool := openCelerisPool(b)
	ctx := context.Background()
	tbl := "celeris_pool_bench_insert"
	if _, err := pool.ExecContext(ctx, "DROP TABLE IF EXISTS "+tbl); err != nil {
		b.Fatalf("drop: %v", err)
	}
	if _, err := pool.ExecContext(ctx, "CREATE TABLE "+tbl+" (id int primary key, label text, n int)"); err != nil {
		b.Fatalf("create: %v", err)
	}
	b.Cleanup(func() {
		_, _ = pool.ExecContext(context.Background(), "DROP TABLE "+tbl)
	})
	// Warmup.
	for i := 0; i < 5; i++ {
		if _, err := pool.ExecContext(ctx, "INSERT INTO "+tbl+" (id,label,n) VALUES ($1,$2,$3)", i, "warmup", i); err != nil {
			b.Fatalf("warmup: %v", err)
		}
	}
	if _, err := pool.ExecContext(ctx, "TRUNCATE "+tbl); err != nil {
		b.Fatalf("truncate: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := pool.ExecContext(ctx, "INSERT INTO "+tbl+" (id,label,n) VALUES ($1,$2,$3)", i, "row", i*2); err != nil {
			b.Fatalf("exec: %v", err)
		}
	}
}

func BenchmarkTransactionRoundtrip_CelerisPool(b *testing.B) {
	pool := openCelerisPool(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tx, err := pool.BeginTx(ctx, nil)
		if err != nil {
			b.Fatalf("begin: %v", err)
		}
		rows, err := tx.QueryContext(ctx, "SELECT 1")
		if err != nil {
			b.Fatalf("query: %v", err)
		}
		if !rows.Next() {
			rows.Close()
			b.Fatal("no row")
		}
		rows.Close()
		if err := tx.Commit(); err != nil {
			b.Fatalf("commit: %v", err)
		}
	}
}

func BenchmarkParallel_CelerisPool(b *testing.B) {
	pool := openCelerisPool(b)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		ctx := context.Background()
		for pb.Next() {
			rows, err := pool.QueryContext(ctx, "SELECT 1")
			if err != nil {
				b.Fatalf("QueryContext: %v", err)
			}
			if !rows.Next() {
				rows.Close()
				b.Fatal("no row")
			}
			rows.Close()
		}
	})
}

func BenchmarkPoolContention_CelerisPool(b *testing.B) {
	pool := openCelerisPool(b)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		ctx := context.Background()
		for pb.Next() {
			rows, err := pool.QueryContext(ctx, "SELECT 1")
			if err != nil {
				b.Fatalf("QueryContext: %v", err)
			}
			if !rows.Next() {
				rows.Close()
				b.Fatal("no row")
			}
			rows.Close()
		}
	})
}

func drainPool(b *testing.B, pool *postgres.Pool, ctx context.Context, q string) {
	b.Helper()
	rows, err := pool.QueryContext(ctx, q)
	if err != nil {
		b.Fatalf("query: %v", err)
	}
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		b.Fatalf("err: %v", err)
	}
	rows.Close()
}

func drainPoolArg(b *testing.B, pool *postgres.Pool, ctx context.Context, q string, arg any) {
	b.Helper()
	rows, err := pool.QueryContext(ctx, q, arg)
	if err != nil {
		b.Fatalf("query: %v", err)
	}
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		b.Fatalf("err: %v", err)
	}
	rows.Close()
}

// drain consumes all rows + their label column, discarding results — just
// enough work to make sure the driver actually decodes every cell.
func drain(b *testing.B, rows *sql.Rows) {
	b.Helper()
	for rows.Next() {
		var id int
		var label string
		if err := rows.Scan(&id, &label); err != nil {
			_ = rows.Close()
			b.Fatalf("scan: %v", err)
		}
	}
	if err := rows.Err(); err != nil {
		b.Fatalf("err: %v", err)
	}
	_ = rows.Close()
}
