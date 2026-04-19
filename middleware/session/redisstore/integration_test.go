//go:build integration

package redisstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/redis"
	"github.com/goceleris/celeris/middleware/store"
)

// newLiveClient connects to the Redis instance named by
// CELERIS_REDIS_ADDR (default 127.0.0.1:6379) or skips the test.
func newLiveClient(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("CELERIS_REDIS_ADDR")
	if addr == "" {
		t.Skip("CELERIS_REDIS_ADDR unset; skipping live Redis integration")
	}
	c, err := redis.NewClient(addr)
	if err != nil {
		t.Fatalf("redis.NewClient(%s): %v", addr, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestSessionRedisLive_RoundTrip(t *testing.T) {
	client := newLiveClient(t)
	// Unique prefix per test so concurrent runs don't collide.
	prefix := fmt.Sprintf("sess-live-%d:", time.Now().UnixNano())
	s := New(client, Options{KeyPrefix: prefix})
	ctx := context.Background()
	t.Cleanup(func() { _ = s.DeletePrefix(ctx, "") })

	if err := s.Set(ctx, "abc", []byte("payload"), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Get(ctx, "abc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "payload" {
		t.Fatalf("Get: got %q", got)
	}
	if err := s.Delete(ctx, "abc"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, "abc"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after Delete: expected ErrNotFound, got %v", err)
	}
}

func TestSessionRedisLive_TTLExpiry(t *testing.T) {
	client := newLiveClient(t)
	prefix := fmt.Sprintf("sess-live-%d:", time.Now().UnixNano())
	s := New(client, Options{KeyPrefix: prefix})
	ctx := context.Background()
	t.Cleanup(func() { _ = s.DeletePrefix(ctx, "") })

	if err := s.Set(ctx, "short", []byte("v"), time.Second); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// Redis TTL resolution is seconds; wait comfortably beyond.
	time.Sleep(1500 * time.Millisecond)
	if _, err := s.Get(ctx, "short"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after TTL: expected ErrNotFound, got %v", err)
	}
}

func TestSessionRedisLive_ScanAndDeletePrefix(t *testing.T) {
	client := newLiveClient(t)
	prefix := fmt.Sprintf("sess-live-%d:", time.Now().UnixNano())
	s := New(client, Options{KeyPrefix: prefix, ScanCount: 50})
	ctx := context.Background()
	t.Cleanup(func() { _ = s.DeletePrefix(ctx, "") })

	_ = s.Set(ctx, "a1", []byte("1"), time.Minute)
	_ = s.Set(ctx, "a2", []byte("2"), time.Minute)
	_ = s.Set(ctx, "b1", []byte("3"), time.Minute)

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
	all, _ := s.Scan(ctx, "")
	sort.Strings(all)
	if len(all) != 1 || all[0] != "b1" {
		t.Fatalf("after DeletePrefix a: %v", all)
	}
}
