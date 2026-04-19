//go:build integration

package postgresstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/postgres"
	"github.com/goceleris/celeris/middleware/store"
)

// runWithPG spins up a pool and a fresh store pointing at a randomly-named
// table so parallel test runs don't collide. Cleans up on test end.
func runWithPG(t *testing.T) (*postgres.Pool, *Store) {
	t.Helper()
	dsn := os.Getenv("CELERIS_PG_DSN")
	if dsn == "" {
		t.Skip("CELERIS_PG_DSN unset; skipping integration test")
	}
	pool, err := postgres.Open(dsn)
	if err != nil {
		t.Fatalf("postgres.Open: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	table := fmt.Sprintf("celeris_sessions_%d", time.Now().UnixNano())
	s, err := New(context.Background(), pool, Options{TableName: table, CleanupInterval: 10 * time.Second})
	if err != nil {
		t.Fatalf("postgresstore.New: %v", err)
	}
	t.Cleanup(func() {
		_ = s.Close()
		_, _ = pool.ExecContext(context.Background(), fmt.Sprintf("DROP TABLE IF EXISTS %s", table))
	})
	return pool, s
}

func TestSetGetDelete_Integration(t *testing.T) {
	_, s := runWithPG(t)
	ctx := context.Background()

	if err := s.Set(ctx, "alpha", []byte("hello"), time.Hour); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, err := s.Get(ctx, "alpha")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(v) != "hello" {
		t.Fatalf("Get: got %q", v)
	}
	if err := s.Delete(ctx, "alpha"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, "alpha"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after Delete: expected ErrNotFound, got %v", err)
	}
}

func TestTTLExpiry_Integration(t *testing.T) {
	_, s := runWithPG(t)
	ctx := context.Background()
	if err := s.Set(ctx, "short", []byte("v"), 200*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}
	time.Sleep(400 * time.Millisecond)
	if _, err := s.Get(ctx, "short"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound post-TTL, got %v", err)
	}
}

func TestUpsert_Integration(t *testing.T) {
	_, s := runWithPG(t)
	ctx := context.Background()
	if err := s.Set(ctx, "u", []byte("v1"), 0); err != nil {
		t.Fatalf("first Set: %v", err)
	}
	if err := s.Set(ctx, "u", []byte("v2"), 0); err != nil {
		t.Fatalf("second Set: %v", err)
	}
	v, _ := s.Get(ctx, "u")
	if string(v) != "v2" {
		t.Fatalf("upsert: got %q", v)
	}
}

func TestDeletePrefix_Integration(t *testing.T) {
	_, s := runWithPG(t)
	ctx := context.Background()
	_ = s.Set(ctx, "a1", []byte("1"), 0)
	_ = s.Set(ctx, "a2", []byte("2"), 0)
	_ = s.Set(ctx, "b1", []byte("3"), 0)
	if err := s.DeletePrefix(ctx, "a"); err != nil {
		t.Fatalf("DeletePrefix: %v", err)
	}
	if _, err := s.Get(ctx, "a1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a1 should be gone")
	}
	v, _ := s.Get(ctx, "b1")
	if string(v) != "3" {
		t.Fatal("b1 should remain")
	}
}

func TestTruncateEmptyPrefix_Integration(t *testing.T) {
	_, s := runWithPG(t)
	ctx := context.Background()
	_ = s.Set(ctx, "k1", []byte("1"), 0)
	_ = s.Set(ctx, "k2", []byte("2"), 0)
	if err := s.DeletePrefix(ctx, ""); err != nil {
		t.Fatalf("TRUNCATE: %v", err)
	}
	if _, err := s.Get(ctx, "k1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("TRUNCATE should remove all rows")
	}
}
