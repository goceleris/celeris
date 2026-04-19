package jwtcache

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/redis"
	"github.com/goceleris/celeris/middleware/internal/fakeredis"
	"github.com/goceleris/celeris/middleware/store"
)

func TestJWTCacheRoundTrip(t *testing.T) {
	srv := fakeredis.Start(t)
	client, err := redis.NewClient(srv.Addr(), redis.WithForceRESP2())
	if err != nil {
		t.Fatalf("redis.NewClient: %v", err)
	}
	defer func() { _ = client.Close() }()
	kv := New(client)

	ctx := context.Background()
	if err := kv.Set(ctx, "key1", []byte("payload"), time.Hour); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, err := kv.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(v) != "payload" {
		t.Fatalf("Get: got %q", v)
	}
}

func TestJWTCacheMissingIsErrNotFound(t *testing.T) {
	srv := fakeredis.Start(t)
	client, err := redis.NewClient(srv.Addr(), redis.WithForceRESP2())
	if err != nil {
		t.Fatalf("redis.NewClient: %v", err)
	}
	defer func() { _ = client.Close() }()
	kv := New(client)
	if _, err := kv.Get(context.Background(), "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestJWTCacheCustomPrefix(t *testing.T) {
	srv := fakeredis.Start(t)
	client, err := redis.NewClient(srv.Addr(), redis.WithForceRESP2())
	if err != nil {
		t.Fatalf("redis.NewClient: %v", err)
	}
	defer func() { _ = client.Close() }()
	kv := New(client, Options{KeyPrefix: "custom:"})
	_ = kv.Set(context.Background(), "k", []byte("v"), 0)

	raw := srv.Data()
	if raw["custom:k"] != "v" {
		t.Fatalf("expected raw key custom:k, got %v", raw)
	}
}
