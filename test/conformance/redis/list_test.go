//go:build redis

package redis_test

import (
	"errors"
	"testing"
	"time"

	celredis "github.com/goceleris/celeris/driver/redis"
)

func TestList_LPushRPush(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "l")
	cleanupKeys(t, c, key)

	n, err := c.RPush(ctx, key, "a", "b", "c")
	if err != nil {
		t.Fatalf("RPush: %v", err)
	}
	if n != 3 {
		t.Fatalf("RPush = %d, want 3", n)
	}
	n, err = c.LPush(ctx, key, "z")
	if err != nil {
		t.Fatalf("LPush: %v", err)
	}
	if n != 4 {
		t.Fatalf("LPush = %d, want 4", n)
	}
	got, err := c.LRange(ctx, key, 0, -1)
	if err != nil {
		t.Fatalf("LRange: %v", err)
	}
	want := []string{"z", "a", "b", "c"}
	if len(got) != 4 || got[0] != want[0] || got[3] != want[3] {
		t.Fatalf("LRange = %v, want %v", got, want)
	}
}

func TestList_LPopRPop(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "l")
	cleanupKeys(t, c, key)

	_, _ = c.RPush(ctx, key, "a", "b", "c")
	s, err := c.LPop(ctx, key)
	if err != nil {
		t.Fatalf("LPop: %v", err)
	}
	if s != "a" {
		t.Fatalf("LPop = %q, want a", s)
	}
	s, err = c.RPop(ctx, key)
	if err != nil {
		t.Fatalf("RPop: %v", err)
	}
	if s != "c" {
		t.Fatalf("RPop = %q, want c", s)
	}
}

func TestList_PopEmpty(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "empty")

	_, err := c.LPop(ctx, key)
	if !errors.Is(err, celredis.ErrNil) {
		t.Fatalf("LPop empty = %v, want ErrNil", err)
	}
}

func TestList_LLen(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "l")
	cleanupKeys(t, c, key)

	_, _ = c.RPush(ctx, key, "a", "b", "c", "d", "e")
	n, err := c.LLen(ctx, key)
	if err != nil {
		t.Fatalf("LLen: %v", err)
	}
	if n != 5 {
		t.Fatalf("LLen = %d, want 5", n)
	}
}

func TestList_LRangeSubslice(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "l")
	cleanupKeys(t, c, key)

	_, _ = c.RPush(ctx, key, "a", "b", "c", "d", "e")
	got, err := c.LRange(ctx, key, 1, 3)
	if err != nil {
		t.Fatalf("LRange: %v", err)
	}
	want := []string{"b", "c", "d"}
	if len(got) != 3 || got[0] != want[0] || got[2] != want[2] {
		t.Fatalf("LRange = %v, want %v", got, want)
	}
}
