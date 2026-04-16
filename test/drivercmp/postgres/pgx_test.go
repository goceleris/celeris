package postgres_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// openPgxPool opens a pgx native pool; we use this for apples-to-apples with
// celeris's native Pool. The database/sql-wrapped pgx flavor is measured
// separately where useful.
func openPgxPool(b *testing.B) *pgxpool.Pool {
	b.Helper()
	cfg, err := pgxpool.ParseConfig(dsn(b))
	if err != nil {
		b.Fatalf("pgxpool.ParseConfig: %v", err)
	}
	cfg.MaxConns = 64
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		b.Fatalf("pgxpool.NewWithConfig: %v", err)
	}
	b.Cleanup(p.Close)
	if err := p.Ping(context.Background()); err != nil {
		b.Skipf("ping: %v", err)
	}
	return p
}

func BenchmarkSelectOne_Pgx(b *testing.B) {
	p := openPgxPool(b)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		var n int
		_ = p.QueryRow(ctx, "SELECT 1").Scan(&n)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var n int
		if err := p.QueryRow(ctx, "SELECT 1").Scan(&n); err != nil {
			b.Fatalf("Scan: %v", err)
		}
	}
}

func BenchmarkSelect1000Rows_Text_Pgx(b *testing.B) {
	ensureTable(b)
	p := openPgxPool(b)
	ctx := context.Background()
	query := fmt.Sprintf("SELECT id, label FROM %s", benchTableName)
	for i := 0; i < 2; i++ {
		drainPgx(b, p, ctx, query)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		drainPgx(b, p, ctx, query)
	}
}

func BenchmarkSelect1000Rows_Binary_Pgx(b *testing.B) {
	ensureTable(b)
	p := openPgxPool(b)
	ctx := context.Background()
	query := fmt.Sprintf("SELECT id, label FROM %s WHERE id <= $1", benchTableName)
	// Parameterized query uses the extended protocol in binary — pgx's
	// default, so this is the fair comparison for celeris's binary path.
	for i := 0; i < 2; i++ {
		drainPgxArg(b, p, ctx, query, benchRowCount)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		drainPgxArg(b, p, ctx, query, benchRowCount)
	}
}

func BenchmarkInsertPrepared_Pgx(b *testing.B) {
	p := openPgxPool(b)
	ctx := context.Background()
	tbl := "pgx_bench_insert"
	if _, err := p.Exec(ctx, "DROP TABLE IF EXISTS "+tbl); err != nil {
		b.Fatalf("drop: %v", err)
	}
	if _, err := p.Exec(ctx, "CREATE TABLE "+tbl+" (id int primary key, label text, n int)"); err != nil {
		b.Fatalf("create: %v", err)
	}
	b.Cleanup(func() {
		_, _ = p.Exec(context.Background(), "DROP TABLE "+tbl)
	})
	// pgx auto-prepares the same SQL on first Exec in simple protocol when
	// QueryExecModeCacheStatement is used (its default).
	for i := 0; i < 5; i++ {
		if _, err := p.Exec(ctx, "INSERT INTO "+tbl+" (id,label,n) VALUES ($1,$2,$3)", i, "warmup", i); err != nil {
			b.Fatalf("warmup: %v", err)
		}
	}
	if _, err := p.Exec(ctx, "TRUNCATE "+tbl); err != nil {
		b.Fatalf("truncate: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := p.Exec(ctx, "INSERT INTO "+tbl+" (id,label,n) VALUES ($1,$2,$3)", i, "row", i*2); err != nil {
			b.Fatalf("exec: %v", err)
		}
	}
}

func BenchmarkTransactionRoundtrip_Pgx(b *testing.B) {
	p := openPgxPool(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tx, err := p.Begin(ctx)
		if err != nil {
			b.Fatalf("begin: %v", err)
		}
		var n int
		if err := tx.QueryRow(ctx, "SELECT 1").Scan(&n); err != nil {
			b.Fatalf("scan: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			b.Fatalf("commit: %v", err)
		}
	}
}

func BenchmarkParallel_QueryContext_Pgx(b *testing.B) {
	p := openPgxPool(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			var n int
			if err := p.QueryRow(ctx, "SELECT 1").Scan(&n); err != nil {
				b.Fatalf("scan: %v", err)
			}
		}
	})
}

func BenchmarkPoolContention_Pgx(b *testing.B) {
	BenchmarkParallel_QueryContext_Pgx(b)
}

// BenchmarkCopyIn_1M_Rows_Pgx measures pgx's CopyFrom over a 1M-row stream.
// Included for completeness; celeris's counterpart is skipped until a public
// COPY API lands.
func BenchmarkCopyIn_1M_Rows_Pgx(b *testing.B) {
	if testing.Short() {
		b.Skip("skipping 1M-row COPY in -short mode")
	}
	p := openPgxPool(b)
	ctx := context.Background()
	tbl := "pgx_bench_copy"
	if _, err := p.Exec(ctx, "DROP TABLE IF EXISTS "+tbl); err != nil {
		b.Fatalf("drop: %v", err)
	}
	if _, err := p.Exec(ctx, "CREATE UNLOGGED TABLE "+tbl+" (id int primary key, label text)"); err != nil {
		b.Fatalf("create: %v", err)
	}
	b.Cleanup(func() {
		_, _ = p.Exec(context.Background(), "DROP TABLE "+tbl)
	})

	const rows = 1_000_000
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		if _, err := p.Exec(ctx, "TRUNCATE "+tbl); err != nil {
			b.Fatalf("truncate: %v", err)
		}
		b.StartTimer()
		src := &copySource{rows: rows}
		conn, err := p.Acquire(ctx)
		if err != nil {
			b.Fatalf("acquire: %v", err)
		}
		_, err = conn.Conn().CopyFrom(ctx, pgx.Identifier{tbl}, []string{"id", "label"}, src)
		conn.Release()
		if err != nil {
			b.Fatalf("CopyFrom: %v", err)
		}
	}
}

// copySource implements pgx.CopyFromSource for the COPY benchmark.
type copySource struct {
	i    int
	rows int
}

func (c *copySource) Next() bool             { c.i++; return c.i <= c.rows }
func (c *copySource) Values() ([]any, error) { return []any{c.i, fmt.Sprintf("row_%d", c.i)}, nil }
func (c *copySource) Err() error             { return nil }

func drainPgx(b *testing.B, p *pgxpool.Pool, ctx context.Context, q string) {
	b.Helper()
	rows, err := p.Query(ctx, q)
	if err != nil {
		b.Fatalf("query: %v", err)
	}
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

func drainPgxArg(b *testing.B, p *pgxpool.Pool, ctx context.Context, q string, arg any) {
	b.Helper()
	rows, err := p.Query(ctx, q, arg)
	if err != nil {
		b.Fatalf("query: %v", err)
	}
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
