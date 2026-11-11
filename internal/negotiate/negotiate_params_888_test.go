package negotiate

import "testing"

// An offer without parameters is weighed by the most specific entry that
// matches it, and an entry without parameters is more specific than one of the
// same type that carries them: "text/html;level=1;q=0, text/html" refuses the
// level-1 variant and still accepts plain text/html (celeris#888 item 98). It
// used to refuse text/html, because both entries ranked alike and q=0 won.
func TestEntryWithoutParametersBeatsOneWith888(t *testing.T) {
	for _, tc := range []struct {
		header string
		offers []string
		want   string
	}{
		{"text/html;level=1;q=0, text/html", []string{"text/html"}, "text/html"},
		{"text/html, text/html;level=1;q=0", []string{"text/html"}, "text/html"},
		{"text/html;level=1;q=0, text/html;q=0.5", []string{"text/html"}, "text/html"},
		{"text/html;level=1, text/html;q=0", []string{"text/html"}, ""},
		{"text/html;q=0, text/html;level=1", []string{"text/html"}, ""},
		{"text/*;foo=1;q=0, text/html", []string{"text/html"}, "text/html"},
		{"text/*;foo=1;q=0, text/*", []string{"text/plain"}, "text/plain"},
		// Parameters do not outrank a more specific type or a different tier.
		{"text/html;level=1;q=0, */*", []string{"text/html"}, ""},
		{"*/*;foo=1;q=0, text/*", []string{"text/html"}, "text/html"},
		{"text/*;foo=1;q=0, */*", []string{"text/html"}, ""},
		{"text/*;foo=1, text/html;q=0", []string{"text/html"}, ""},
		// What an entry with parameters accepted, it still accepts.
		{"application/json;charset=utf-8", []string{"application/json"}, "application/json"},
		{"application/json;charset=utf-8;q=0.5, */*;q=0.1", []string{"text/plain", "application/json"}, "application/json"},
		{"text/html;level=1;q=0.4, text/plain;q=0.3", []string{"text/plain", "text/html"}, "text/html"},
		// A content coding is read the same way.
		{"gzip;foo=1;q=0, gzip", []string{"gzip"}, "gzip"},
		{"gzip;q=0, gzip", []string{"gzip"}, ""},
		{"gzip;foo=1;q=0, br", []string{"gzip", "br"}, "br"},
	} {
		if got := Accept(tc.header, tc.offers); got != tc.want {
			t.Errorf("Accept(%q, %v) = %q, want %q", tc.header, tc.offers, got, tc.want)
		}
	}
}

// Params records a parameter other than the weight, and only that.
func TestParseRecordsParameters888(t *testing.T) {
	for _, tc := range []struct {
		entry string
		want  bool
	}{
		{"text/html", false},
		{"text/html;q=0.5", false},
		{"text/html;Q=0.5", false},
		{"text/html;q=abc", false},
		{"text/html; q = 0", false},
		{"text/html;", false},
		{"text/html;level=1", true},
		{"text/html;level=1;q=0.5", true},
		{"text/html;q=0.5;level=1", true},
		{"application/json;charset=utf-8", true},
		{"gzip;foo", true},
		{"gzip;foo=1;q=0", true},
		{"gzip;qq=1", true},
	} {
		got := Parse(tc.entry)
		if len(got) != 1 || got[0].Params != tc.want {
			t.Errorf("Parse(%q) = %+v, want Params=%v", tc.entry, got, tc.want)
		}
	}
}

// Accept-Encoding as browsers and tools send it, with no parameter other than
// the weight, is answered as it was before entries carried parameters.
func TestAcceptEncodingWithoutParametersIsUnchanged888(t *testing.T) {
	for _, tc := range []struct {
		header string
		offers []string
		want   string
	}{
		{"gzip, deflate, br", []string{"br", "gzip"}, "gzip"},
		{"gzip, deflate, br;q=0.5", []string{"br", "gzip"}, "gzip"},
		{"gzip;q=0, *", []string{"gzip", "br"}, "br"},
		{"gzip, *;q=0", []string{"br", "gzip"}, "gzip"},
		{"gzip;q=0, gzip", []string{"gzip"}, ""},
		{"*, gzip", []string{"br", "gzip"}, "br"},
		{"gzip;q=0.8, br;q=0.8", []string{"br", "gzip"}, "gzip"},
		{"x-gzip", []string{"gzip"}, "gzip"},
		{"identity;q=0, *;q=0", []string{"gzip"}, ""},
	} {
		if got := Accept(tc.header, tc.offers); got != tc.want {
			t.Errorf("Accept(%q, %v) = %q, want %q", tc.header, tc.offers, got, tc.want)
		}
	}
}
