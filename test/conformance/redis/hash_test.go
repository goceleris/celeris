//go:build redis

package redis_test

import (
	"errors"
	"sort"
	"testing"
	"time"

	celredis "github.com/goceleris/celeris/driver/redis"
)

func TestHash_HSetHGet(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "h")
	cleanupKeys(t, c, key)

	n, err := c.HSet(ctx, key, "field1", "value1", "field2", "value2")
	if err != nil {
		t.Fatalf("HSet: %v", err)
	}
	if n != 2 {
		t.Fatalf("HSet = %d, want 2", n)
	}
	got, err := c.HGet(ctx, key, "field1")
	if err != nil {
		t.Fatalf("HGet: %v", err)
	}
	if got != "value1" {
		t.Fatalf("HGet field1 = %q, want value1", got)
	}
}

func TestHash_HGetMissingField(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "h")
	cleanupKeys(t, c, key)

	_, err := c.HSet(ctx, key, "known", "x")
	if err != nil {
		t.Fatalf("HSet: %v", err)
	}
	_, err = c.HGet(ctx, key, "absent")
	if !errors.Is(err, celredis.ErrNil) {
		t.Fatalf("HGet missing field = %v, want ErrNil", err)
	}
}

func TestHash_HDel(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "h")
	cleanupKeys(t, c, key)

	_, _ = c.HSet(ctx, key, "a", "1", "b", "2", "c", "3")
	n, err := c.HDel(ctx, key, "a", "b", "nonexistent")
	if err != nil {
		t.Fatalf("HDel: %v", err)
	}
	if n != 2 {
		t.Fatalf("HDel = %d, want 2", n)
	}
}

func TestHash_HGetAll(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "h")
	cleanupKeys(t, c, key)

	_, _ = c.HSet(ctx, key, "a", "1", "b", "2", "c", "3")
	m, err := c.HGetAll(ctx, key)
	if err != nil {
		t.Fatalf("HGetAll: %v", err)
	}
	if len(m) != 3 {
		t.Fatalf("HGetAll len = %d, want 3", len(m))
	}
	if m["a"] != "1" || m["b"] != "2" || m["c"] != "3" {
		t.Fatalf("HGetAll = %v", m)
	}
}

func TestHash_HExists(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "h")
	cleanupKeys(t, c, key)

	_, _ = c.HSet(ctx, key, "present", "1")

	ok, err := c.HExists(ctx, key, "present")
	if err != nil {
		t.Fatalf("HExists present: %v", err)
	}
	if !ok {
		t.Fatalf("HExists present = false, want true")
	}
	ok, _ = c.HExists(ctx, key, "missing")
	if ok {
		t.Fatalf("HExists missing = true, want false")
	}
}

func TestHash_HKeysHVals(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "h")
	cleanupKeys(t, c, key)

	_, _ = c.HSet(ctx, key, "a", "1", "b", "2")
	keys, err := c.HKeys(ctx, key)
	if err != nil {
		t.Fatalf("HKeys: %v", err)
	}
	sort.Strings(keys)
	if len(keys) != 2 || keys[0] != "a" || keys[1] != "b" {
		t.Fatalf("HKeys = %v, want [a b]", keys)
	}
	vals, err := c.HVals(ctx, key)
	if err != nil {
		t.Fatalf("HVals: %v", err)
	}
	sort.Strings(vals)
	if len(vals) != 2 || vals[0] != "1" || vals[1] != "2" {
		t.Fatalf("HVals = %v, want [1 2]", vals)
	}
}
