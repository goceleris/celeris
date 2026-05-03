//go:build integration

package jwtcache

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

func TestJWTCacheLive_RoundTrip(t *testing.T) {
	client := newLiveClient(t)
	prefix := fmt.Sprintf("jwks-live-%d:", time.Now().UnixNano())
	kv := New(client, Options{KeyPrefix: prefix})
	ctx := context.Background()

	body := []byte(`{"keys":[{"kty":"oct","k":"example"}]}`)
	if err := kv.Set(ctx, "issuer-a", body, time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := kv.Get(ctx, "issuer-a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("Get: got %q", got)
	}
	if err := kv.Delete(ctx, "issuer-a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := kv.Get(ctx, "issuer-a"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after Delete: expected ErrNotFound, got %v", err)
	}
}
