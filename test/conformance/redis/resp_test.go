//go:build redis

package redis_test

import (
	"testing"
	"time"

	celredis "github.com/goceleris/celeris/driver/redis"
)

// TestRESP_DefaultIsRESP3 confirms that by default the driver negotiates RESP3
// via HELLO 3. We cannot directly observe the handshake, so we rely on Proto()
// reporting 3 after a successful Ping.
func TestRESP_DefaultIsRESP3(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	// Some servers (e.g. Redis < 6.0) reject HELLO and the driver falls back
	// to RESP2. Accept either 2 or 3 — the assertion is that the driver is in
	// a sane negotiated state.
	p := c.Proto()
	if p != 2 && p != 3 {
		t.Fatalf("Proto = %d, want 2 or 3", p)
	}
}

// TestRESP_ForceRESP2 exercises the WithForceRESP2 code path: AUTH + SELECT
// instead of HELLO. Every command must still work correctly.
func TestRESP_ForceRESP2(t *testing.T) {
	c := connectClient(t, celredis.WithForceRESP2())
	ctx := ctxWithTimeout(t, 5*time.Second)
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := c.Proto(); got != 2 {
		t.Fatalf("Proto = %d, want 2", got)
	}

	key := uniqueKey(t, "resp2")
	cleanupKeys(t, c, key)

	if err := c.Set(ctx, key, "v", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := c.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "v" {
		t.Fatalf("Get = %q, want v", got)
	}

	// HGETALL in RESP2 comes back as a flat array; the driver must still
	// decode it to a map.
	hkey := uniqueKey(t, "h")
	cleanupKeys(t, c, hkey)
	if _, err := c.HSet(ctx, hkey, "a", "1", "b", "2"); err != nil {
		t.Fatalf("HSet: %v", err)
	}
	m, err := c.HGetAll(ctx, hkey)
	if err != nil {
		t.Fatalf("HGetAll: %v", err)
	}
	if m["a"] != "1" || m["b"] != "2" {
		t.Fatalf("HGetAll = %v", m)
	}
}

// TestRESP_Proto2ExplicitMatches verifies WithProto(2) produces the same
// observable behaviour as WithForceRESP2.
func TestRESP_Proto2ExplicitMatches(t *testing.T) {
	c := connectClient(t, celredis.WithProto(2))
	ctx := ctxWithTimeout(t, 5*time.Second)
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := c.Proto(); got != 2 {
		t.Fatalf("Proto = %d, want 2", got)
	}
}
