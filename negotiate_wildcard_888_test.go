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
		// Equal q-values: the entry that gives the q-value decides, the
		// earliest in the header, whatever its specificity (as on main).
		{"*, gzip", []string{"gzip", "br"}, "gzip"},
		{"*, gzip", []string{"br", "gzip"}, "br"},
		{"*;q=0.1, gzip, br", []string{"br", "gzip"}, "gzip"},
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
		// Equal q-values with a wildcard first: the named entry's offer
		// stays first in line (as on main), not the ones only "*/*" reaches.
		{"*/*, application/json", []string{"application/json", "application/xml", "text/plain"}, "application/json"},
		{"text/*, text/html", []string{"text/html", "text/plain"}, "text/html"},
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

// Context.Respond offers json, xml and text/plain: "*/*, application/json"
// ties all three at q=1 and the entry that gives the q-value, the earliest in
// the header, is the same one for all of them, so the first offer (json) wins,
// as it did before the most specific entry decided the weight.
func TestRespondWildcardFirstServesJSON888(t *testing.T) {
	for _, accept := range []string{"*/*, application/json", "*/*;q=0.8, application/json;q=0.8", "*, application/json"} {
		s, rw := newTestStream("GET", "/test")
		s.Headers = append(s.Headers, [2]string{"accept", accept})
		c := acquireContext(s)
		if err := c.Respond(200, respondItem888{"v"}); err != nil {
			t.Fatal(err)
		}
		var ct string
		for _, h := range rw.headers {
			if h[0] == "content-type" {
				ct = h[1]
			}
		}
		if ct != "application/json" {
			t.Errorf("Respond with Accept %q: content-type %q, want application/json", accept, ct)
		}
		releaseContext(c)
		s.Release()
	}
}

type respondItem888 struct{ K string }
