package cache

import "testing"

// TestForbidsStorageMatchesDirectives888: directives are tokens: a quoted
// comma does not split one, and a longer name is not a shorter one.
func TestForbidsStorageMatchesDirectives888(t *testing.T) {
	for cc, want := range map[string]bool{
		"no-store":                       true,
		"public,no-cache":                true,
		"public , private":               true,
		`no-cache="a, b", max-age=60`:    true,
		`x-ext="no-cache, private"`:      false,
		"x-private-ext":                  false,
		"max-age = 0":                    true,
		"max-age=00":                     true,
		"max-age=05":                     false,
		"max-age=1, s-max-age=0":         false,
		"max-age=3600":                   false,
		"must-revalidate, max-age=10":    false,
		"":                               false,
		"public, max-age=10, immutable,": false,
	} {
		if got := forbidsStorage(cc); got != want {
			t.Errorf("forbidsStorage(%q) = %v, want %v", cc, got, want)
		}
	}
}
