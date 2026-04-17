//go:build memcached

package memcached_test

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"errors"
	"testing"
	"time"

	celmc "github.com/goceleris/celeris/driver/memcached"
)

// TestProtocol_TextBinary runs the same set of assertions against both the
// text and binary protocols so we catch dialect-specific regressions.
func TestProtocol_TextBinary(t *testing.T) {
	cases := []struct {
		name  string
		proto celmc.Protocol
	}{
		{"text", celmc.ProtocolText},
		{"binary", celmc.ProtocolBinary},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(t, celmc.WithProtocol(tc.proto))
			if c.Protocol() != tc.proto {
				t.Fatalf("Protocol() = %v, want %v", c.Protocol(), tc.proto)
			}
			ctx := ctxWithTimeout(t, 5*time.Second)

			// --- Set + Get round-trip with binary-safe payload ---
			key := uniqueKey(t, "proto_rt")
			cleanupKeys(t, c, key)
			payload := make([]byte, 256)
			if _, err := crand.Read(payload); err != nil {
				t.Fatalf("rand: %v", err)
			}
			if err := c.Set(ctx, key, payload, 0); err != nil {
				t.Fatalf("Set: %v", err)
			}
			got, err := c.GetBytes(ctx, key)
			if err != nil {
				t.Fatalf("GetBytes: %v", err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("GetBytes binary mismatch (%d vs %d bytes)", len(got), len(payload))
			}

			// --- Add then Add again (miss-semantics error) ---
			// Text returns ErrNotStored; binary returns ErrCASConflict on
			// StatusKeyExists. Both are acceptable "duplicate-key" signals.
			addKey := uniqueKey(t, "proto_add")
			cleanupKeys(t, c, addKey)
			if err := c.Add(ctx, addKey, "first", 0); err != nil {
				t.Fatalf("Add first: %v", err)
			}
			err = c.Add(ctx, addKey, "second", 0)
			if !errors.Is(err, celmc.ErrNotStored) && !errors.Is(err, celmc.ErrCASConflict) {
				t.Fatalf("Add dup = %v, want ErrNotStored or ErrCASConflict", err)
			}

			// --- Delete idempotency ---
			if err := c.Delete(ctx, addKey); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if err := c.Delete(ctx, addKey); !errors.Is(err, celmc.ErrCacheMiss) {
				t.Fatalf("Delete twice = %v, want ErrCacheMiss", err)
			}

			// --- Incr / Decr ---
			counterKey := uniqueKey(t, "proto_counter")
			cleanupKeys(t, c, counterKey)
			if err := c.Set(ctx, counterKey, "100", 0); err != nil {
				t.Fatalf("Set counter: %v", err)
			}
			n, err := c.Incr(ctx, counterKey, 7)
			if err != nil {
				t.Fatalf("Incr: %v", err)
			}
			if n != 107 {
				t.Fatalf("Incr = %d, want 107", n)
			}
			n, err = c.Decr(ctx, counterKey, 7)
			if err != nil {
				t.Fatalf("Decr: %v", err)
			}
			if n != 100 {
				t.Fatalf("Decr = %d, want 100", n)
			}

			// --- Version + Ping ---
			if _, err := c.Version(ctx); err != nil {
				t.Fatalf("Version: %v", err)
			}
			if err := c.Ping(ctx); err != nil {
				t.Fatalf("Ping: %v", err)
			}

			// --- Stats round-trip (driver dials a text conn when binary) ---
			stats, err := c.Stats(ctx)
			if err != nil {
				t.Fatalf("Stats: %v", err)
			}
			if len(stats) == 0 {
				t.Fatal("Stats returned empty map")
			}
		})
	}
}

// TestProtocol_Replace_Missing checks that Replace's miss semantics agree
// across both protocols.
func TestProtocol_Replace_Missing(t *testing.T) {
	for _, proto := range []celmc.Protocol{celmc.ProtocolText, celmc.ProtocolBinary} {
		proto := proto
		t.Run(proto.String(), func(t *testing.T) {
			c := newClient(t, celmc.WithProtocol(proto))
			ctx := ctxWithTimeout(t, 5*time.Second)
			key := uniqueKey(t, "proto_rep_missing")

			// Text: NOT_STORED -> ErrNotStored. Binary: StatusKeyNotFound ->
			// ErrCacheMiss. Both are semantically equivalent for Replace-on-
			// missing; accept either.
			err := c.Replace(ctx, key, "no", 0)
			if !errors.Is(err, celmc.ErrNotStored) && !errors.Is(err, celmc.ErrCacheMiss) {
				t.Fatalf("Replace missing = %v, want ErrNotStored or ErrCacheMiss", err)
			}
		})
	}
}

// TestProtocol_GetMulti_MixedHitMiss ensures that GetMulti returns only the
// hits (not empty-string placeholders) across both dialects.
func TestProtocol_GetMulti_MixedHitMiss(t *testing.T) {
	for _, proto := range []celmc.Protocol{celmc.ProtocolText, celmc.ProtocolBinary} {
		proto := proto
		t.Run(proto.String(), func(t *testing.T) {
			c := newClient(t, celmc.WithProtocol(proto))
			ctx := ctxWithTimeout(t, 5*time.Second)

			present := []string{uniqueKey(t, "mh_a"), uniqueKey(t, "mh_b")}
			missing := []string{uniqueKey(t, "mh_x"), uniqueKey(t, "mh_y")}
			cleanupKeys(t, c, append(append([]string{}, present...), missing...)...)

			for i, k := range present {
				if err := c.Set(ctx, k, "v"+string(rune('0'+i)), 0); err != nil {
					t.Fatalf("Set %s: %v", k, err)
				}
			}
			all := append([]string{}, present...)
			all = append(all, missing...)
			got, err := c.GetMulti(ctx, all...)
			if err != nil {
				t.Fatalf("GetMulti: %v", err)
			}
			if len(got) != len(present) {
				t.Fatalf("GetMulti len = %d, want %d", len(got), len(present))
			}
			for _, k := range missing {
				if _, ok := got[k]; ok {
					t.Fatalf("GetMulti has missing key %s (value %q)", k, got[k])
				}
			}
		})
	}
}

// TestProtocol_LargeValue stores a 512 KiB payload under both dialects to
// validate streamed reads / writes. Keep it well under memcached's default
// -I cap (1 MiB) so the test doesn't need a custom server flag.
func TestProtocol_LargeValue(t *testing.T) {
	for _, proto := range []celmc.Protocol{celmc.ProtocolText, celmc.ProtocolBinary} {
		proto := proto
		t.Run(proto.String(), func(t *testing.T) {
			c := newClient(t, celmc.WithProtocol(proto))
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			key := uniqueKey(t, "proto_large")
			cleanupKeys(t, c, key)

			payload := make([]byte, 512*1024)
			if _, err := crand.Read(payload); err != nil {
				t.Fatalf("rand: %v", err)
			}
			if err := c.Set(ctx, key, payload, 0); err != nil {
				t.Fatalf("Set: %v", err)
			}
			got, err := c.GetBytes(ctx, key)
			if err != nil {
				t.Fatalf("GetBytes: %v", err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("large payload mismatch (%d vs %d bytes)", len(got), len(payload))
			}
		})
	}
}
