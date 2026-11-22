package etag

import (
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// TestNotModifiedForRangedRequestDropsContentRange888: a handler that answers
// a Range request with 206, its own representation tag and a Content-Range
// gets 304 when If-None-Match matches (celeris#832, RFC 9110 13.2.2), but the
// 304 carried the 206's Content-Range, which means nothing on a 304 (RFC 9110
// 14.4: it belongs to 206 and 416) and tells a cache the 304's "part".
func TestNotModifiedForRangedRequestDropsContentRange888(t *testing.T) {
	mw := New()
	own := func(c *celeris.Context) error {
		c.SetHeader("etag", `"v1"`)
		c.SetHeader("content-range", "bytes 0-3/10")
		c.SetHeader("cache-control", "max-age=60")
		c.SetHeader("accept-ranges", "bytes")
		return c.Blob(206, "text/plain", []byte("0123"))
	}
	ranged := celeristest.WithHeader("range", "bytes=0-3")

	r := run832(t, mw, own, ranged, celeristest.WithHeader("if-none-match", `"v1"`))
	if r.StatusCode != 304 || len(r.Body) != 0 {
		t.Fatalf("ranged GET with the matching tag: %d %q, want 304 with no body", r.StatusCode, r.Body)
	}
	if cr := r.Header("content-range"); cr != "" {
		t.Errorf("the 304 carries Content-Range %q, want none", cr)
	}
	// What a 304 does carry (RFC 9110 15.4.5) is untouched.
	if r.Header("etag") != `"v1"` || r.Header("cache-control") != "max-age=60" || r.Header("accept-ranges") != "bytes" {
		t.Errorf("the 304 lost its validators: etag %q cache-control %q accept-ranges %q",
			r.Header("etag"), r.Header("cache-control"), r.Header("accept-ranges"))
	}

	// The part itself, with the non-matching tag, keeps its Content-Range.
	p := run832(t, mw, own, ranged, celeristest.WithHeader("if-none-match", `"v2"`))
	if p.StatusCode != 206 || p.Header("content-range") != "bytes 0-3/10" {
		t.Errorf("ranged GET with another tag: %d content-range %q, want 206 bytes 0-3/10", p.StatusCode, p.Header("content-range"))
	}

	// The unranged request, same handler shape: a 200 with the full tag.
	full := func(c *celeris.Context) error {
		c.SetHeader("etag", `"v1"`)
		return c.Blob(200, "text/plain", []byte("0123456789"))
	}
	if f := run832(t, mw, full, celeristest.WithHeader("if-none-match", `"v1"`)); f.StatusCode != 304 || f.Header("content-range") != "" {
		t.Errorf("unranged 304: %d content-range %q", f.StatusCode, f.Header("content-range"))
	}
}
