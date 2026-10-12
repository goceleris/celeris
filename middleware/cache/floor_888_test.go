package cache

import (
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// celeris#888 item 27, review round 5. Rounds 3 and 4 each found a way for a
// malformed Cache-Control value to hide a "private" or "no-store" that main
// refused (main: strings.Contains on the lowercased value). Patching the
// parser shape by shape did not converge, so the structure changed:
//
//   - the floor: a value that holds "private" or "no-store" anywhere, in any
//     case, is not stored, before any parsing. The parser can only add
//     refusals (no-cache, max-age=0, a value that is not a list, a max-age
//     whose argument is not 1*DIGIT) and compute the TTL; it can never make the
//     cache looser than main.
//   - max-age is exactly 1*DIGIT (RFC 9111 4.2.1: an invalid max-age is
//     stale), so max-age=60private, max-age=abc, max-age=-1 and
//     max-age="60" are not stored.
//
// The last bug: SetHeader strips CR, LF and NUL, so a handler that joins two
// directives with "\n" sends "max-age=60private", and the parser read 60.

// gluedCC888 are values, as the handler passes them to SetHeader, that main
// refused or that are not well formed. The wire value is what SetHeader leaves
// (CR, LF and NUL stripped).
var gluedCC888 = []string{
	"max-age=60private",
	"max-age=60\nprivate",
	"max-age=60\r\nprivate",
	"max-age=60\x00private",
	"max-age=600no-store",
	"max-age=60\nno-store",
	"max-age=60\r\nno-store",
	"max-age=private",
	"max-age=no-store",
	"max-age=-1private",
	"max-age=-1\nprivate",
	"max-age=60,\nprivate",
	"max-age=60\r\n, private",
	"pri\nvate",
	"pri\r\nvate, max-age=60",
	"no-\x00store",
	"max-age=60, no-\r\nstore",
	"public,max-age=60;private",
	// the case of the bytes does not matter
	"Private",
	"NO-STORE",
	"max-age=60PRIVATE",
	"max-age=60\nPrivate",
	"public, No-Store",
	"x-Private-ext=1, max-age=60",
	// not main's refusals, but max-age has no valid argument
	"max-age=3600no-cache",
	"max-age=3600\nno-cache",
	"max-age=60public",
	"max-age=abc",
	"max-age=",
	"max-age=-1",
	`max-age="60"`,
}

// TestCacheControlGluedNeverStored888 is the end-to-end half: two GETs
// through cache.New with and without singleflight; nothing is written to the
// store and the second request is the second user's own response.
func TestCacheControlGluedNeverStored888(t *testing.T) {
	for _, cc := range gluedCC888 {
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
				runOnce(t, mw, h, "GET", "/glued888")
				second := runOnce(t, mw, h, "GET", "/glued888")
				if kv.n != 0 || second.Header("x-cache") != "MISS" || string(second.Body) != "secret-of-user-2" {
					t.Errorf("cache-control %q (on the wire %q): %d store write(s), second request x-cache %q body %q, want no write, MISS and the second user's own body",
						cc, second.Header("cache-control"), kv.n, second.Header("x-cache"), second.Body)
				}
			})
		}
	}
}

// TestCacheControlFloorKeepsWellFormed888: the controls. Values with neither
// word are stored, with the TTL they had.
func TestCacheControlFloorKeepsWellFormed888(t *testing.T) {
	for _, tc := range []struct {
		cc   string
		want time.Duration
	}{
		{"public, max-age=60", 60 * time.Second},
		{"max-age=60, must-revalidate", 60 * time.Second},
		{"max-age=05", 5 * time.Second},
		{"public", time.Minute},
		{`x-ext="a, b", max-age=9`, 9 * time.Second},
	} {
		t.Run(tc.cc, func(t *testing.T) {
			kv := &ttlStore{MemoryStore: NewMemoryStore()}
			defer kv.Close()
			h := func(c *celeris.Context) error {
				c.SetHeader("cache-control", tc.cc)
				return c.String(200, "shared")
			}
			mw := New(Config{Store: kv, TTL: time.Minute})
			runOnce(t, mw, h, "GET", "/ok888")
			if kv.n != 1 || kv.last != tc.want {
				t.Errorf("cache-control %q: %d write(s), TTL %v, want 1 write, TTL %v", tc.cc, kv.n, kv.last, tc.want)
			}
		})
	}
}

// ---- the property: the head is never looser than main ----

// refStrip is what SetHeader does to a value: CR, LF and NUL are removed, so
// the bytes on either side are glued. (Written here on purpose: the oracle
// shares no code with the implementation.)
func refStrip(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if c := s[i]; c != '\r' && c != '\n' && c != 0 {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// refMainRefuses is main's check on the value as it is (case-sensitive).
func refMainRefuses(v string) bool {
	return strings.Contains(v, "no-store") || strings.Contains(v, "private")
}

// refLowerASCII is a hand-written ASCII lowercase, no library involved.
func refLowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if x, err := strconv.Atoi(v); err == nil {
			return x
		}
	}
	return def
}

func envSeed() uint64 {
	if v := os.Getenv("CELERIS_CC_FUZZ_SEED"); v != "" {
		if x, err := strconv.ParseUint(v, 10, 64); err == nil {
			return x
		}
	}
	return 1
}

// headStores reports whether the cache would store a response with this
// Cache-Control value: capture's own two steps (lowercase, parse).
func headStores(v string) bool {
	noStore, _, _ := parseCacheControl(strings.ToLower(v))
	return !noStore
}

