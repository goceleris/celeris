//go:build redisspec

package redisspec

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/goceleris/celeris/driver/redis/protocol"
)

var keySeq uint64

// uniqueKey returns a collision-free key for the test.
func uniqueKey(t *testing.T, label string) string {
	t.Helper()
	n := atomic.AddUint64(&keySeq, 1)
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("redisspec:%s:%d:%s", label, n, hex.EncodeToString(b))
}

// cleanupKey schedules a DEL of the given key on test cleanup.
func cleanupKey(t *testing.T, c *redisRawConn, keys ...string) {
	t.Helper()
	t.Cleanup(func() {
		args := append([]string{"DEL"}, keys...)
		c.Send(args...)
		// Drain the response; ignore errors on cleanup.
		c.ReadValueTimeout(t, defaultReadTimeout)
	})
}

// flushTestDB sends FLUSHDB to reset state for a test. Used sparingly.
func flushTestDB(t *testing.T, c *redisRawConn) {
	t.Helper()
	c.Send("FLUSHDB")
	v := c.ReadValue(t)
	if v.Type == protocol.TyError {
		t.Fatalf("FLUSHDB: %s", v.Str)
	}
}

// ExpectSimple asserts the next value is a simple string matching expected.
func (c *redisRawConn) ExpectSimple(t *testing.T, expected string) {
	t.Helper()
	v := c.ReadValue(t)
	if v.Type != protocol.TySimple {
		t.Fatalf("expected simple string, got %v: %q", v.Type, v.Str)
	}
	if string(v.Str) != expected {
		t.Fatalf("expected %q, got %q", expected, v.Str)
	}
}

// ExpectError asserts the next value is an error whose message starts with prefix.
func (c *redisRawConn) ExpectError(t *testing.T, prefix string) {
	t.Helper()
	v := c.ReadValue(t)
	if v.Type != protocol.TyError {
		t.Fatalf("expected error, got %v: %+v", v.Type, v)
	}
	if !strings.HasPrefix(string(v.Str), prefix) {
		t.Fatalf("error %q does not start with %q", v.Str, prefix)
	}
}

// ExpectInt asserts the next value is an integer matching expected.
func (c *redisRawConn) ExpectInt(t *testing.T, expected int64) {
	t.Helper()
	v := c.ReadValue(t)
	if v.Type != protocol.TyInt {
		t.Fatalf("expected integer, got %v: %+v", v.Type, v)
	}
	if v.Int != expected {
		t.Fatalf("expected :%d, got :%d", expected, v.Int)
	}
}

// ExpectBulk asserts the next value is a bulk string matching expected.
func (c *redisRawConn) ExpectBulk(t *testing.T, expected string) {
	t.Helper()
	v := c.ReadValue(t)
	if v.Type != protocol.TyBulk {
		t.Fatalf("expected bulk string, got %v: %+v", v.Type, v)
	}
	if string(v.Str) != expected {
		t.Fatalf("expected %q, got %q", expected, v.Str)
	}
}

// ExpectBulkBytes asserts the next value is a bulk string matching expected bytes.
func (c *redisRawConn) ExpectBulkBytes(t *testing.T, expected []byte) {
	t.Helper()
	v := c.ReadValue(t)
	if v.Type != protocol.TyBulk {
		t.Fatalf("expected bulk string, got %v: %+v", v.Type, v)
	}
	if len(v.Str) != len(expected) {
		t.Fatalf("bulk length %d, want %d", len(v.Str), len(expected))
	}
	for i := range expected {
		if v.Str[i] != expected[i] {
			t.Fatalf("bulk differs at byte %d: got 0x%02x, want 0x%02x", i, v.Str[i], expected[i])
		}
	}
}

// ExpectNull asserts the next value is null (RESP2 $-1 / *-1 or RESP3 _).
func (c *redisRawConn) ExpectNull(t *testing.T) {
	t.Helper()
	v := c.ReadValue(t)
	if v.Type != protocol.TyNull {
		t.Fatalf("expected null, got %v: %+v", v.Type, v)
	}
}

// ExpectArray reads the next value, asserts it is an array with n elements,
// and returns the elements.
func (c *redisRawConn) ExpectArray(t *testing.T, n int) []protocol.Value {
	t.Helper()
	v := c.ReadValue(t)
	if v.Type != protocol.TyArray {
		t.Fatalf("expected array, got %v: %+v", v.Type, v)
	}
	if len(v.Array) != n {
		t.Fatalf("expected array of %d, got %d", n, len(v.Array))
	}
	return v.Array
}

// ExpectOK is shorthand for ExpectSimple(t, "OK").
func (c *redisRawConn) ExpectOK(t *testing.T) {
	t.Helper()
	c.ExpectSimple(t, "OK")
}

// ExpectQueued is shorthand for ExpectSimple(t, "QUEUED").
func (c *redisRawConn) ExpectQueued(t *testing.T) {
	t.Helper()
	c.ExpectSimple(t, "QUEUED")
}

// ExpectType reads the next value and returns it, asserting only that there
// was no read error. Callers inspect the type themselves.
func (c *redisRawConn) ExpectType(t *testing.T, ty protocol.Type) protocol.Value {
	t.Helper()
	v := c.ReadValue(t)
	if v.Type != ty {
		t.Fatalf("expected type %v, got %v: %+v", ty, v.Type, v)
	}
	return v
}
