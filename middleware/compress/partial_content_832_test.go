package compress

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// TestCompressLeavesPartialContentAlone832 is celeris#832 for compress. A
// 206's Content-Range counts bytes of the identity representation, but the
// middleware compressed the part like any 2xx and left Content-Range as it
// was: Content-Encoding gzip over 55 bytes that claimed to be bytes
// 0-4999/10000. A range is over the encoded representation once a
// Content-Encoding applies (RFC 9110 §14.1.2), so no client could reassemble
// it. A 206 goes through untouched, still with Vary: Accept-Encoding; a full
// response from the same handler is still compressed.
func TestCompressLeavesPartialContentAlone832(t *testing.T) {
	content := []byte(strings.Repeat("0123456789", 1000))
	p := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
	mw := New()
	h := func(c *celeris.Context) error { return c.File(p) }
	run := func(opts ...celeristest.Option) *celeristest.ResponseRecorder {
		all := append([]celeristest.Option{celeristest.WithHandlers(mw, h), celeristest.WithHeader("accept-encoding", "gzip")}, opts...)
		c, rec := celeristest.NewContextT(t, "GET", "/f", all...)
		if err := c.Next(); err != nil {
			t.Fatalf("Next: %v", err)
		}
		return rec
	}

	// The control: the full response is compressed.
	full := run()
	if full.StatusCode != 200 || full.Header("content-encoding") != "gzip" {
		t.Fatalf("full GET: %d content-encoding %q, want 200 gzip", full.StatusCode, full.Header("content-encoding"))
	}
	if got := decompressGzip(t, full.Body); !bytes.Equal(got, content) {
		t.Fatalf("full GET: gzip body decodes to %d bytes, want the %d-byte file", len(got), len(content))
	}

	part := run(celeristest.WithHeader("range", "bytes=0-4999"))
	if part.StatusCode != 206 || part.Header("content-range") != "bytes 0-4999/10000" {
		t.Fatalf("ranged GET: %d content-range %q, want 206 bytes 0-4999/10000", part.StatusCode, part.Header("content-range"))
	}
	if ce := part.Header("content-encoding"); ce != "" {
		t.Errorf("ranged GET: Content-Encoding %q on a 206 whose Content-Range counts identity bytes (body %d bytes), want none", ce, len(part.Body))
	}
	if !bytes.Equal(part.Body, content[:5000]) {
		t.Errorf("ranged GET: body is %d bytes and not bytes 0-4999 of the file", len(part.Body))
	}
	if v := part.Header("vary"); !strings.Contains(strings.ToLower(v), "accept-encoding") {
		t.Errorf("ranged GET: Vary %q, want Accept-Encoding (the full response varies on it)", v)
	}
}
