package negotiate

import "testing"

// celeris#888 items 44, 45 and 40. RFC 9110 12.5.3: "*" matches any content
// coding not listed elsewhere in the field, so a coding that is listed (with a
// q-value) is judged by its own entry, whatever a wildcard entry says;
// RFC 9110 12.5.1 gives a media range the same rule (the most specific range
// that matches a type decides its q). RFC 9110 5.6.6 and 12.4.2: a parameter
// name is case-insensitive, so "Q=0" is a q-value.
var wildcard888 = []struct {
	name   string
	header string
	offers []string
	want   string
}{
	// Item 44: a "*;q=0" entry refuses what is not listed, not what is.
	{"gzip-then-star-q0", "gzip, *;q=0", []string{"gzip", "br"}, "gzip"},
	{"br-then-star-q0", "br, *;q=0", []string{"gzip", "br"}, "br"},
	{"star-q0-then-gzip", "*;q=0, gzip", []string{"gzip", "br"}, "gzip"},
	{"star-q0-then-gzip-offer-order", "*;q=0, gzip", []string{"br", "gzip"}, "gzip"},
	{"rfc9110-12.5.3-example/gzip", "gzip;q=1.0, identity; q=0.5, *;q=0", []string{"gzip", "br", "deflate"}, "gzip"},
	{"rfc9110-12.5.3-example/unlisted", "gzip;q=1.0, identity; q=0.5, *;q=0", []string{"br", "deflate", "zstd"}, ""},
	{"rfc9110-12.5.3-example/identity", "gzip;q=1.0, identity; q=0.5, *;q=0", []string{"identity"}, "identity"},
	{"star-q0-alone", "*;q=0", []string{"gzip", "br"}, ""},
	{"star-q0-listed-q0", "gzip;q=0, *;q=0", []string{"gzip", "br"}, ""},
	{"star-q0-listed-small-q", "br;q=0.1, *;q=0", []string{"gzip", "br"}, "br"},
	{"listed-q0-star-positive", "gzip;q=0, *", []string{"gzip", "br"}, "br"},
	{"listed-q0-star-positive-only-gzip", "gzip;q=0, *", []string{"gzip"}, ""},
	{"star-q0-media", "*/*;q=0, text/html", []string{"text/html", "text/plain"}, "text/html"},
	{"language", "*;q=0, en", []string{"fr", "en"}, "en"},
	// The most specific entry decides a positive q-value too.
	{"listed-lower-than-star", "gzip;q=0.5, *;q=1", []string{"gzip", "br"}, "br"},
	{"listed-lower-than-star-only-gzip", "gzip;q=0.5, *;q=1", []string{"gzip"}, "gzip"},
	{"listed-higher-than-star", "gzip, *;q=0.5", []string{"br", "gzip"}, "gzip"},
	{"type-wildcard-q0-exact-wins", "text/*;q=0, text/html", []string{"text/html", "text/plain"}, "text/html"},
	{"exact-q0-type-wildcard", "text/html;q=0, text/*", []string{"text/html", "text/plain"}, "text/plain"},
	{"type-wildcard-over-star", "text/*;q=0.1, */*;q=0.9", []string{"text/html", "application/json"}, "application/json"},
	{"type-wildcard-over-star-q0", "text/*;q=0, */*", []string{"text/html", "application/json"}, "application/json"},
	{"exact-over-type-over-star", "*/*;q=0.2, text/*;q=0.3, text/html;q=0.4", []string{"application/json", "text/plain", "text/html"}, "text/html"},
	// What the rewrite must keep.
	{"star-q0-after-q0", "gzip;q=0, deflate, *;q=0", []string{"gzip", "deflate", "br"}, "deflate"},
	{"star-with-listed-q0-after", "*, br;q=0", []string{"br", "gzip"}, "gzip"},
	{"star-only", "*", []string{"br", "gzip"}, "br"},
	{"tie-keeps-header-order", "gzip, br", []string{"br", "gzip"}, "gzip"},
	{"tie-keeps-offer-order-for-star", "*", []string{"gzip", "br"}, "gzip"},
	// Equal q-values between offers: the entry that gives the q-value, the
	// earliest in the header whatever its specificity, then the offer order
	// (what origin/main did). A wildcard before the named entry is no
	// reason to prefer the offers only the wildcard reaches.
	{"tie-wildcard-first-media", "*/*, application/json", []string{"application/json", "application/xml", "text/plain"}, "application/json"},
	{"tie-wildcard-first-media-named-last-offer", "*/*, application/json", []string{"application/xml", "application/json", "text/plain"}, "application/xml"},
	{"tie-star-first-coding", "*, gzip", []string{"gzip", "br"}, "gzip"},
	{"tie-star-first-coding-offer-order", "*, gzip", []string{"br", "gzip"}, "br"},
	{"tie-type-wildcard-first", "text/*, text/html", []string{"text/html", "text/plain"}, "text/html"},
	{"tie-type-wildcard-first-offer-order", "text/*, text/html", []string{"text/plain", "text/html"}, "text/plain"},
	{"tie-low-wildcard-then-named", "*;q=0.1, gzip, br", []string{"br", "gzip"}, "gzip"},
	{"tie-low-wildcard-then-named-q-lower-later", "*;q=0.1, gzip, br;q=0.5", []string{"br", "gzip"}, "gzip"},
	{"tie-named-first-wildcard-last", "gzip, *", []string{"br", "gzip"}, "gzip"},
	{"tie-language-wildcard-first", "*, en", []string{"fr", "en"}, "fr"},
	// Entries equally specific (a repeated coding): q=0 refuses, whatever the
	// order, and otherwise the higher q-value counts.
	{"repeated-q0-first", "gzip;q=0, gzip", []string{"gzip"}, ""},
	{"repeated-q0-last", "gzip, gzip;q=0", []string{"gzip"}, ""},
	{"repeated-q0-between", "gzip, gzip;q=0, gzip;q=0.5", []string{"gzip", "br"}, ""},
	{"repeated-higher-q-counts", "gzip;q=0.2, br;q=0.5, gzip;q=0.8", []string{"br", "gzip"}, "gzip"},
	{"repeated-wildcard-q0", "*;q=0.5, *;q=0, br", []string{"gzip", "br"}, "br"},
	// Item 45 (and 40): the weight's name is case-insensitive.
	{"upper-q0-refuses", "br;Q=0, gzip", []string{"br", "gzip"}, "gzip"},
	{"upper-q0-alone", "gzip;Q=0", []string{"gzip"}, ""},
	{"upper-q-weight", "gzip;Q=0.3, br;q=0.4", []string{"gzip", "br"}, "br"},
	{"upper-q-weight-wins", "gzip;Q=0.5, br;q=0.4", []string{"br", "gzip"}, "gzip"},
	{"blank-before-upper-q", "gzip; Q=0", []string{"gzip"}, ""},
	{"upper-q-after-param", "text/html;level=1;Q=0, text/plain", []string{"text/html", "text/plain"}, "text/plain"},
	{"star-upper-q0", "gzip, *;Q=0", []string{"gzip", "br"}, "gzip"},
}

func TestAcceptWildcardMatchesOnlyUnlisted888(t *testing.T) {
	for _, tc := range wildcard888 {
		t.Run(tc.name, func(t *testing.T) {
			if got := Accept(tc.header, tc.offers); got != tc.want {
				t.Errorf("Accept(%q, %q) = %q, want %q", tc.header, tc.offers, got, tc.want)
			}
		})
	}
}

func TestParseWeightNameCaseInsensitive888(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   float64
	}{
		{"gzip;q=0.5", 0.5}, {"gzip;Q=0.5", 0.5}, {"gzip; Q=0.5", 0.5}, {"gzip;foo=1;Q=0.25", 0.25},
		{"gzip;Q=0", 0}, {"gzip;q=0", 0},
		// Not weights: no change from before.
		{"gzip;qq=0", 1}, {"gzip;q", 1}, {"gzip;Q", 1}, {"gzip;q = 0", 1}, {"gzip;x=q=0", 1},
	} {
		got := Parse(tc.header)
		if len(got) != 1 || got[0].MediaType != "gzip" || got[0].Quality != tc.want {
			t.Errorf("Parse(%q) = %+v, want gzip at q=%v", tc.header, got, tc.want)
		}
	}
}
