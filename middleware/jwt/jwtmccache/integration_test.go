//go:build integration

package jwtmccache

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

func TestJWTMCCacheLive_RoundTrip(t *testing.T) {
	addr := os.Getenv("CELERIS_MEMCACHED_ADDR")
	if addr == "" {
		t.Skip("CELERIS_MEMCACHED_ADDR unset")
	}
	c, err := celmc.NewClient(addr)
	if err != nil {
		t.Fatalf("celmc.NewClient: %v", err)
	}
	defer func() { _ = c.Close() }()
	kv := New(c, Options{KeyPrefix: fmt.Sprintf("jwks-live-%d:", time.Now().UnixNano())})

	ctx := context.Background()
	body := []byte(`{"keys":[{"kty":"oct","k":"example"}]}`)
	if err := kv.Set(ctx, "issuer-a", body, time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, err := kv.Get(ctx, "issuer-a")
	if err != nil || string(v) != string(body) {
		t.Fatalf("Get: %v", err)
	}
	if err := kv.Delete(ctx, "issuer-a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := kv.Get(ctx, "issuer-a"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after Delete: %v", err)
	}
}
