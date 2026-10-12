package cache

import (
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// celeris#888 item 27, review round 2: a Cache-Control directive's quoted
// string can hold a quoted-pair (RFC 9110 5.6.4: a backslash and the byte it
// escapes, so \" is not the end of the string), and a quote that never closes
// ends nothing. A tokenizer that flips its quote state on every double quote
// reads the comma after ext="a\"" as inside the string and never sees the
// next directive: a private or no-store response was then stored and served
// to other users.

var quotedPairCases888 = []struct {
	cc          string
	noStore     bool
	maxAge      int
	hasMaxAge   bool
	description string
}{
	{cc: `ext="a\"", no-store`, noStore: true, description: "a quoted-pair, then no-store"},
	{cc: `ext="a\"", private`, noStore: true, description: "a quoted-pair, then private"},
	{cc: `ext="a\"", no-cache`, noStore: true, description: "a quoted-pair, then no-cache"},
	{cc: `ext="a\"", max-age=0`, noStore: true, description: "a quoted-pair, then max-age=0"},
	{cc: `ext="a\"", max-age=5`, maxAge: 5, hasMaxAge: true, description: "a quoted-pair, then max-age=5 (the TTL is 5 s, not Config.TTL)"},
	{cc: `ext="a\\", private`, noStore: true, description: "an escaped backslash, then the closing quote, then private"},
	{cc: `ext="a\\\"", private`, noStore: true, description: "an escaped backslash and an escaped quote, then private"},
	{cc: `ext="a\", private", max-age=7`, noStore: true, description: "private is inside the string (a quoted-pair keeps it open), and the floor refuses it all the same, as main did (round 5)"},
	{cc: `ext="a, max-age=5`, noStore: true, description: "a quote that never closes: not storable"},
	{cc: `ext="a\"`, noStore: true, description: "a quote that only a quoted-pair would close: not storable"},
	{cc: `ext="a, private`, noStore: true, description: "a quote that never closes, private after it"},
	{cc: `ext=a"b, private`, noStore: true, description: "a quote inside a token is not the start of a string"},
	{cc: `ext=a"b, max-age=5, private`, noStore: true, description: "a stray quote, then private"},
	{cc: `ext=a="b, private"`, noStore: true, description: "a second '=' does not start a string, so private is a directive"},
	{cc: `ext="a"="b, private"`, noStore: true, description: "a second '=' after a closed string does not start one either"},
	{cc: `ext=a="b, max-age=5`, noStore: true, description: "a stray quote after a second '=' is doubtful, whatever follows"},
	{cc: `"private", max-age=5`, noStore: true, description: "a quote in the directive name is doubtful"},
	{cc: `max-age=5, ext=a"b`, noStore: true, description: "a stray quote in the last directive is doubtful"},
	{cc: `ext=a"b, max-age=5`, noStore: true, description: "a stray quote: the value cannot be read around it, so it is not storable"},
	{cc: `ext="a", private`, noStore: true, description: "a well-formed string, then private (the control)"},
	{cc: `ext="a", max-age=9`, maxAge: 9, hasMaxAge: true, description: "a well-formed string, then max-age (the control)"},
	{cc: `no-cache="a, b", max-age=60`, noStore: true, description: "the field-qualified no-cache with a quoted comma (the control)"},
	{cc: `x-ext="no-cache, private", max-age=11`, noStore: true, description: "private inside a string: the floor refuses it, as main did (round 5)"},
	{cc: `x-ext="no-cache, public", max-age=11`, noStore: false, maxAge: 11, hasMaxAge: true, description: "directives named inside a string are not directives (the control)"},
}

// TestParseCacheControlQuotedPairs888 is the unit half.
func TestParseCacheControlQuotedPairs888(t *testing.T) {
	for _, tc := range quotedPairCases888 {
		t.Run(tc.cc, func(t *testing.T) {
			noStore, maxAge, hasMaxAge := parseCacheControl(tc.cc)
			if noStore != tc.noStore || hasMaxAge != tc.hasMaxAge || maxAge != tc.maxAge {
				t.Errorf("parseCacheControl(%s) = noStore %v, max-age %d (present %v), want noStore %v, max-age %d (present %v): %s",
					tc.cc, noStore, maxAge, hasMaxAge, tc.noStore, tc.maxAge, tc.hasMaxAge, tc.description)
			}
		})
	}
}

// TestCacheQuotedPairNeverStoresAPrivateResponse888 is the end-to-end half:
// what the cache writes to its store, with and without singleflight.
func TestCacheQuotedPairNeverStoresAPrivateResponse888(t *testing.T) {
	const cfgTTL = time.Minute
	for _, tc := range quotedPairCases888 {
		for _, noSF := range []bool{false, true} {
			name := tc.cc
			if noSF {
				name += "/no-singleflight"
			}
			t.Run(name, func(t *testing.T) {
				kv := &ttlStore{MemoryStore: NewMemoryStore()}
				defer kv.Close()
				h := func(c *celeris.Context) error {
					c.SetHeader("cache-control", tc.cc)
					return c.String(200, "secret-of-user-a")
				}
				mw := New(Config{Store: kv, TTL: cfgTTL, DisableSingleflight: noSF})
				runOnce(t, mw, h, "GET", "/qp888")
				wantWrites, wantTTL := 1, cfgTTL
				if tc.noStore {
					wantWrites, wantTTL = 0, 0
				} else if tc.hasMaxAge {
					wantTTL = time.Duration(tc.maxAge) * time.Second
				}
				if kv.n != wantWrites || kv.last != wantTTL {
					t.Errorf("cache-control %s: %d write(s), last TTL %v, want %d write(s), TTL %v: %s",
						tc.cc, kv.n, kv.last, wantWrites, wantTTL, tc.description)
				}
			})
		}
	}
}
