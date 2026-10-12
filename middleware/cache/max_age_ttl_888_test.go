package cache

import (
	"context"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// ttlStore records the TTL of the last write a fill made.
type ttlStore struct {
	*MemoryStore
	last time.Duration
	n    int
}

func (s *ttlStore) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	s.last, s.n = ttl, s.n+1
	return s.MemoryStore.Set(ctx, key, value, ttl)
}

func (s *ttlStore) setIfNewer(ctx context.Context, key string, value []byte, ttl time.Duration, gen uint64) error {
	s.last, s.n = ttl, s.n+1
	if vs, ok := any(s.MemoryStore).(versioned888); ok {
		return vs.setIfNewer(ctx, key, value, ttl, gen)
	}
	return s.MemoryStore.Set(ctx, key, value, ttl)
}

// TestCacheControlSetsTheStoredTTL888: the TTL a response is stored with is
// min(Config.TTL, max-age) and is never 0: a TTL of 0 means no expiry in
// MemoryStore and no freshness limit in the coalesced call. A string that
// only contains "max-age=0" (an extension directive, a quoted value) is not a
// max-age directive.
func TestCacheControlSetsTheStoredTTL888(t *testing.T) {
	const cfgTTL = time.Minute
	for _, tc := range []struct {
		cc   string
		want time.Duration
	}{
		{"public, max-age=30", 30 * time.Second},
		{"max-age=7200", cfgTTL},
		{"max-age=", cfgTTL},
		{"x-max-age=0", cfgTTL},
		{"x-max-age=0, max-age=1", time.Second},
		{`ext="max-age=0", max-age=45`, 45 * time.Second},
		{"", cfgTTL},
	} {
		for _, noSF := range []bool{false, true} {
			name := tc.cc
			if noSF {
				name += "/no-singleflight"
			}
			t.Run(name, func(t *testing.T) {
				kv := &ttlStore{MemoryStore: NewMemoryStore()}
				defer kv.Close()
				h := func(c *celeris.Context) error {
					if tc.cc != "" {
						c.SetHeader("cache-control", tc.cc)
					}
					return c.String(200, "value")
				}
				mw := New(Config{Store: kv, TTL: cfgTTL, DisableSingleflight: noSF})
				runOnce(t, mw, h, "GET", "/ttl888")
				if kv.n != 1 || kv.last != tc.want {
					t.Errorf("cache-control %q: %d write(s), last TTL %v, want 1 write with TTL %v", tc.cc, kv.n, kv.last, tc.want)
				}
			})
		}
	}
}
