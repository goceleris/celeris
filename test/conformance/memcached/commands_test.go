//go:build memcached

package memcached_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	celmc "github.com/goceleris/celeris/driver/memcached"
)

// TestSetGet_SmallValue covers the single-key Set + Get round trip and
// validates that typed Get returns the exact bytes we stored.
func TestSetGet_SmallValue(t *testing.T) {
	c := newClient(t)
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

// TestSetGet_TTL validates that a Set with a relative TTL expires on the
// server. We use a 1-second TTL and then wait 1.5s before re-reading.
func TestSetGet_TTL(t *testing.T) {
	c := newClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "ttl")
	cleanupKeys(t, c, key)

	if err := c.Set(ctx, key, "expiresoon", 1*time.Second); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// Immediately after Set the value should be present.
	got, err := c.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get pre-expire: %v", err)
	}
	if got != "expiresoon" {
		t.Fatalf("Get pre-expire = %q, want expiresoon", got)
	}
	// Wait for the entry to lapse. memcached's expiration check is lazy on
	// read, so a short window above 1s is sufficient.
	time.Sleep(1500 * time.Millisecond)
	_, err = c.Get(ctx, key)
	if !errors.Is(err, celmc.ErrCacheMiss) {
		t.Fatalf("Get post-expire = %v, want ErrCacheMiss", err)
	}
}

// TestGetMulti verifies that keys present and absent are correctly
// distinguished in the returned map. Missing keys MUST be absent, not
// mapped to an empty string.
func TestGetMulti(t *testing.T) {
	c := newClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)

	present := make([]string, 6)
	for i := range present {
		present[i] = uniqueKey(t, "multi_p")
	}
	missing := make([]string, 4)
	for i := range missing {
		missing[i] = uniqueKey(t, "multi_m")
	}
	all := append([]string{}, present...)
	all = append(all, missing...)
	cleanupKeys(t, c, all...)

	for i, k := range present {
		if err := c.Set(ctx, k, k+"=v"+string(rune('0'+i)), 0); err != nil {
			t.Fatalf("Set %s: %v", k, err)
		}
	}
	got, err := c.GetMulti(ctx, all...)
	if err != nil {
		t.Fatalf("GetMulti: %v", err)
	}
	if len(got) != len(present) {
		t.Fatalf("GetMulti len = %d, want %d", len(got), len(present))
	}
	for i, k := range present {
		want := k + "=v" + string(rune('0'+i))
		v, ok := got[k]
		if !ok {
			t.Fatalf("GetMulti missing present key %s", k)
		}
		if v != want {
			t.Fatalf("GetMulti[%s] = %q, want %q", k, v, want)
		}
	}
	for _, k := range missing {
		if _, ok := got[k]; ok {
			t.Fatalf("GetMulti: missing key %s unexpectedly present as %q", k, got[k])
		}
	}
}

// TestAdd covers the Add semantics: succeeds on a fresh key, fails with
// ErrNotStored when the key already exists.
func TestAdd(t *testing.T) {
	c := newClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "add")
	cleanupKeys(t, c, key)

	if err := c.Add(ctx, key, "first", 0); err != nil {
		t.Fatalf("Add first: %v", err)
	}
	err := c.Add(ctx, key, "second", 0)
	if !errors.Is(err, celmc.ErrNotStored) {
		t.Fatalf("Add second = %v, want ErrNotStored", err)
	}
	got, _ := c.Get(ctx, key)
	if got != "first" {
		t.Fatalf("Get after Add = %q, want first", got)
	}
}

// TestReplace covers Replace: fails on missing, succeeds on existing.
func TestReplace(t *testing.T) {
	c := newClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "replace")
	cleanupKeys(t, c, key)

	err := c.Replace(ctx, key, "ghost", 0)
	if !errors.Is(err, celmc.ErrNotStored) {
		t.Fatalf("Replace missing = %v, want ErrNotStored", err)
	}
	if err := c.Set(ctx, key, "original", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := c.Replace(ctx, key, "updated", 0); err != nil {
		t.Fatalf("Replace existing: %v", err)
	}
	got, _ := c.Get(ctx, key)
	if got != "updated" {
		t.Fatalf("Get after Replace = %q, want updated", got)
	}
}