// TestCacheControlFloorAgainstMain888 is the property. Random byte-level
// header values over the token characters plus ; , = " \ blanks, the two
// words in several cases, max-age pieces, and CR, LF and NUL that are
// stripped afterwards (so pieces are glued, and a word may be split by one).
// If main's check refuses the value (as it is, or after SetHeader's strip, or
// lowercased by hand), the head refuses it. The oracle is strings.Contains and
// a hand-written lowercase: it reads no directive. CELERIS_CC_FUZZ_N sets the
// number of cases and CELERIS_CC_FUZZ_SEED the seed (the lane's script runs
// 3 million per seed, seeds 1 to 3).
func TestCacheControlFloorAgainstMain888(t *testing.T) {
	n := envInt("CELERIS_CC_FUZZ_N", 1_000_000)
	if testing.Short() {
		n = 20_000
	}
	words := []string{
		"private", "no-store", "Private", "NO-STORE", "pRiVaTe", "No-Store", "PRIVATE",
		"max-age=", "max-age", "60", "0", "-1", "3600", "no-cache", "public", "must-revalidate",
		"s-maxage=", "x-", "ext=", "community=", "a", "z9",
	}
	seps := []string{"", "", "", ",", ", ", " ", ";", "; ", "\n", "\r\n", "\x00", "\t", ",\n", "\n,"}
	punct := `;,="\ ` + "\t"
	const tchars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!#$%&'*+-.^_`|~"
	r := rand.New(rand.NewPCG(envSeed(), 3))
	piece := func(b *strings.Builder) {
		switch k := r.IntN(10); {
		case k < 5:
			w := words[r.IntN(len(words))]
			if r.IntN(6) == 0 && len(w) > 1 { // a word split by a character that is stripped
				at := 1 + r.IntN(len(w)-1)
				w = w[:at] + []string{"\n", "\r\n", "\x00", "\r"}[r.IntN(4)] + w[at:]
			}
			b.WriteString(w)
		case k < 7:
			b.WriteByte(tchars[r.IntN(len(tchars))])
		case k < 9:
			b.WriteByte(punct[r.IntN(len(punct))])
		default:
			b.WriteString(seps[r.IntN(len(seps))])
		}
	}
	var refused, glueOnly, caseOnly, bad int
	for i := 0; i < n; i++ {
		var b strings.Builder
		for j, k := 0, 1+r.IntN(10); j < k; j++ {
			piece(&b)
			if r.IntN(3) == 0 {
				b.WriteString(seps[r.IntN(len(seps))])
			}
		}
		raw := b.String()
		wire := refStrip(raw) // what reaches the cache through SetHeader
		for _, v := range []string{raw, wire} {
			if !refMainRefuses(v) && !refMainRefuses(refLowerASCII(v)) {
				continue
			}
			if headStores(v) {
				if bad++; bad <= 20 {
					t.Errorf("main refuses %q but the head stores it (wire value %q)", v, wire)
				}
			}
		}
		if refMainRefuses(wire) || refMainRefuses(refLowerASCII(wire)) {
			refused++
			if !refMainRefuses(raw) && !refMainRefuses(refLowerASCII(raw)) {
				glueOnly++
			}
			if !refMainRefuses(wire) {
				caseOnly++
			}
		}
	}
	if refused < n/10 || glueOnly == 0 || caseOnly == 0 {
		t.Fatalf("degenerate corpus: %d of %d refused by main's check, %d only after the strip, %d only when lowercased", refused, n, glueOnly, caseOnly)
	}
	t.Logf("%d cases: %d that main's check refuses (%d only after the strip, %d only when lowercased): the head refuses all but %d", n, refused, glueOnly, caseOnly, bad)
}

// TestCacheControlMaxAgeIsDigits888 is the property for max-age: with the
// argument any string over digits, token characters, quotes and blanks, the
// value is stored exactly when the argument is 1*DIGIT and not all zeros. The
// oracle is a digit loop.
func TestCacheControlMaxAgeIsDigits888(t *testing.T) {
	n := envInt("CELERIS_CC_FUZZ_N", 1_000_000)
	if testing.Short() {
		n = 20_000
	}
	const alphabet = "0123456789012345678901234567890123456789abcxyz-+.e\"\\ \t;=_"
	r := rand.New(rand.NewPCG(envSeed(), 4))
	var stored, refused, bad int
	for i := 0; i < n; i++ {
		arg := make([]byte, r.IntN(8))
		for j := range arg {
			arg[j] = alphabet[r.IntN(len(alphabet))]
		}
		a := string(arg)
		cc := []string{"max-age=" + a, "public, max-age=" + a + ", must-revalidate", "max-age=" + a + ", max-age=7"}[r.IntN(3)]
		// the oracle: blanks after the argument are list whitespace; what is
		// left must be digits only, and not all zeros.
		arg1 := strings.TrimRight(a, " \t")
		digits, nonzero := arg1 != "", false
		for k := 0; k < len(arg1); k++ {
			if arg1[k] < '0' || arg1[k] > '9' {
				digits = false
			} else if arg1[k] != '0' {
				nonzero = true
			}
		}
		noStore, maxAge, has := parseCacheControl(cc)
		if digits && nonzero {
			stored++
			if noStore || !has {
				if bad++; bad <= 20 {
					t.Errorf("%q: refused (or no TTL), want stored with the max-age", cc)
				}
			}
			continue
		}
		refused++
		if !noStore || has || maxAge != 0 {
			if bad++; bad <= 20 {
				t.Errorf("%q: stored, want refused: the max-age argument is not 1*DIGIT (or is zero)", cc)
			}
		}
	}
	if stored == 0 || refused == 0 {
		t.Fatalf("degenerate corpus: %d stored, %d refused", stored, refused)
	}
	t.Logf("%d cases: %d stored, %d refused (%d wrong)", n, stored, refused, bad)
}
