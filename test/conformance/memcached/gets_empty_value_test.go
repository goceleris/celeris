//go:build memcached

package memcached_test

import (
	"testing"
	"time"

	celmc "github.com/goceleris/celeris/driver/memcached"
)

// TestGetsEmptyValue: Gets on an item whose value is empty returns the item
// (empty value, its flags and a non-zero CAS token) on the text and the binary
// protocol, and the token can be used to update it. The binary dispatcher
// used to append an item to a GET reply only when the value was non-empty, so
// Gets reported ErrCacheMiss for an item that exists.
func TestGetsEmptyValue(t *testing.T) {
	for _, proto := range []celmc.Protocol{celmc.ProtocolText, celmc.ProtocolBinary} {
		t.Run(proto.String(), func(t *testing.T) {
			c := newClient(t, celmc.WithProtocol(proto))
			ctx := ctxWithTimeout(t, 20*time.Second)
			key := uniqueKey(t, "gets_empty")
			cleanupKeys(t, c, key)
			if err := c.SetBytes(ctx, key, []byte{}, 0); err != nil {
				t.Fatalf("SetBytes(empty): %v", err)
			}
			it, err := c.Gets(ctx, key)
			if err != nil {
				t.Fatalf("Gets(empty value) = %v, want the item", err)
			}
			if len(it.Value) != 0 || it.CAS == 0 {
				t.Fatalf("Gets(empty value) = %d bytes, cas %d; want 0 bytes and a non-zero cas", len(it.Value), it.CAS)
			}
			ok, err := c.CASBytes(ctx, key, []byte("next"), it.CAS, 0)
			if err != nil || !ok {
				t.Fatalf("CASBytes with the token from Gets = %v, %v; want true", ok, err)
			}
			if got, err := c.Get(ctx, key); err != nil || got != "next" {
				t.Fatalf("Get after CAS = %q, %v; want next", got, err)
			}
		})
	}
}
