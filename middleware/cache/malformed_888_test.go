package cache

import (
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// celeris#888 item 27, review round 4. A Cache-Control value that is not well
// formed is not stored. main refused any value that held "no-store" or
// "private" anywhere (a substring check); matching directives as tokens
// (item 27) made the cache read the directive name as the text before the
// first '=' and nothing else, so a value that a recipient cannot split into
// directives hid its private or no-store from the cache and the response of one
// user was stored and served to the next one:
//
//	private; max-age=0       (';' is not a list separator: one directive named "private; max-age")
//	no-cache; no-store       (the whole value is one name)
//	max-age=60 private       (max-age=60, then text no list element can hold)
//	x="a"private             (text after the closing quote)
//	private\max-age          (a backslash outside a string)
//	="a, private"            (no name)
//	x ="a, private"          (a blank before the '=')
//
// The cache must not guess what such a value means: it stores nothing.

// malformedCC888 are values with no valid reading as a list of directives.
// Every one of them held "private" or "no-store" (or no-cache) for main.
var malformedCC888 = []string{
	"private; max-age=0",
	"no-cache; no-store",
	"no-store; no-cache; must-revalidate",
	"private;max-age=600",
	"max-age=60 private",
	"public, max-age=60; private",
	`private\max-age`,
	`x="a"private`,
	`0="" no-cache`,
	`="a, private"`,
	`x ="a, private"`,
	` ="a, private", max-age=5`,
	"max-age=5 private",
	"private;x",
	"max-age=60; no-store",
	"public; private",
	"max-age=60 no-store",
	"private max-age=60",
	"no-store max-age=60",
	`ext=a\b, private`,
	`ext=a\b, max-age=5`,
	"max-age=5; community=\"x\"",
	"x= private",
	"max-age= 60 private",
	"=private",
	"private, =x",
	"max-age=60,, =x",
	"max-age=60, private;",
}

// wellFormedCC888 are the controls: values that parse, with the reading they
// had before this round (no-store/private/no-cache/max-age=0 not stored, max-age
// the TTL, a quoted string or a longer name hides nothing).
var wellFormedCC888 = []struct {
	cc        string
	noStore   bool
	maxAge    int
	hasMaxAge bool
}{
	{cc: "private, max-age=60", noStore: true},
	{cc: "public, max-age=60", maxAge: 60, hasMaxAge: true},
	{cc: "public ,  max-age=60 , must-revalidate", maxAge: 60, hasMaxAge: true},
	{cc: "public,\tmax-age=60", maxAge: 60, hasMaxAge: true},
	{cc: "max-age=60,", maxAge: 60, hasMaxAge: true},
	{cc: ",max-age=60", maxAge: 60, hasMaxAge: true},
	{cc: "max-age=60,,must-revalidate", maxAge: 60, hasMaxAge: true},
	{cc: "max-age=60 ", maxAge: 60, hasMaxAge: true},
	{cc: "max-age=", noStore: false},
	{cc: `x-ext="private; no-store", max-age=9`, maxAge: 9, hasMaxAge: true},
	{cc: `x-ext="a" , max-age=9`, maxAge: 9, hasMaxAge: true},
	{cc: `no-cache="set-cookie", max-age=9`, noStore: true},
	{cc: "x-private=1, max-age=9", maxAge: 9, hasMaxAge: true},
	{cc: "x=private, max-age=9", maxAge: 9, hasMaxAge: true},
	{cc: "s-maxage=3, max-age=7", maxAge: 7, hasMaxAge: true},
	{cc: "", noStore: false},
	{cc: " ", noStore: false},
}

// TestParseCacheControlMalformedStoresNothing888 is the unit half.
func TestParseCacheControlMalformedStoresNothing888(t *testing.T) {
	for _, cc := range malformedCC888 {
		t.Run(cc, func(t *testing.T) {
			if noStore, maxAge, has := parseCacheControl(cc); !noStore || has || maxAge != 0 {
				t.Errorf("parseCacheControl(%q) = noStore %v, max-age %d (present %v), want noStore: a value that is not well formed is not stored", cc, noStore, maxAge, has)
			}
		})
	}
	for _, tc := range wellFormedCC888 {
		t.Run("well-formed/"+tc.cc, func(t *testing.T) {
			noStore, maxAge, has := parseCacheControl(tc.cc)
			if noStore != tc.noStore || maxAge != tc.maxAge || has != tc.hasMaxAge {
				t.Errorf("parseCacheControl(%q) = noStore %v, max-age %d (present %v), want noStore %v, max-age %d (present %v)",
					tc.cc, noStore, maxAge, has, tc.noStore, tc.maxAge, tc.hasMaxAge)
			}
		})
	}
}

// TestCacheMalformedNeverStoresAResponse888 is the end-to-end half: two GETs
// through cache.New, with and without singleflight; the second must not get
// the first user's response.
func TestCacheMalformedNeverStoresAResponse888(t *testing.T) {
	for _, cc := range append([]string{"private, max-age=60"}, malformedCC888...) {
		for _, noSF := range []bool{false, true} {
			name := cc
			if noSF {
				name += "/no-singleflight"
			}
			t.Run(name, func(t *testing.T) {
				kv := &ttlStore{MemoryStore: NewMemoryStore()}
				defer kv.Close()
				runs := 0
				h := func(c *celeris.Context) error {
					runs++
					c.SetHeader("cache-control", cc)
					return c.String(200, "secret-of-user-%d", runs)
				}
				mw := New(Config{Store: kv, TTL: time.Minute, DisableSingleflight: noSF})
				runOnce(t, mw, h, "GET", "/me888")
				second := runOnce(t, mw, h, "GET", "/me888")
				if kv.n != 0 || second.Header("x-cache") != "MISS" || string(second.Body) != "secret-of-user-2" {
					t.Errorf("cache-control %q: %d store write(s), second request x-cache %q body %q, want no write, MISS and the second user's own body",
						cc, kv.n, second.Header("x-cache"), second.Body)
				}
			})
		}
	}
}

// refTchar is RFC 9110 5.6.2's tchar, for lower case values.
func refTchar(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", b) >= 0
}

// refCC is an independent, strict reading of Cache-Control (RFC 9111 5.2:
// 1#directive, directive = token ["=" (token / quoted-string)], with the list
// syntax of RFC 9110 5.6.1: empty elements and blanks around the commas are
// allowed). ok is false for a value that is not a list of directives. The one
// leniency, shared with the implementation and with the max-age= row of
// TestCacheControlSetsTheStoredTTL888: "name=" with an empty unquoted value.
func refCC(s string) (dirs [][2]string, ok bool) {
	i := 0
	ows := func() {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
	}
	for {
		ows()
		for i < len(s) && s[i] == ',' {
			i++
			ows()
		}
		if i >= len(s) {
			return dirs, true
		}
		st := i
		for i < len(s) && refTchar(s[i]) {
			i++
		}
		if i == st {
			return nil, false
		}
		name, val := s[st:i], ""
		if i < len(s) && s[i] == '=' {
			i++
			if i < len(s) && s[i] == '"' {
				i++
				closed := false
				for i < len(s) {
					c := s[i]
					if c == '\\' {
						if i+1 >= len(s) {
							return nil, false
						}
						i += 2
						continue
					}
					i++
					if c == '"' {
						closed = true
						break
					}
				}
				if !closed {
					return nil, false
				}
				val = `"` // a quoted value: never a max-age
			} else {
				vs := i
				for i < len(s) && refTchar(s[i]) {
					i++
				}
				val = s[vs:i]
			}
		}
		dirs = append(dirs, [2]string{name, val})
		ows()
		if i < len(s) && s[i] != ',' {
			return nil, false
		}
	}
}

// refDecision is what a recipient that reads a well-formed value strictly
// does: not stored for no-store, private, no-cache or max-age=0, else the
// first max-age with a numeric value, capped at maxDeltaSeconds.
func refDecision(dirs [][2]string) (noStore bool, maxAge int, has bool) {
	for _, d := range dirs {
		switch d[0] {
		case "no-store", "private", "no-cache":
			return true, 0, false
		case "max-age":
			if has || strings.HasPrefix(d[1], `"`) {
				continue
			}
			if n, ok := atoiPrefix(d[1]); ok {
				if n == 0 {
					return true, 0, false
				}
				maxAge, has = min(n, maxDeltaSeconds), true
			}
		}
	}
	return false, maxAge, has
}

// TestParseCacheControlAgainstTheReference888 is the property: over random
// strings of the tokens that matter, parseCacheControl stores nothing exactly
// when the value is not well formed or the strict reading says not to store,
// and gives the strict reading's TTL otherwise. In particular: every value
// that main refused (it held "no-store" or "private" as a substring) is
// refused unless it is a well-formed value in which they are not directives
// (x-private=1, a quoted string, x=private). CELERIS_CC_FUZZ_N sets the number
// of cases (the lane's script runs 3 million).
func TestParseCacheControlAgainstTheReference888(t *testing.T) {
	n := 300_000
	if v := os.Getenv("CELERIS_CC_FUZZ_N"); v != "" {
		if x, err := strconv.Atoi(v); err == nil {
			n = x
		}
	}
	if testing.Short() {
		n = 20_000
	}
	alpha := []string{"private", "no-store", "no-cache", "max-age", "public", "x", "=", `"`, `\`, ",", " ", "\t", ";", "0", "5", "a", "-"}
	r := rand.New(rand.NewPCG(1, 2))
	var wellFormed, malformed, mainRefusedButStored, bad int
	example := map[string]string{}
	fail := func(kind, s string, got, want string) {
		if bad++; bad <= 20 {
			t.Errorf("%s: %q: parseCacheControl %s, want %s", kind, s, got, want)
		}
	}
	for i := 0; i < n; i++ {
		var b strings.Builder
		for j, k := 0, 1+r.IntN(9); j < k; j++ {
			b.WriteString(alpha[r.IntN(len(alpha))])
		}
		s := b.String()
		noStore, maxAge, has := parseCacheControl(s)
		mainRefused := strings.Contains(s, "no-store") || strings.Contains(s, "private")
		dirs, ok := refCC(s)
		if !ok {
			malformed++
			if !noStore {
				fail("malformed value stored", s, fmt.Sprintf("noStore=%v", noStore), "noStore=true")
			}
			continue
		}
		wellFormed++
		rNo, rMa, rHas := refDecision(dirs)
		if noStore != rNo || (!rNo && (maxAge != rMa || has != rHas)) {
			fail("well-formed value", s, fmt.Sprintf("(%v, %d, %v)", noStore, maxAge, has), fmt.Sprintf("(%v, %d, %v)", rNo, rMa, rHas))
		}
		if mainRefused && !noStore {
			mainRefusedButStored++
			example[s] = "stored"
		}
	}
	// A value main refused and this stores must be a well-formed one whose
	// strict reading has no such directive: show that the classes are not
	// empty (the property above is not vacuous) and not large.
	if malformed == 0 || wellFormed == 0 {
		t.Fatalf("degenerate corpus: %d well formed, %d malformed", wellFormed, malformed)
	}
	t.Logf("%d cases: %d well formed, %d malformed (all not stored); %d well-formed values that main refused and this stores (no such directive)", n, wellFormed, malformed, mainRefusedButStored)
	shown := 0
	for s := range example {
		if shown++; shown > 5 {
			break
		}
		t.Logf("  main refused, stored here, well formed: %q", s)
	}
}
