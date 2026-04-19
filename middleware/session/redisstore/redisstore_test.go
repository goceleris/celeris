package redisstore

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/redis"
	"github.com/goceleris/celeris/middleware/internal/fakeredis"
	"github.com/goceleris/celeris/middleware/store"
)

func newTestClient(t *testing.T) (*redis.Client, *fakeredis.Server) {
	t.Helper()
	srv := fakeredis.Start(t)
	c, err := redis.NewClient(srv.Addr(), redis.WithForceRESP2())
	if err != nil {
		t.Fatalf("redis.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, srv
}

func TestSetGetRoundTrip(t *testing.T) {
	client, _ := newTestClient(t)
	s := New(client)

	ctx := context.Background()
	if err := s.Set(ctx, "alpha", []byte("hello"), time.Hour); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Get(ctx, "alpha")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("Get: got %q want %q", got, "hello")
	}
}

func TestGetMissingReturnsErrNotFound(t *testing.T) {
	client, _ := newTestClient(t)
	s := New(client)
	_, err := s.Get(context.Background(), "absent")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get missing: expected ErrNotFound, got %v", err)
	}
}

func TestDelete(t *testing.T) {
	client, _ := newTestClient(t)
	s := New(client)
	ctx := context.Background()
	_ = s.Set(ctx, "k", []byte("v"), 0)
	if err := s.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, "k"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after Delete: expected ErrNotFound, got %v", err)
	}
}

func TestPrefixIsolation(t *testing.T) {
	client, srv := newTestClient(t)
	s := New(client, Options{KeyPrefix: "myprefix:"})
	ctx := context.Background()
	_ = s.Set(ctx, "k", []byte("v"), 0)

	// Raw key in Redis includes the prefix.
	raw := srv.Data()
	if raw["myprefix:k"] != "v" {
		t.Fatalf("expected raw key myprefix:k, got data=%v", raw)
	}
}

func TestScanAndDeletePrefix(t *testing.T) {
	client, _ := newTestClient(t)
	s := New(client)
	ctx := context.Background()
	_ = s.Set(ctx, "a1", []byte("1"), 0)
	_ = s.Set(ctx, "a2", []byte("2"), 0)
	_ = s.Set(ctx, "b1", []byte("3"), 0)

	keys, err := s.Scan(ctx, "a")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	sort.Strings(keys)
	if len(keys) != 2 || keys[0] != "a1" || keys[1] != "a2" {
		t.Fatalf("Scan(a): got %v", keys)
	}

	if err := s.DeletePrefix(ctx, "a"); err != nil {
		t.Fatalf("DeletePrefix: %v", err)
	}
	remaining, _ := s.Scan(ctx, "")
	sort.Strings(remaining)
	if len(remaining) != 1 || remaining[0] != "b1" {
		t.Fatalf("after DeletePrefix a: remaining=%v", remaining)
	}
}

func TestInterfaceConformance(t *testing.T) {
	client, _ := newTestClient(t)
	var _ store.KV = New(client)
	var _ store.Scanner = New(client)
	var _ store.PrefixDeleter = New(client)
}
