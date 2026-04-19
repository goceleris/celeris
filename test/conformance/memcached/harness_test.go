//go:build memcached

// Package memcached_test exercises the celeris Memcached driver against a real
// server. It is gated by both a `memcached` build tag and the
// CELERIS_MEMCACHED_ADDR environment variable; with either missing the tests
// skip cleanly so `go test ./...` remains green on a vanilla checkout.
//
// To run:
//
//	CELERIS_MEMCACHED_ADDR=127.0.0.1:11211 \
//	  go test -tags memcached ./test/conformance/memcached/...
//
// See docker-compose.yml in this directory for a disposable server.
package memcached_test

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

	celmc "github.com/goceleris/celeris/driver/memcached"
)

const (
	// envAddr is the env var holding the server address. Tests skip if empty.
	envAddr = "CELERIS_MEMCACHED_ADDR"
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
		t.Skipf("skipping: %s not set (set it to a reachable memcached address to run)", envAddr)
	}
	return addr
}

// clientOpts returns the default option set used by newClient.
func clientOpts() []celmc.Option {
	return []celmc.Option{
		celmc.WithDialTimeout(3 * time.Second),
		celmc.WithTimeout(3 * time.Second),
	}
}

// newClient dials a *Client, pings it, and registers cleanup. Additional
// options override the defaults.
func newClient(t *testing.T, extra ...celmc.Option) *celmc.Client {
	t.Helper()
	addr := addrFromEnv(t)
	opts := append(clientOpts(), extra...)
	c, err := celmc.NewClient(addr, opts...)
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
// Memcached keys are limited to 250 bytes with no whitespace/control bytes;
// this helper emits ASCII-safe keys well under the cap.
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
	return fmt.Sprintf("celeris_test_%08x_%d_%s_%s", nameHash, n, hex.EncodeToString(buf), label)
}

// sanitizeLabel normalizes a human label into a key-safe suffix (memcached
// disallows whitespace and control bytes inside keys).
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

// cleanupKeys schedules a Delete of each key on test cleanup. Errors are
// swallowed — the goal is to tidy, not to assert.
func cleanupKeys(t *testing.T, c *celmc.Client, keys ...string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, k := range keys {
			_ = c.Delete(ctx, k)
		}
	})
}

// ctxWithTimeout returns a context bounded to d that is cancelled on test exit.
func ctxWithTimeout(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}
