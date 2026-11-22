//go:build memcached

package memcached_test

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	celmc "github.com/goceleris/celeris/driver/memcached"
)

// This file reads back what the memcached driver writes, and what a server
// holds that the driver did not write (item flags), across the two wire
// protocols: values the framing could confuse (CRLF, END, an empty value,
// 1 MiB), the value and key size limits, counters at the uint64 edges, the
// error each command gives for a missing key, flags and CAS tokens, and a
// multi-get that mixes hits, misses and large values. The memcached Driver
// Conformance run (drivers.yml, 1.6.29, 1.6.36, 1.6.41) executes it.

var mcProtocols = []celmc.Protocol{celmc.ProtocolText, celmc.ProtocolBinary}

// rawText runs one text-protocol request on its own connection and returns the
// server's first line. It is how a test stores a flag the client API cannot set.
func rawText(t *testing.T, req string) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addrFromEnv(t), 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write: %v", err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return strings.TrimSpace(line)
}

// TestRoundTripValues: byte-exact values, including the ones that look like
// protocol, and the server's size limit.
func TestRoundTripValues(t *testing.T) {
	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}
	framing := append(append([]byte{}, all...), "\r\nEND\r\nVALUE x 0 1\r\nz\r\nSTORED\r\n"...)
	values := []struct {
		name string
		v    []byte
	}{
		{"empty", []byte{}},
		{"one_byte", []byte{0}},
		{"all_bytes_and_framing", framing},
		{"trailing_crlf", []byte("line\r\n")},
		{"1000000", bytes.Repeat([]byte("a"), 1000000)},
		{"1MiB_minus_512", bytes.Repeat([]byte("b"), 1<<20-512)}, // largest the default -I 1m item size takes
	}
	tooLarge := []struct {
		name string
		n    int
	}{{"1MiB", 1 << 20}, {"2MiB", 2 << 20}}

	for _, proto := range mcProtocols {
		t.Run(proto.String(), func(t *testing.T) {
			c := newClient(t, celmc.WithProtocol(proto))
			ctx := ctxWithTimeout(t, 60*time.Second)
			key := uniqueKey(t, "val")
			cleanupKeys(t, c, key)
			for _, tc := range values {
				t.Run(tc.name, func(t *testing.T) {
					if err := c.SetBytes(ctx, key, tc.v, 0); err != nil {
						t.Fatalf("SetBytes: %v", err)
					}
					got, err := c.GetBytes(ctx, key)
					if err != nil || !bytes.Equal(got, tc.v) {
						t.Fatalf("GetBytes: %d bytes, %v; want %d bytes", len(got), err, len(tc.v))
					}
					s, err := c.Get(ctx, key)
					if err != nil || s != string(tc.v) {
						t.Fatalf("Get: %d bytes, %v; want %d bytes", len(s), err, len(tc.v))
					}
					m, err := c.GetMultiBytes(ctx, key, key+"_missing")
					if err != nil || len(m) != 1 || !bytes.Equal(m[key], tc.v) {
						t.Fatalf("GetMultiBytes: %d entries, %v", len(m), err)
					}
				})
				// M1: the binary protocol drops an empty value from a GET reply
				// (state.go dispatchBinary appends an item only when the value is
				// non-empty), so Gets reports a miss for an item that exists.
				if proto == celmc.ProtocolBinary && len(tc.v) == 0 && !mcKnownBugs {
					continue
				}
				t.Run(tc.name+"_gets", func(t *testing.T) {
					if err := c.SetBytes(ctx, key, tc.v, 0); err != nil {
						t.Fatalf("SetBytes: %v", err)
					}
					it, err := c.Gets(ctx, key)
					if err != nil || !bytes.Equal(it.Value, tc.v) || it.CAS == 0 {
						t.Fatalf("Gets: %d bytes, cas %d, %v", len(it.Value), it.CAS, err)
					}
				})
			}
			for _, tc := range tooLarge {
				t.Run("too_large_"+tc.name, func(t *testing.T) {
					if err := c.SetBytes(ctx, key, []byte("old"), 0); err != nil {
						t.Fatal(err)
					}
					err := c.SetBytes(ctx, key, bytes.Repeat([]byte("x"), tc.n), 0)
					if err == nil {
						t.Fatalf("SetBytes(%d bytes) = nil, want the server's too-large error", tc.n)
					}
					// The refused item must not leave the connection out of
					// step: the next requests still get their own replies.
					if err := c.Ping(ctx); err != nil {
						t.Fatalf("Ping after a too-large set: %v", err)
					}
					if err := c.SetBytes(ctx, key, []byte("after"), 0); err != nil {
						t.Fatalf("Set after a too-large set: %v", err)
					}
					if got, err := c.Get(ctx, key); err != nil || got != "after" {
						t.Fatalf("Get after a too-large set = %q, %v", got, err)
					}
				})
			}
		})
	}
}

