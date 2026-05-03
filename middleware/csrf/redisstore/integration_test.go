//go:build integration

package redisstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/redis"
	"github.com/goceleris/celeris/middleware/store"
)

func newLiveClient(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("CELERIS_REDIS_ADDR")
	if addr == "" {
		t.Skip("CELERIS_REDIS_ADDR unset")
	}
	c, err := redis.NewClient(addr)
	if err != nil {
		t.Fatalf("redis.NewClient(%s): %v", addr, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestCSRFRedisLive_GetDelAtomic(t *testing.T) {
	client := newLiveClient(t)
	prefix := fmt.Sprintf("csrf-live-%d:", time.Now().UnixNano())
	s := New(client, Options{KeyPrefix: prefix})
	ctx := context.Background()

	_ = s.Set(ctx, "tok", []byte("abc"), time.Minute)
	v, err := s.GetAndDelete(ctx, "tok")
	if err != nil {
		t.Fatalf("GetAndDelete: %v", err)
	}
	if string(v) != "abc" {
		t.Fatalf("got %q", v)
	}
	if _, err := s.GetAndDelete(ctx, "tok"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second: expected ErrNotFound, got %v", err)
	}
}

func TestCSRFRedisLive_OldRedisCompatFallback(t *testing.T) {
	client := newLiveClient(t)
	prefix := fmt.Sprintf("csrf-live-%d:", time.Now().UnixNano())
	s := New(client, Options{KeyPrefix: prefix, OldRedisCompat: true})
	ctx := context.Background()

	_ = s.Set(ctx, "tok", []byte("v"), time.Minute)
	v, err := s.GetAndDelete(ctx, "tok")
	if err != nil {
		t.Fatalf("GetAndDelete fallback: %v", err)
	}
	if string(v) != "v" {
		t.Fatalf("got %q", v)
	}
	if _, err := s.GetAndDelete(ctx, "tok"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second: expected ErrNotFound, got %v", err)
	}
}