// TestAppendPrepend covers Append and Prepend byte concatenation.
func TestAppendPrepend(t *testing.T) {
	c := newClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "concat")
	cleanupKeys(t, c, key)

	if err := c.Set(ctx, key, "middle", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := c.Append(ctx, key, "_end"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := c.Prepend(ctx, key, "start_"); err != nil {
		t.Fatalf("Prepend: %v", err)
	}
	got, err := c.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := "start_middle_end"
	if got != want {
		t.Fatalf("Get = %q, want %q", got, want)
	}

	// Append on a missing key fails with ErrNotStored.
	missing := uniqueKey(t, "concat_missing")
	err = c.Append(ctx, missing, "lonely")
	if !errors.Is(err, celmc.ErrNotStored) {
		t.Fatalf("Append missing = %v, want ErrNotStored", err)
	}
}

// TestCAS covers CAS semantics: stale token fails with ErrCASConflict;
// fresh token succeeds and advances the CAS.
func TestCAS(t *testing.T) {
	c := newClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "cas")
	cleanupKeys(t, c, key)

	if err := c.Set(ctx, key, "v0", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	item, err := c.Gets(ctx, key)
	if err != nil {
		t.Fatalf("Gets: %v", err)
	}
	if item.CAS == 0 {
		t.Fatalf("Gets returned zero CAS")
	}

	// Fresh CAS succeeds.
	ok, err := c.CAS(ctx, key, "v1", item.CAS, 0)
	if err != nil {
		t.Fatalf("CAS fresh: %v", err)
	}
	if !ok {
		t.Fatalf("CAS fresh returned !ok")
	}

	// Stale CAS (reuse old token) fails.
	ok, err = c.CAS(ctx, key, "v2", item.CAS, 0)
	if !errors.Is(err, celmc.ErrCASConflict) {
		t.Fatalf("CAS stale = (%v, %v), want ErrCASConflict", ok, err)
	}
	if ok {
		t.Fatalf("CAS stale returned ok=true on conflict")
	}

	got, _ := c.Get(ctx, key)
	if got != "v1" {
		t.Fatalf("Get = %q, want v1", got)
	}
}

// TestDelete covers the Delete command and its idempotency: after an initial
// successful delete, a repeat returns ErrCacheMiss.
func TestDelete(t *testing.T) {
	c := newClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "delete")
	cleanupKeys(t, c, key)

	if err := c.Set(ctx, key, "tombstone", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := c.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Idempotency check: a second delete returns ErrCacheMiss.
	err := c.Delete(ctx, key)
	if !errors.Is(err, celmc.ErrCacheMiss) {
		t.Fatalf("Delete second = %v, want ErrCacheMiss", err)
	}
}

// TestIncrDecr covers Incr and Decr: delta applies to a numeric body,
// ErrCacheMiss fires on a missing key, and Decr saturates at 0 per
// memcached semantics.
func TestIncrDecr(t *testing.T) {
	c := newClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "counter")
	cleanupKeys(t, c, key)

	// Missing key.
	_, err := c.Incr(ctx, key, 1)
	if !errors.Is(err, celmc.ErrCacheMiss) {
		t.Fatalf("Incr missing = %v, want ErrCacheMiss", err)
	}

	// Seed and increment.
	if err := c.Set(ctx, key, "10", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	n, err := c.Incr(ctx, key, 5)
	if err != nil {
		t.Fatalf("Incr: %v", err)
	}
	if n != 15 {
		t.Fatalf("Incr = %d, want 15", n)
	}
	n, err = c.Decr(ctx, key, 3)
	if err != nil {
		t.Fatalf("Decr: %v", err)
	}
	if n != 12 {
		t.Fatalf("Decr = %d, want 12", n)
	}

	// Decr saturates at 0 (memcached does not wrap).
	n, err = c.Decr(ctx, key, 1000)
	if err != nil {
		t.Fatalf("Decr saturate: %v", err)
	}
	if n != 0 {
		t.Fatalf("Decr saturate = %d, want 0", n)
	}
}

// TestTouch extends the TTL of an existing key. After Touch we verify
// the value is still present (it shouldn't have expired) and that
// touching a missing key returns ErrCacheMiss.
func TestTouch(t *testing.T) {
	c := newClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "touch")
	cleanupKeys(t, c, key)

	if err := c.Set(ctx, key, "alive", 1*time.Second); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := c.Touch(ctx, key, 60*time.Second); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	// Sleep past the original TTL; Touch should have extended it.
	time.Sleep(1500 * time.Millisecond)
	got, err := c.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get after Touch: %v", err)
	}
	if got != "alive" {
		t.Fatalf("Get = %q, want alive", got)
	}

	// Touch on a missing key returns ErrCacheMiss.
	missing := uniqueKey(t, "touch_missing")
	err = c.Touch(ctx, missing, 60*time.Second)
	if !errors.Is(err, celmc.ErrCacheMiss) {
		t.Fatalf("Touch missing = %v, want ErrCacheMiss", err)
	}
}

// TestStats verifies that Stats returns a non-empty map containing at least
// one well-known field like "pid" or "version".
func TestStats(t *testing.T) {
	c := newClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)

	stats, err := c.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if len(stats) == 0 {
		t.Fatal("Stats: empty map")
	}
	// memcached always reports at least these keys.
	found := false
	for _, want := range []string{"pid", "version", "uptime"} {
		if _, ok := stats[want]; ok {
			found = true
			break
		}
	}
	if !found {
		// Dump a sample of keys to aid debugging.
		var sample []string
		for k := range stats {
			sample = append(sample, k)
			if len(sample) >= 5 {
				break
			}
		}
		t.Fatalf("Stats missing any of pid/version/uptime; sample keys: %v", sample)
	}
}

// TestVersion returns a non-empty version string.
func TestVersion(t *testing.T) {
	c := newClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)

	v, err := c.Version(ctx)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if strings.TrimSpace(v) == "" {
		t.Fatal("Version returned empty string")
	}
}

// TestPing returns nil on a healthy server.
func TestPing(t *testing.T) {
	c := newClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)

	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

// TestGet_ContextCanceled ensures that a canceled context short-circuits the
// driver rather than hanging waiting for a server reply.
func TestGet_ContextCanceled(t *testing.T) {
	c := newClient(t)
	key := uniqueKey(t, "ctxcancel")
	cleanupKeys(t, c, key)

	// Seed so a live Get would otherwise succeed.
	setupCtx, cancelSetup := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelSetup()
	if err := c.Set(setupCtx, key, "v", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := c.Get(ctx, key)
	if err == nil {
		t.Fatalf("Get with cancelled ctx = nil, want non-nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("Get with cancelled ctx hung for %v", time.Since(start))
	}
}