// TestRoundTripKeys: the key limits the driver enforces before the wire.
func TestRoundTripKeys(t *testing.T) {
	for _, proto := range mcProtocols {
		t.Run(proto.String(), func(t *testing.T) {
			c := newClient(t, celmc.WithProtocol(proto))
			ctx := ctxWithTimeout(t, 20*time.Second)
			suffix := uniqueKey(t, "k")
			ok := map[string]string{
				"250_bytes": strings.Repeat("k", 250-len(suffix)-1) + "_" + suffix,
				"utf8":      "kéy世_" + suffix,
				"symbols":   "a:b/c-d.e_" + suffix,
			}
			for name, key := range ok {
				t.Run("ok_"+name, func(t *testing.T) {
					cleanupKeys(t, c, key)
					if err := c.Set(ctx, key, "v", 0); err != nil {
						t.Fatalf("Set(%d-byte key): %v", len(key), err)
					}
					if got, err := c.Get(ctx, key); err != nil || got != "v" {
						t.Fatalf("Get = %q, %v", got, err)
					}
				})
			}
			bad := map[string]string{
				"251_bytes": strings.Repeat("k", 251),
				"space":     "a b",
				"empty":     "",
				"control":   "a\x01b",
				"newline":   "a\nb",
				"crlf":      "a\r\nset x 0 0 1\r\nb",
			}
			for name, key := range bad {
				t.Run("malformed_"+name, func(t *testing.T) {
					if err := c.Set(ctx, key, "v", 0); !errors.Is(err, celmc.ErrMalformedKey) {
						t.Fatalf("Set = %v, want ErrMalformedKey", err)
					}
					if _, err := c.Get(ctx, key); !errors.Is(err, celmc.ErrMalformedKey) {
						t.Fatalf("Get = %v, want ErrMalformedKey", err)
					}
				})
			}
			if err := c.Ping(ctx); err != nil {
				t.Fatalf("Ping after rejected keys: %v", err)
			}
		})
	}
}

// TestRoundTripCounters: Incr and Decr at the uint64 edges and on values the
// server cannot treat as a number.
func TestRoundTripCounters(t *testing.T) {
	const maxU64 = "18446744073709551615"
	for _, proto := range mcProtocols {
		t.Run(proto.String(), func(t *testing.T) {
			c := newClient(t, celmc.WithProtocol(proto))
			ctx := ctxWithTimeout(t, 20*time.Second)
			k := uniqueKey(t, "ctr")
			cleanupKeys(t, c, k)

			if err := c.Set(ctx, k, maxU64, 0); err != nil {
				t.Fatal(err)
			}
			if n, err := c.Incr(ctx, k, 1); err != nil || n != 0 {
				t.Fatalf("Incr(max uint64, 1) = %d, %v; want a wrap to 0", n, err)
			}
			if err := c.Set(ctx, k, "5", 0); err != nil {
				t.Fatal(err)
			}
			if n, err := c.Decr(ctx, k, 10); err != nil || n != 0 {
				t.Fatalf("Decr(5, 10) = %d, %v; want 0 (a counter does not go below zero)", n, err)
			}
			if n, err := c.Incr(ctx, k, 1<<64-1); err != nil || n != 1<<64-1 {
				t.Fatalf("Incr(0, max uint64) = %d, %v", n, err)
			}
			// 99 -> 100 grows the text, 100 -> 99 shrinks it: the number is right
			// either way (the server may pad the shorter text with a space).
			if err := c.Set(ctx, k, "99", 0); err != nil {
				t.Fatal(err)
			}
			if n, err := c.Incr(ctx, k, 1); err != nil || n != 100 {
				t.Fatalf("Incr(99) = %d, %v", n, err)
			}
			if s, err := c.Get(ctx, k); err != nil || strings.TrimSpace(s) != "100" {
				t.Fatalf("Get after Incr = %q, %v", s, err)
			}
			if n, err := c.Decr(ctx, k, 1); err != nil || n != 99 {
				t.Fatalf("Decr(100) = %d, %v", n, err)
			}
			if s, err := c.Get(ctx, k); err != nil || strings.TrimSpace(s) != "99" {
				t.Fatalf("Get after Decr = %q, %v", s, err)
			}

			if err := c.Set(ctx, k+"_text", "abc", 0); err != nil {
				t.Fatal(err)
			}
			cleanupKeys(t, c, k+"_text")
			if _, err := c.Incr(ctx, k+"_text", 1); err == nil || errors.Is(err, celmc.ErrCacheMiss) {
				t.Fatalf("Incr(non-numeric) = %v, want the server's non-numeric error", err)
			}
			if _, err := c.Incr(ctx, k+"_missing", 1); !errors.Is(err, celmc.ErrCacheMiss) {
				t.Fatalf("Incr(missing) = %v, want ErrCacheMiss", err)
			}
			if _, err := c.Decr(ctx, k+"_missing", 1); !errors.Is(err, celmc.ErrCacheMiss) {
				t.Fatalf("Decr(missing) = %v, want ErrCacheMiss", err)
			}
			if err := c.Ping(ctx); err != nil {
				t.Fatalf("Ping after counter errors: %v", err)
			}
		})
	}
}

