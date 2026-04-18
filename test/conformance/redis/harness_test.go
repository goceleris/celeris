//go:build redis

// Package redis_test exercises the celeris Redis driver against a real server.
// It is gated by both a `redis` build tag and the CELERIS_REDIS_ADDR environment
// variable; with either missing the tests skip cleanly so `go test ./...`
// remains green on a vanilla checkout.
//
// To run:
//
//	CELERIS_REDIS_ADDR=127.0.0.1:6379 \
//	  go test -tags redis ./test/conformance/redis/...
//
// Tests that require AUTH also honour CELERIS_REDIS_PASSWORD. See
// docker-compose.yml in this directory for a disposable server.
package redis_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	celredis "github.com/goceleris/celeris/driver/redis"
)

const (
	// envAddr is the env var holding the server address. Tests skip if empty.
	envAddr = "CELERIS_REDIS_ADDR"
	// envPassword is the env var holding the AUTH password. Tests that require
	// authentication skip if empty.
	envPassword = "CELERIS_REDIS_PASSWORD"
)

// keyCounter gives each generated key a monotonic suffix so parallel subtests
// never collide even with the same random prefix.
var keyCounter uint64

// addrFromEnv returns the address or skips. It is the standard gate at the top
// of every conformance test.
func addrFromEnv(t *testing.T) string {
	t.Helper()
	addr := strings.TrimSpace(os.Getenv(envAddr))
	if addr == "" {
		t.Skipf("skipping: %s not set (set it to a reachable redis address to run)", envAddr)
	}
	return addr
}

// passwordFromEnv returns the password, or skips the test if unset. Use this
// at the top of AUTH-specific tests only; plain tests should let connectClient
// pick up the password opportunistically.
func passwordFromEnv(t *testing.T) string {
	t.Helper()
	pw := os.Getenv(envPassword)
	if pw == "" {
		t.Skipf("skipping: %s not set", envPassword)
	}
	return pw
}

// clientOpts returns the default options used by connectClient. Caller may
// append more options.
func clientOpts() []celredis.Option {
	var opts []celredis.Option
	if pw := os.Getenv(envPassword); pw != "" {
		opts = append(opts, celredis.WithPassword(pw))
	}
	opts = append(opts,
		celredis.WithDialTimeout(3*time.Second),
		celredis.WithReadTimeout(3*time.Second),
		celredis.WithWriteTimeout(3*time.Second),
	)
	return opts
}

// connectClient dials a Client, pings it, and registers cleanup.
func connectClient(t *testing.T, extra ...celredis.Option) *celredis.Client {
	t.Helper()
	addr := addrFromEnv(t)
	opts := append(clientOpts(), extra...)
	c, err := celredis.NewClient(addr, opts...)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		_ = c.Close()
		t.Skipf("skipping: PING failed (server unreachable?): %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// uniqueKey returns a collision-free key scoped to a test. It folds in a hash
// of the test's full name, the process-unique counter, and random bytes.
func uniqueKey(t *testing.T, label string) string {
	t.Helper()
	h := fnv.New32a()
	_, _ = h.Write([]byte(t.Name()))
	nameHash := h.Sum32()

	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	n := atomic.AddUint64(&keyCounter, 1)
	label = sanitizeLabel(label)
	return fmt.Sprintf("celeris:test:%08x:%d:%s:%s", nameHash, n, hex.EncodeToString(buf), label)
}

// sanitizeLabel normalizes a human label into a key-safe suffix.
func sanitizeLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "k"
	}
	return b.String()
}

// cleanupKeys schedules a DEL against the given keys on test cleanup.
func cleanupKeys(t *testing.T, c *celredis.Client, keys ...string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = c.Del(ctx, keys...)
	})
}

// ctxWithTimeout returns a context bounded to d that is cancelled on test exit.
func ctxWithTimeout(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}
