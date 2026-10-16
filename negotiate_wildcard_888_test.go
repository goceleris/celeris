package celeris

import "testing"

// celeris#888 items 44, 45 and 40, through the Context methods that use
// internal/negotiate: a "*" entry stands only for what the header does not
// list (RFC 9110 12.5.3), and the weight's name is case-insensitive.
func TestAcceptsEncodingsWildcardAndWeightName888(t *testing.T) {
	for _, tc := range []struct {
		ae     string
		offers []string
		want   string
	}{
		{"gzip, *;q=0", []string{"br", "gzip"}, "gzip"},
		{"br, *;q=0", []string{"gzip", "br"}, "br"},
		{"*;q=0, gzip", []string{"gzip", "br"}, "gzip"},
		{"gzip;q=1.0, identity; q=0.5, *;q=0", []string{"gzip", "br", "identity"}, "gzip"},
		{"gzip;q=1.0, identity; q=0.5, *;q=0", []string{"br", "zstd"}, ""},
		{"gzip;q=1.0, identity; q=0.5, *;q=0", []string{"identity"}, "identity"},
		{"*;q=0", []string{"gzip", "br"}, ""},
		{"gzip;q=0, *", []string{"gzip", "br"}, "br"},
		{"br;Q=0, gzip", []string{"br", "gzip"}, "gzip"},
		{"gzip;Q=0", []string{"gzip"}, ""},
		{"gzip, br;q=0.8", []string{"br", "gzip"}, "gzip"},
	} {
		s, _ := newTestStream("GET", "/test")
		s.Headers = append(s.Headers, [2]string{"accept-encoding", tc.ae})
		c := acquireContext(s)
		if got := c.AcceptsEncodings(tc.offers...); got != tc.want {
			t.Errorf("AcceptsEncodings(%q) with Accept-Encoding %q = %q, want %q", tc.offers, tc.ae, got, tc.want)
		}
		releaseContext(c)
		s.Release()
	}
}

func TestNegotiateMostSpecificRange888(t *testing.T) {
	for _, tc := range []struct {
		accept string
		offers []string
		want   string
	}{
		{"*/*;q=0, text/html", []string{"application/json", "text/html"}, "text/html"},
		{"text/*;q=0, text/html", []string{"text/plain", "text/html"}, "text/html"},
		{"text/html;q=0, text/*", []string{"text/html", "text/plain"}, "text/plain"},
		{"text/*;q=0.1, */*;q=0.9", []string{"text/html", "application/json"}, "application/json"},
		{"application/json;Q=0, */*", []string{"application/json", "application/xml"}, "application/xml"},
	} {
		s, _ := newTestStream("GET", "/test")
		s.Headers = append(s.Headers, [2]string{"accept", tc.accept})
		c := acquireContext(s)
		if got := c.Negotiate(tc.offers...); got != tc.want {
			t.Errorf("Negotiate(%q) with Accept %q = %q, want %q", tc.offers, tc.accept, got, tc.want)
		}
		releaseContext(c)
		s.Release()
	}
}

func TestAcceptsLanguagesWildcard888(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	s.Headers = append(s.Headers, [2]string{"accept-language", "*;q=0, en"})
	defer s.Release()
	c := acquireContext(s)
	defer releaseContext(c)
	if got := c.AcceptsLanguages("fr", "en"); got != "en" {
		t.Errorf("AcceptsLanguages with %q = %q, want en", "*;q=0, en", got)
	}
}
