package store

import (
	"context"
	"errors"
	"testing"
	"time"
	"unsafe"
)

// keyView returns a string that shares b's bytes, as a request string does
// on epoll and io_uring: a view of the connection's receive buffer, which
// the engine overwrites with the connection's next request.
func keyView(b []byte) string { return unsafe.String(unsafe.SliceData(b), len(b)) }

// TestMemoryKVKeepsItsOwnKeys pins celeris#719.
//
// A caller hands MemoryKV a key and may change its bytes once the call
// returns: idempotency passes c.Header("idempotency-key"), session the
// extracted ID, and on the native engines those are views of the receive
// buffer. MemoryKV must keep a key of its own wherever the key goes into
// the map. That is every insert, and also every re-assignment of an existing
// key: a Go map assignment to a key already present replaces the stored key
// with the one given. Each case stores under a view, overwrites the view's
// bytes with a key of the same length, and then needs the entry under its
// original key, with Scan reporting the original key and no other.
func TestMemoryKVKeepsItsOwnKeys(t *testing.T) {
	ctx := context.Background()
	const orig, other = "key-aaaa", "key-bbbb"
	cases := []struct {
		name string
		// prep runs before the store call, with an owned key (setup only).
		prep func(m *MemoryKV)
		// put stores under k, a view.
		put func(m *MemoryKV, k string) error
	}{
		{"Set new key", nil, func(m *MemoryKV, k string) error {
			return m.Set(ctx, k, []byte("1"), 0)
		}},
		{"SetNX new key", nil, func(m *MemoryKV, k string) error {
			if ok, err := m.SetNX(ctx, k, []byte("1"), 0); err != nil || !ok {
				return errors.Join(err, errors.New("SetNX did not acquire"))
			}
			return nil
		}},
		{"SetNX over an expired key", func(m *MemoryKV) {
			_ = m.Set(ctx, orig, []byte("old"), time.Nanosecond)
			time.Sleep(time.Millisecond)
		}, func(m *MemoryKV, k string) error {
			if ok, err := m.SetNX(ctx, k, []byte("1"), 0); err != nil || !ok {
				return errors.Join(err, errors.New("SetNX did not acquire the expired key"))
			}
			return nil
		}},
		{"Increment new key", nil, func(m *MemoryKV, k string) error {
			_, err := m.Increment(ctx, k, 0)
			return err
		}},
		{"Increment existing key", func(m *MemoryKV) {
			_, _ = m.Increment(ctx, orig, 0)
		}, func(m *MemoryKV, k string) error {
			_, err := m.Increment(ctx, k, 0)
			return err
		}},
		{"Increment over an expired key", func(m *MemoryKV) {
			_, _ = m.Increment(ctx, orig, time.Nanosecond)
			time.Sleep(time.Millisecond)
		}, func(m *MemoryKV, k string) error {
			_, err := m.Increment(ctx, k, 0)
			return err
		}},
		// Control: the update path keeps the entry and never re-assigns.
		{"Set existing key (control)", func(m *MemoryKV) {
			_ = m.Set(ctx, orig, []byte("old"), 0)
		}, func(m *MemoryKV, k string) error {
			return m.Set(ctx, k, []byte("1"), 0)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMemoryKV(MemoryKVConfig{Shards: 1})
			defer m.Close()
			if tc.prep != nil {
				tc.prep(m)
			}
			buf := []byte(orig)
			if err := tc.put(m, keyView(buf)); err != nil {
				t.Fatal(err)
			}
			copy(buf, other) // the engine receives the next request
			keys, _ := m.Scan(ctx, "key-")
			v, err := m.Get(ctx, orig)
			t.Logf("MW719MEMKV case=%q get(%s) err=%v scan=%q", tc.name, orig, err, keys)
			if err != nil || len(v) == 0 {
				t.Errorf("Get(%q) after the caller's key bytes changed: %q, %v; want the stored value", orig, v, err)
			}
			if len(keys) != 1 || keys[0] != orig {
				t.Errorf("Scan reports keys %q; want [%q]", keys, orig)
			}
		})
	}
}
