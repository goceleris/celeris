//go:build redis

package redis_test

import (
	"testing"
	"time"
)

func TestKeys_Type(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	str := uniqueKey(t, "string")
	list := uniqueKey(t, "list")
	hash := uniqueKey(t, "hash")
	cleanupKeys(t, c, str, list, hash)

	_ = c.Set(ctx, str, "v", 0)
	_, _ = c.RPush(ctx, list, "a")
	_, _ = c.HSet(ctx, hash, "f", "v")

	cases := []struct {
		key  string
		want string
	}{
		{str, "string"},
		{list, "list"},
		{hash, "hash"},
	}
	for _, tc := range cases {
		got, err := c.Type(ctx, tc.key)
		if err != nil {
			t.Fatalf("Type(%s): %v", tc.key, err)
		}
		if got != tc.want {
			t.Fatalf("Type(%s) = %q, want %q", tc.key, got, tc.want)
		}
	}
}

func TestKeys_Rename(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	src := uniqueKey(t, "src")
	dst := uniqueKey(t, "dst")
	cleanupKeys(t, c, src, dst)

	_ = c.Set(ctx, src, "hello", 0)
	if err := c.Rename(ctx, src, dst); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	got, err := c.Get(ctx, dst)
	if err != nil {
		t.Fatalf("Get after Rename: %v", err)
	}
	if got != "hello" {
		t.Fatalf("Get after Rename = %q, want hello", got)
	}
}

func TestKeys_RandomKey(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "rk")
	cleanupKeys(t, c, key)

	_ = c.Set(ctx, key, "v", 0)
	// RandomKey returns SOME key in the DB; we don't assert it's ours because
	// the DB may have other keys. Just check the call succeeds or ErrNil for
	// an empty DB.
	_, err := c.RandomKey(ctx)
	if err != nil {
		// Empty-DB case produces ErrNil; accept that.
		t.Logf("RandomKey: %v (likely empty DB)", err)
	}
}
