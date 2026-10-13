package cache

import "testing"

// TestParseCacheControlMatchesDirectives888: directives are tokens: a quoted
// comma does not split one, and a longer name is not a shorter one (no-cache,
// max-age=0). "private" and "no-store" are the exception since round 5: the
// floor refuses them wherever they are, as main did.
func TestParseCacheControlMatchesDirectives888(t *testing.T) {
	for cc, want := range map[string]bool{
		"no-store":                       true,
		"public,no-cache":                true,
		"public , private":               true,
		`no-cache="a, b", max-age=60`:    true,
		`x-ext="no-cache, private"`:      true, // round 5: the floor, private anywhere (as main)
		"x-private-ext":                  true,
		`x-ext="no-cache"`:               false,
		"x-no-cache-ext":                 false,
		"max-age = 0":                    true,
		"max-age=00":                     true,
		"max-age=05":                     false,
		"max-age=1, s-max-age=0":         false,
		"max-age=3600":                   false,
		"must-revalidate, max-age=10":    false,
		"":                               false,
		"public, max-age=10, immutable,": false,
		"x-max-age=0":                    false,
		`ext="max-age=0", max-age=60`:    false,
		"x-max-age=0, max-age=1":         false,
	} {
		if got, _, _ := parseCacheControl(cc); got != want {
			t.Errorf("parseCacheControl(%q) stores nothing = %v, want %v", cc, got, want)
		}
	}
}
