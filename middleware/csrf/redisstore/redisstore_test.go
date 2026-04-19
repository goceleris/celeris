package redisstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/redis"
	"github.com/goceleris/celeris/middleware/internal/fakeredis"
	"github.com/goceleris/celeris/middleware/store"
)

func newTestClient(t *testing.T) *redis.Client {
	t.Helper()
	srv := fakeredis.Start(t)
	c, err := redis.NewClient(srv.Addr(), redis.WithForceRESP2())
	if err != nil {
		t.Fatalf("redis.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestSetGetDelete(t *testing.T) {
	client := newTestClient(t)
	s := New(client)
	ctx := context.Background()

	if err := s.Set(ctx, "tok1", []byte("abc"), time.Hour); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, err := s.Get(ctx, "tok1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(v) != "abc" {
		t.Fatalf("Get: got %q", v)
	}
	if err := s.Delete(ctx, "tok1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, "tok1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after Delete: expected ErrNotFound, got %v", err)
	}
}

func TestGetAndDeleteAtomic(t *testing.T) {
	client := newTestClient(t)
	s := New(client)
	ctx := context.Background()
	_ = s.Set(ctx, "single", []byte("token"), time.Hour)

	v, err := s.GetAndDelete(ctx, "single")
	if err != nil {
		t.Fatalf("GetAndDelete: %v", err)
	}
	if string(v) != "token" {
		t.Fatalf("GetAndDelete: got %q", v)
	}
	// Second call should miss.
	_, err = s.GetAndDelete(ctx, "single")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after GetAndDelete: expected ErrNotFound, got %v", err)
	}
}

func TestGetAndDeleteFallback(t *testing.T) {
	client := newTestClient(t)
	s := New(client, Options{OldRedisCompat: true})
	ctx := context.Background()
	_ = s.Set(ctx, "single", []byte("tok"), time.Hour)

	v, err := s.GetAndDelete(ctx, "single")
	if err != nil {
		t.Fatalf("GetAndDelete fallback: %v", err)
	}
	if string(v) != "tok" {
		t.Fatalf("fallback: got %q", v)
	}
	_, err = s.GetAndDelete(ctx, "single")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second fallback call: expected ErrNotFound, got %v", err)
	}
}

func TestMissingIsErrNotFound(t *testing.T) {
	client := newTestClient(t)
	s := New(client)
	ctx := context.Background()
	if _, err := s.Get(ctx, "absent"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get absent: expected ErrNotFound, got %v", err)
	}
	if _, err := s.GetAndDelete(ctx, "absent"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetAndDelete absent: expected ErrNotFound, got %v", err)
	}
}

func TestInterfaceConformance(t *testing.T) {
	client := newTestClient(t)
	var _ store.KV = New(client)
	var _ store.GetAndDeleter = New(client)
}