// TestRoundTripMissingKey: the error each command gives for a key that is not there.
func TestRoundTripMissingKey(t *testing.T) {
	for _, proto := range mcProtocols {
		t.Run(proto.String(), func(t *testing.T) {
			c := newClient(t, celmc.WithProtocol(proto))
			ctx := ctxWithTimeout(t, 20*time.Second)
			k := uniqueKey(t, "miss")
			if _, err := c.Get(ctx, k); !errors.Is(err, celmc.ErrCacheMiss) {
				t.Errorf("Get = %v, want ErrCacheMiss", err)
			}
			if _, err := c.GetBytes(ctx, k); !errors.Is(err, celmc.ErrCacheMiss) {
				t.Errorf("GetBytes = %v, want ErrCacheMiss", err)
			}
			if _, err := c.Gets(ctx, k); !errors.Is(err, celmc.ErrCacheMiss) {
				t.Errorf("Gets = %v, want ErrCacheMiss", err)
			}
			if err := c.Touch(ctx, k, time.Minute); !errors.Is(err, celmc.ErrCacheMiss) {
				t.Errorf("Touch = %v, want ErrCacheMiss", err)
			}
			if err := c.Delete(ctx, k); !errors.Is(err, celmc.ErrCacheMiss) {
				t.Errorf("Delete = %v, want ErrCacheMiss", err)
			}
			if err := c.Append(ctx, k, "x"); !errors.Is(err, celmc.ErrNotStored) {
				t.Errorf("Append = %v, want ErrNotStored", err)
			}
			if err := c.Prepend(ctx, k, "x"); !errors.Is(err, celmc.ErrNotStored) {
				t.Errorf("Prepend = %v, want ErrNotStored", err)
			}
			if m, err := c.GetMulti(ctx, k, k+"2"); err != nil || len(m) != 0 {
				t.Errorf("GetMulti(all missing) = %v, %v; want an empty map and no error", m, err)
			}
			if m, err := c.GetMulti(ctx); err != nil || len(m) != 0 {
				t.Errorf("GetMulti() = %v, %v; want an empty map and no error", m, err)
			}
		})
	}
}

// TestRoundTripFlagsAndCAS: the client API cannot set item flags, so a flag is
// stored with a raw text `set`; Gets, Get and GetMulti must read it back on
// both protocols, and CAS tokens must change with every store.
func TestRoundTripFlagsAndCAS(t *testing.T) {
	for _, proto := range mcProtocols {
		t.Run(proto.String(), func(t *testing.T) {
			c := newClient(t, celmc.WithProtocol(proto))
			ctx := ctxWithTimeout(t, 20*time.Second)
			k := uniqueKey(t, "flags")
			for _, flags := range []uint32{0, 1, 65535, 1 << 31, 4294967295} {
				key := fmt.Sprintf("%s_%d", k, flags)
				cleanupKeys(t, c, key)
				if got := rawText(t, fmt.Sprintf("set %s %d 0 3\r\nabc\r\n", key, flags)); got != "STORED" {
					t.Fatalf("raw set: %q", got)
				}
				it, err := c.Gets(ctx, key)
				if err != nil || it.Flags != flags || string(it.Value) != "abc" || it.Key != key {
					t.Fatalf("Gets(flags %d) = %+v, %v", flags, it, err)
				}
				if got, err := c.Get(ctx, key); err != nil || got != "abc" {
					t.Fatalf("Get(flags %d) = %q, %v", flags, got, err)
				}
				if m, err := c.GetMulti(ctx, key, key+"_x"); err != nil || m[key] != "abc" || len(m) != 1 {
					t.Fatalf("GetMulti(flags %d) = %v, %v", flags, m, err)
				}
			}

			key := k + "_cas"
			cleanupKeys(t, c, key)
			var prev uint64
			for i := 0; i < 5; i++ {
				if err := c.Set(ctx, key, fmt.Sprint(i), 0); err != nil {
					t.Fatal(err)
				}
				it, err := c.Gets(ctx, key)
				if err != nil || it.CAS == 0 || it.CAS == prev {
					t.Fatalf("Gets #%d: cas %d (previous %d), %v; want a new non-zero token per store", i, it.CAS, prev, err)
				}
				prev = it.CAS
			}
			if ok, err := c.CAS(ctx, key, "x", prev+1000000, 0); ok || !errors.Is(err, celmc.ErrCASConflict) {
				t.Fatalf("CAS with a wrong token = %v, %v; want false, ErrCASConflict", ok, err)
			}
			if ok, err := c.CAS(ctx, key, "x", prev, 0); !ok || err != nil {
				t.Fatalf("CAS with the current token = %v, %v", ok, err)
			}
			if _, err := c.CAS(ctx, key+"_missing", "x", 1, 0); !errors.Is(err, celmc.ErrCacheMiss) {
				t.Fatalf("CAS on a missing key = %v, want ErrCacheMiss", err)
			}
		})
	}
}

