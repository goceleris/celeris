package postgresstore

import (
	"context"
	"os"
	"testing"

	"github.com/goceleris/celeris/driver/postgres"
)

// bench uses a live PG for realistic allocs/op; skip when unset.
func newBenchStore(b *testing.B) *Store {
	b.Helper()
	dsn := os.Getenv("CELERIS_PG_DSN")
	if dsn == "" {
		b.Skip("CELERIS_PG_DSN not set")
	}
	pool, err := postgres.Open(dsn)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { _ = pool.Close() })
	s, err := New(context.Background(), pool, Options{CleanupInterval: 0})
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	b.Cleanup(func() { _ = s.Close() })
	return s
}

func BenchmarkGet(b *testing.B) {
	s := newBenchStore(b)
	ctx := context.Background()
	key := "bench-get"
	_ = s.Set(ctx, key, []byte("hello world"), 0)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = s.Get(ctx, key)
	}
}

func BenchmarkSet(b *testing.B) {
	s := newBenchStore(b)
	ctx := context.Background()
	val := []byte("hello world")
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = s.Set(ctx, "bench-set", val, 0)
	}
}
