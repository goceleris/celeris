package cache

import (
	"context"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// ttlStore records the TTL of the last write a fill made (a fill into a store
// that is not the cache's own MemoryStore is a plain Set).
type ttlStore struct {
	*MemoryStore
	last time.Duration
	n    int
}

func (s *ttlStore) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	s.last, s.n = ttl, s.n+1
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
		// Round 5 (RFC 9111 4.2.1: an invalid max-age is stale): the argument
		// is 1*DIGIT, or the response is not stored (want 0). "max-age=" was
		// stored for Config.TTL until then.
		{"max-age=", 0},
		{"max-age=abc", 0},
		{"max-age=-1", 0},
		{"max-age=60abc", 0},
		{"max-age=+60", 0},
		{"max-age=6e1", 0},
		{"max-age=60.5", 0},
		{`max-age="60"`, 0},
		{`max-age="0"`, 0},
		{"max-age=60, max-age=abc", 0},
		{"max-age=abc, max-age=60", 0},
		{"max-age=60, max-age=0", 0}, // any max-age=0, not only the first
		{"max-age=05", 5 * time.Second},
		{"foo=, max-age=5", 5 * time.Second}, // "name=" stays stored for the other names
		{"x-max-age=0", cfgTTL},
		// A delta-seconds past what a time.Duration holds (9223372036 s is
		// the last that fits) wrapped to a negative TTL, which the stores
		// read as "no expiry" (the "forever" idiom max-age=9999999999).
		{"max-age=9223372036", cfgTTL},
		{"max-age=9223372037", cfgTTL},
		{"max-age=9999999999", cfgTTL},
		{"max-age=10000000000", cfgTTL},
		{"max-age=99999999999999999999999", cfgTTL},
		{"max-age=9999999999, max-age=1", cfgTTL},
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
				wantWrites := 1
				if tc.want == 0 {
					wantWrites = 0
				}
				if kv.n != wantWrites || kv.last != tc.want {
					t.Errorf("cache-control %q: %d write(s), last TTL %v, want %d write(s) with TTL %v", tc.cc, kv.n, kv.last, wantWrites, tc.want)
				}
			})
		}
	}
}

// TestCacheHugeMaxAgeStillExpires888: the stored TTL of a response that says
// max-age=9999999999 is Config.TTL, so the entry is gone once that passes. A
// wrapped (negative) TTL stored the entry with no expiry at all.
func TestCacheHugeMaxAgeStillExpires888(t *testing.T) {
	for _, noSF := range []bool{false, true} {
		name := "singleflight"
		if noSF {
			name = "no-singleflight"
		}
		t.Run(name, func(t *testing.T) {
			kv := NewMemoryStore()
			defer kv.Close()
			var runs int
			h := func(c *celeris.Context) error {
				runs++
				c.SetHeader("cache-control", "max-age=9999999999")
				return c.String(200, "v%d", runs)
			}
			mw := New(Config{Store: kv, TTL: 50 * time.Millisecond, DisableSingleflight: noSF})
			runOnce(t, mw, h, "GET", "/huge888")
			time.Sleep(120 * time.Millisecond)
			rec := runOnce(t, mw, h, "GET", "/huge888")
			if rec.Header("x-cache") != "MISS" || runs != 2 {
				t.Errorf("120 ms after a fill with Config.TTL 50 ms: %q x-cache %q after %d handler run(s), want a MISS and 2 runs", rec.Body, rec.Header("x-cache"), runs)
			}
		})
	}
}
