//go:build redis

package redis_test

import (
	"sort"
	"testing"
	"time"
)

func TestSet_SAddSMembers(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "s")
	cleanupKeys(t, c, key)

	n, err := c.SAdd(ctx, key, "a", "b", "c")
	if err != nil {
		t.Fatalf("SAdd: %v", err)
	}
	if n != 3 {
		t.Fatalf("SAdd = %d, want 3", n)
	}
	// Idempotent re-add returns 0 new elements.
	n, _ = c.SAdd(ctx, key, "a")
	if n != 0 {
		t.Fatalf("SAdd existing = %d, want 0", n)
	}
	got, err := c.SMembers(ctx, key)
	if err != nil {
		t.Fatalf("SMembers: %v", err)
	}
	sort.Strings(got)
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("SMembers = %v, want [a b c]", got)
	}
}

func TestSet_SRem(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "s")
	cleanupKeys(t, c, key)

	_, _ = c.SAdd(ctx, key, "a", "b", "c")
	n, err := c.SRem(ctx, key, "a", "absent")
	if err != nil {
		t.Fatalf("SRem: %v", err)
	}
	if n != 1 {
		t.Fatalf("SRem = %d, want 1", n)
	}
}

func TestSet_SIsMember(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "s")
	cleanupKeys(t, c, key)

	_, _ = c.SAdd(ctx, key, "a")
	ok, err := c.SIsMember(ctx, key, "a")
	if err != nil {
		t.Fatalf("SIsMember present: %v", err)
	}
	if !ok {
		t.Fatalf("SIsMember present = false, want true")
	}
	ok, _ = c.SIsMember(ctx, key, "missing")
	if ok {
		t.Fatalf("SIsMember missing = true, want false")
	}
}

func TestSet_SCard(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "s")
	cleanupKeys(t, c, key)

	_, _ = c.SAdd(ctx, key, "a", "b", "c", "d")
	n, err := c.SCard(ctx, key)
	if err != nil {
		t.Fatalf("SCard: %v", err)
	}
	if n != 4 {
		t.Fatalf("SCard = %d, want 4", n)
	}
}
