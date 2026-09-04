package ratelimit

import (
	"context"
	"testing"
	"time"
	"unsafe"
)

// The limiter retains KeyFunc's result as a map key. On the native engines
// that string is often a zero-copy view over the connection's read buffer
// (ClientIP from X-Forwarded-For, an API-key header, ...), which the next
// request on the conn overwrites. A retained alias then mutates under the
// map: the original client's bucket becomes unreachable (limit silently
// reset -- a fresh bucket per request) and a phantom bucket appears under
// whatever bytes now sit in the buffer. The limiter must own its keys.
func TestRetainedKeyIsNotAliasedToCallerBuffer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := time.Now().UnixNano()

	run := func(t *testing.T, allow func(key string, now int64) (bool, int, int64)) {
		buf := []byte("10.0.0.1")
		aliased := unsafe.String(&buf[0], len(buf)) // what the engine hands a middleware
		for i := 0; i < 3; i++ {
			if ok, _, _ := allow(aliased, now); !ok {
				t.Fatalf("request %d unexpectedly denied while filling the bucket", i)
			}
		}
		if ok, _, _ := allow(aliased, now); ok {
			t.Fatal("4th request must be denied (burst exhausted)")
		}
		// The connection buffer is reused: same bytes, different client.
		copy(buf, "10.9.9.9")
		// The ORIGINAL client, presented as an owned string, must still be
		// limited by its exhausted bucket.
		if ok, _, _ := allow("10.0.0.1", now); ok {
			t.Fatal("original key lost its bucket: the limiter retained an alias of the caller's buffer")
		}
		// And the bytes now in the buffer must NOT have inherited that
		// bucket (a phantom entry keyed by mutated memory).
		if ok, _, _ := allow("10.9.9.9", now); !ok {
			t.Fatal("a never-seen key was denied: a phantom bucket exists under the mutated bytes")
		}
	}
	t.Run("token_bucket", func(t *testing.T) {
		l := newShardedLimiter(ctx, 4, 0.000001, 3, time.Hour)
		run(t, l.allow)
	})
	t.Run("sliding_window", func(t *testing.T) {
		l := newSlidingWindowLimiter(ctx, 4, 0.000001, 3, time.Hour)
		run(t, l.allow)
	})
}
