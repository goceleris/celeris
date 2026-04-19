//go:build integration

package memcachedstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	celmc "github.com/goceleris/celeris/driver/memcached"
	"github.com/goceleris/celeris/middleware/store"
)

func newLiveClient(t *testing.T) *celmc.Client {
	t.Helper()
	addr := os.Getenv("CELERIS_MEMCACHED_ADDR")
	if addr == "" {
		t.Skip("CELERIS_MEMCACHED_ADDR unset; skipping live memcached integration")
	}
	c, err := celmc.NewClient(addr)
	if err != nil {
		t.Fatalf("celmc.NewClient(%s): %v", addr, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestSessionMemcachedLive_RoundTrip(t *testing.T) {
	c := newLiveClient(t)
	prefix := fmt.Sprintf("sess-live-%d:", time.Now().UnixNano())
	s := New(c, Options{KeyPrefix: prefix})
	ctx := context.Background()
	t.Cleanup(func() { _ = s.Delete(ctx, "alpha") })

	if err := s.Set(ctx, "alpha", []byte("payload"), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Get(ctx, "alpha")
	if err != nil || string(got) != "payload" {
		t.Fatalf("Get: got=%q err=%v", got, err)
	}
	if err := s.Delete(ctx, "alpha"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, "alpha"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after Delete: %v", err)
	}
}

func TestSessionMemcachedLive_TTLExpiry(t *testing.T) {
	c := newLiveClient(t)
	prefix := fmt.Sprintf("sess-live-%d:", time.Now().UnixNano())
	s := New(c, Options{KeyPrefix: prefix})
	ctx := context.Background()
	if err := s.Set(ctx, "short", []byte("v"), time.Second); err != nil {
		t.Fatalf("Set: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := s.Get(ctx, "short"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("post-TTL: expected ErrNotFound, got %v", err)
	}
}
