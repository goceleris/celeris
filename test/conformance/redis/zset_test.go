//go:build redis

package redis_test

import (
	"testing"
	"time"

	celredis "github.com/goceleris/celeris/driver/redis"
)

func TestZSet_ZAddZRange(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "z")
	cleanupKeys(t, c, key)

	n, err := c.ZAdd(ctx, key,
		celredis.Z{Score: 1, Member: "a"},
		celredis.Z{Score: 2, Member: "b"},
		celredis.Z{Score: 3, Member: "c"},
	)
	if err != nil {
		t.Fatalf("ZAdd: %v", err)
	}
	if n != 3 {
		t.Fatalf("ZAdd = %d, want 3", n)
	}
	got, err := c.ZRange(ctx, key, 0, -1)
	if err != nil {
		t.Fatalf("ZRange: %v", err)
	}
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("ZRange = %v, want [a b c]", got)
	}
}

func TestZSet_ZRangeByScore(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "z")
	cleanupKeys(t, c, key)

	_, _ = c.ZAdd(ctx, key,
		celredis.Z{Score: 1, Member: "a"},
		celredis.Z{Score: 2, Member: "b"},
		celredis.Z{Score: 3, Member: "c"},
		celredis.Z{Score: 4, Member: "d"},
	)
	got, err := c.ZRangeByScore(ctx, key, &celredis.ZRangeBy{Min: "2", Max: "3"})
	if err != nil {
		t.Fatalf("ZRangeByScore: %v", err)
	}
	if len(got) != 2 || got[0] != "b" || got[1] != "c" {
		t.Fatalf("ZRangeByScore = %v, want [b c]", got)
	}
	// With LIMIT.
	got, err = c.ZRangeByScore(ctx, key, &celredis.ZRangeBy{Min: "-inf", Max: "+inf", Offset: 1, Count: 2})
	if err != nil {
		t.Fatalf("ZRangeByScore LIMIT: %v", err)
	}
	if len(got) != 2 || got[0] != "b" || got[1] != "c" {
		t.Fatalf("ZRangeByScore LIMIT = %v, want [b c]", got)
	}
}

func TestZSet_ZRem(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "z")
	cleanupKeys(t, c, key)

	_, _ = c.ZAdd(ctx, key,
		celredis.Z{Score: 1, Member: "a"},
		celredis.Z{Score: 2, Member: "b"},
	)
	n, err := c.ZRem(ctx, key, "a", "absent")
	if err != nil {
		t.Fatalf("ZRem: %v", err)
	}
	if n != 1 {
		t.Fatalf("ZRem = %d, want 1", n)
	}
}

func TestZSet_ZScore(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "z")
	cleanupKeys(t, c, key)

	_, _ = c.ZAdd(ctx, key, celredis.Z{Score: 42.5, Member: "m"})
	s, err := c.ZScore(ctx, key, "m")
	if err != nil {
		t.Fatalf("ZScore: %v", err)
	}
	if s != 42.5 {
		t.Fatalf("ZScore = %v, want 42.5", s)
	}
}

func TestZSet_ZCard(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "z")
	cleanupKeys(t, c, key)

	_, _ = c.ZAdd(ctx, key,
		celredis.Z{Score: 1, Member: "a"},
		celredis.Z{Score: 2, Member: "b"},
		celredis.Z{Score: 3, Member: "c"},
	)
	n, err := c.ZCard(ctx, key)
	if err != nil {
		t.Fatalf("ZCard: %v", err)
	}
	if n != 3 {
		t.Fatalf("ZCard = %d, want 3", n)
	}
}
