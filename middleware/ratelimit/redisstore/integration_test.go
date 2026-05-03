//go:build integration

package redisstore

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/redis"
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

// TestRatelimitRedisLive_TokenBucket exercises the Lua EVALSHA path
// against a real Redis. The token bucket drains after Burst requests
// and recovers as Redis time advances.
func TestRatelimitRedisLive_TokenBucket(t *testing.T) {
	client := newLiveClient(t)
	prefix := fmt.Sprintf("rl-live-%d:", time.Now().UnixNano())
	store, err := New(context.Background(), client, Options{
		KeyPrefix: prefix,
		RPS:       10,
		Burst:     5,
		TTL:       2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// First 5 must succeed.
	for i := 0; i < 5; i++ {
		ok, _, _, err := store.Allow("user-42")
		if err != nil {
			t.Fatalf("Allow %d: %v", i, err)
		}
		if !ok {
			t.Fatalf("Allow %d: expected true", i)
		}
	}
	// Sixth must fail.
	ok, _, _, err := store.Allow("user-42")
	if err != nil {
		t.Fatalf("Allow 5: %v", err)
	}
	if ok {
		t.Fatal("Allow 5: expected rate-limited (false)")
	}
}

func TestRatelimitRedisLive_Undo(t *testing.T) {
	client := newLiveClient(t)
	prefix := fmt.Sprintf("rl-live-%d:", time.Now().UnixNano())
	store, err := New(context.Background(), client, Options{
		KeyPrefix: prefix,
		RPS:       10,
		Burst:     1,
		TTL:       2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	key := "undo-key"

	ok, _, _, err := store.Allow(key)
	if err != nil || !ok {
		t.Fatalf("first Allow: ok=%v err=%v", ok, err)
	}
	if err := store.Undo(key); err != nil {
		t.Fatalf("Undo: %v", err)
	}
	ok, _, _, err = store.Allow(key)
	if err != nil || !ok {
		t.Fatalf("post-Undo Allow: ok=%v err=%v", ok, err)
	}
}
