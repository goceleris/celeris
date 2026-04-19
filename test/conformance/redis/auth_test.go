//go:build redis

package redis_test

import (
	"testing"
	"time"

	celredis "github.com/goceleris/celeris/driver/redis"
)

// TestAuth_PasswordSuccess validates that the default connect path (which
// applies CELERIS_REDIS_PASSWORD via clientOpts) authenticates successfully
// when the server requires it.
func TestAuth_PasswordSuccess(t *testing.T) {
	_ = passwordFromEnv(t) // skip unless password set

	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

// TestAuth_PasswordFailure validates that an incorrect password fails on the
// first command (HELLO/AUTH pre-flight).
func TestAuth_PasswordFailure(t *testing.T) {
	_ = passwordFromEnv(t) // skip unless password set
	addr := addrFromEnv(t)

	c, err := celredis.NewClient(addr,
		celredis.WithPassword("definitely-not-the-password"),
		celredis.WithDialTimeout(3*time.Second),
	)
	if err != nil {
		// Some drivers error at NewClient; ours defers to first command.
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = c.Close() }()
	ctx := ctxWithTimeout(t, 5*time.Second)
	if err := c.Ping(ctx); err == nil {
		t.Fatalf("Ping with bad password succeeded, want auth error")
	}
}

// TestAuth_ForceRESP2WithPassword forces the RESP2 codepath (plain AUTH, no
// HELLO) and confirms the password is still applied correctly.
func TestAuth_ForceRESP2WithPassword(t *testing.T) {
	_ = passwordFromEnv(t) // skip unless password set

	c := connectClient(t, celredis.WithForceRESP2())
	ctx := ctxWithTimeout(t, 5*time.Second)
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}
