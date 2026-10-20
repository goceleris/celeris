package compress

import (
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// TestAcceptEncodingWildcardAndWeightName888: "*" stands only for the codings
// the header does not list (RFC 9110 12.5.3), and the weight's name is
// case-insensitive (RFC 9110 12.4.2), so a client that lists the coding it
// takes and refuses the rest with "*;q=0" is sent it, and one that refuses it
// with "Q=0" is not (celeris#888 items 44, 45 and 40).
func TestAcceptEncodingWildcardAndWeightName888(t *testing.T) {
	body := testBody()
	h := func(c *celeris.Context) error { return c.Blob(200, "text/plain", body) }
	for _, tc := range []struct {
		ae     string
		wantCE string
	}{
		{"gzip, *;q=0", "gzip"},
		{"br, *;q=0", "br"},
		{"*;q=0, gzip", "gzip"},
		{"gzip;q=1.0, identity; q=0.5, *;q=0", "gzip"},
		{"*;q=0", ""},
		{"identity, *;q=0", ""},
		{"gzip;q=0, *", "zstd"},
		{"zstd;q=0, br;Q=0, gzip", "gzip"},
		{"zstd;Q=0, br;Q=0, gzip;Q=0", ""},
	} {
		c, rec := celeristest.NewContextT(t, "GET", "/", celeristest.WithHeader("accept-encoding", tc.ae),
			celeristest.WithHandlers(New(), h))
		if err := c.Next(); err != nil {
			t.Fatalf("%q: %v", tc.ae, err)
		}
		if got := rec.Header("content-encoding"); got != tc.wantCE {
			t.Errorf("Accept-Encoding %q: content-encoding %q, want %q", tc.ae, got, tc.wantCE)
		}
	}
}