// TestRoundTripSetTypes: the Go value kinds Set accepts come back as their
// text form.
func TestRoundTripSetTypes(t *testing.T) {
	for _, proto := range mcProtocols {
		t.Run(proto.String(), func(t *testing.T) {
			c := newClient(t, celmc.WithProtocol(proto))
			ctx := ctxWithTimeout(t, 20*time.Second)
			k := uniqueKey(t, "types")
			cleanupKeys(t, c, k)
			for _, tc := range []struct {
				v    any
				want string
			}{
				{"héllo 世界", "héllo 世界"},
				{[]byte{0, 1, 2}, "\x00\x01\x02"},
				{42, "42"},
				{int64(-7), "-7"},
				{int64(1<<63 - 1), "9223372036854775807"},
				{3.5, "3.5"},
				{true, "1"},
				{false, "0"},
			} {
				if err := c.Set(ctx, k, tc.v, 0); err != nil {
					t.Fatalf("Set(%T %v): %v", tc.v, tc.v, err)
				}
				if got, err := c.Get(ctx, k); err != nil || got != tc.want {
					t.Fatalf("Set(%T %v) then Get = %q, %v; want %q", tc.v, tc.v, got, err, tc.want)
				}
			}
		})
	}
}

// TestRoundTripMultiGet: many keys with hits, misses and large values in one
// multi-get; the binary protocol pipelines it with quiet opcodes.
func TestRoundTripMultiGet(t *testing.T) {
	for _, proto := range mcProtocols {
		t.Run(proto.String(), func(t *testing.T) {
			c := newClient(t, celmc.WithProtocol(proto))
			ctx := ctxWithTimeout(t, 60*time.Second)
			k := uniqueKey(t, "multi")
			var keys []string
			want := map[string][]byte{}
			for i := 0; i < 300; i++ {
				key := fmt.Sprintf("%s_%03d", k, i)
				keys = append(keys, key)
				cleanupKeys(t, c, key)
				var v []byte
				switch {
				case i%3 == 0: // miss
					continue
				case i%50 == 1: // large: 10 of these are ~2 MiB together
					v = bytes.Repeat([]byte{byte('a' + i%26)}, 200*1024)
				case i%7 == 0: // empty
					v = []byte{}
				default:
					v = []byte(fmt.Sprintf("value-%d\r\nEND", i))
				}
				if err := c.SetBytes(ctx, key, v, 0); err != nil {
					t.Fatalf("SetBytes(%s): %v", key, err)
				}
				want[key] = v
			}
			got, err := c.GetMultiBytes(ctx, keys...)
			if err != nil {
				t.Fatalf("GetMultiBytes: %v", err)
			}
			if len(got) != len(want) {
				t.Fatalf("GetMultiBytes: %d entries, want %d", len(got), len(want))
			}
			for key, v := range want {
				if g, ok := got[key]; !ok || !bytes.Equal(g, v) {
					t.Fatalf("GetMultiBytes[%s]: %d bytes (present %v), want %d bytes", key, len(g), ok, len(v))
				}
			}
			// The same keys listed twice: one entry per key.
			dup := append(append([]string{}, keys[1:5]...), keys[1:5]...)
			if m, err := c.GetMulti(ctx, dup...); err != nil || len(m) != 3 { // keys[3] is a miss (i%3==0)
				t.Fatalf("GetMulti(duplicates) = %d entries, %v; want 3", len(m), err)
			}
		})
	}
}
