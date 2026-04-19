//go:build redis

package redis_test

import (
	"errors"
	"testing"
	"time"

	celredis "github.com/goceleris/celeris/driver/redis"
)

func TestStrings_SetGet(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "setget")
	cleanupKeys(t, c, key)

	if err := c.Set(ctx, key, "hello", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := c.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "hello" {
		t.Fatalf("Get = %q, want %q", got, "hello")
	}
}

func TestStrings_GetMissingReturnsErrNil(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "missing")

	_, err := c.Get(ctx, key)
	if !errors.Is(err, celredis.ErrNil) {
		t.Fatalf("Get on missing = %v, want ErrNil", err)
	}
}

func TestStrings_SetNX(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "setnx")
	cleanupKeys(t, c, key)

	ok, err := c.SetNX(ctx, key, "first", 0)
	if err != nil {
		t.Fatalf("SetNX first: %v", err)
	}
	if !ok {
		t.Fatalf("SetNX first = false, want true")
	}
	ok, err = c.SetNX(ctx, key, "second", 0)
	if err != nil {
		t.Fatalf("SetNX second: %v", err)
	}
	if ok {
		t.Fatalf("SetNX second = true, want false (key already exists)")
	}
	got, _ := c.Get(ctx, key)
	if got != "first" {
		t.Fatalf("Get after SetNX = %q, want %q", got, "first")
	}
}

func TestStrings_DelExists(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	k1 := uniqueKey(t, "a")
	k2 := uniqueKey(t, "b")
	cleanupKeys(t, c, k1, k2)

	if err := c.Set(ctx, k1, "1", 0); err != nil {
		t.Fatalf("Set k1: %v", err)
	}
	if err := c.Set(ctx, k2, "2", 0); err != nil {
		t.Fatalf("Set k2: %v", err)
	}
	n, err := c.Exists(ctx, k1, k2)
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if n != 2 {
		t.Fatalf("Exists = %d, want 2", n)
	}
	n, err = c.Del(ctx, k1, k2)
	if err != nil {
		t.Fatalf("Del: %v", err)
	}
	if n != 2 {
		t.Fatalf("Del = %d, want 2", n)
	}
	n, _ = c.Exists(ctx, k1, k2)
	if n != 0 {
		t.Fatalf("Exists after Del = %d, want 0", n)
	}
}

func TestStrings_IncrDecr(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "counter")
	cleanupKeys(t, c, key)

	n, err := c.Incr(ctx, key)
	if err != nil {
		t.Fatalf("Incr: %v", err)
	}
	if n != 1 {
		t.Fatalf("Incr first = %d, want 1", n)
	}
	n, _ = c.Incr(ctx, key)
	if n != 2 {
		t.Fatalf("Incr second = %d, want 2", n)
	}
	n, err = c.Decr(ctx, key)
	if err != nil {
		t.Fatalf("Decr: %v", err)
	}
	if n != 1 {
		t.Fatalf("Decr = %d, want 1", n)
	}
}

func TestStrings_ExpireTTL(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "ttl")
	cleanupKeys(t, c, key)

	if err := c.Set(ctx, key, "v", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	ok, err := c.Expire(ctx, key, 60*time.Second)
	if err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if !ok {
		t.Fatalf("Expire returned false, want true")
	}
	ttl, err := c.TTL(ctx, key)
	if err != nil {
		t.Fatalf("TTL: %v", err)
	}
	if ttl <= 0 || ttl > 60*time.Second {
		t.Fatalf("TTL = %v, want (0, 60s]", ttl)
	}

	// TTL on non-existent key should return -2 seconds.
	missing, err := c.TTL(ctx, uniqueKey(t, "notset"))
	if err != nil {
		t.Fatalf("TTL missing: %v", err)
	}
	if missing != -2*time.Second {
		t.Fatalf("TTL missing = %v, want -2s", missing)
	}
}

func TestStrings_SetWithExpiration(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "setex")
	cleanupKeys(t, c, key)

	if err := c.Set(ctx, key, "v", 60*time.Second); err != nil {
		t.Fatalf("Set: %v", err)
	}
	ttl, err := c.TTL(ctx, key)
	if err != nil {
		t.Fatalf("TTL: %v", err)
	}
	if ttl <= 0 || ttl > 60*time.Second {
		t.Fatalf("TTL = %v, want (0, 60s]", ttl)
	}
}

func TestStrings_MGet(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	k1 := uniqueKey(t, "a")
	k2 := uniqueKey(t, "b")
	k3 := uniqueKey(t, "c") // intentionally unset
	cleanupKeys(t, c, k1, k2, k3)

	_ = c.Set(ctx, k1, "v1", 0)
	_ = c.Set(ctx, k2, "v2", 0)

	out, err := c.MGet(ctx, k1, k2, k3)
	if err != nil {
		t.Fatalf("MGet: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("MGet len = %d, want 3", len(out))
	}
	if out[0] != "v1" || out[1] != "v2" {
		t.Fatalf("MGet values = %v, want [v1 v2 \"\"]", out)
	}
	// out[2] represents the missing key; the driver decodes nulls as "".
	if out[2] != "" {
		t.Fatalf("MGet missing slot = %q, want empty", out[2])
	}
}

func TestStrings_GetBytes(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "bytes")
	cleanupKeys(t, c, key)

	if err := c.Set(ctx, key, "binary\x00payload", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := c.GetBytes(ctx, key)
	if err != nil {
		t.Fatalf("GetBytes: %v", err)
	}
	if string(got) != "binary\x00payload" {
		t.Fatalf("GetBytes = %q, want %q", got, "binary\x00payload")
	}
}
