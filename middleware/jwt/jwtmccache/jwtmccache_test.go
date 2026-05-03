package jwtmccache

import (
	"context"
	"errors"
	"testing"
	"time"

	celmc "github.com/goceleris/celeris/driver/memcached"
	"github.com/goceleris/celeris/middleware/internal/fakememcached"
	"github.com/goceleris/celeris/middleware/store"
)

func newClient(t *testing.T) (*celmc.Client, *fakememcached.Server) {
	t.Helper()
	srv := fakememcached.Start(t)
	c, err := celmc.NewClient(srv.Addr())
	if err != nil {
		t.Fatalf("celmc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, srv
}

func TestJWTMCCacheRoundTrip(t *testing.T) {
	c, _ := newClient(t)
	kv := New(c)
	ctx := context.Background()
	if err := kv.Set(ctx, "issuer-a", []byte("payload"), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, err := kv.Get(ctx, "issuer-a")
	if err != nil || string(v) != "payload" {
		t.Fatalf("Get: got=%q err=%v", v, err)
	}
}

func TestJWTMCCacheMissingIsErrNotFound(t *testing.T) {
	c, _ := newClient(t)
	kv := New(c)
	if _, err := kv.Get(context.Background(), "absent"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestJWTMCCacheCustomPrefix(t *testing.T) {
	c, srv := newClient(t)
	kv := New(c, Options{KeyPrefix: "custom:"})
	_ = kv.Set(context.Background(), "k", []byte("v"), 0)
	if srv.Data()["custom:k"] != "v" {
		t.Fatalf("raw key: expected custom:k → v, got %v", srv.Data())
	}
}
