package etag

import (
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

func run832(t *testing.T, mw, h celeris.HandlerFunc, opts ...celeristest.Option) *celeristest.ResponseRecorder {
	t.Helper()
	all := append([]celeristest.Option{celeristest.WithHandlers(mw, h)}, opts...)
	c, rec := celeristest.NewContextT(t, "GET", "/f", all...)
	if err := c.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}
	return rec
}

// TestEtagLeavesPartialContentAlone832 is celeris#832 for etag. The
// middleware hashes the buffered body, and for a 206 that body is the part:
// the 206 carried an ETag that is not the representation's, and an
// If-None-Match with that tag on a ranged request was answered 304. A 206
// goes through untouched: no tag of the middleware's, no If-None-Match
// evaluation (the middleware cannot know the representation's tag from a
// part), and a tag the handler set itself is kept.
func TestEtagLeavesPartialContentAlone832(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(p, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	mw := New()
	h := func(c *celeris.Context) error { return c.File(p) }

	// The control: a full response is tagged, and its tag answers 304.
	full := run832(t, mw, h)
	if full.StatusCode != 200 || full.Header("etag") == "" {
		t.Fatalf("full GET: %d etag %q, want 200 with an ETag", full.StatusCode, full.Header("etag"))
	}
	if r := run832(t, mw, h, celeristest.WithHeader("if-none-match", full.Header("etag"))); r.StatusCode != 304 {
		t.Fatalf("full GET with If-None-Match of its own tag: %d, want 304", r.StatusCode)
	}

	part := run832(t, mw, h, celeristest.WithHeader("range", "bytes=0-3"))
	if part.StatusCode != 206 || string(part.Body) != "0123" || part.Header("content-range") != "bytes 0-3/10" {
		t.Fatalf("ranged GET: %d %q content-range %q, want 206 \"0123\" bytes 0-3/10", part.StatusCode, part.Body, part.Header("content-range"))
	}
	if got := part.Header("etag"); got != "" {
		t.Errorf("ranged GET: the 206 got ETag %q from the middleware, want none (the full response's is %q)", got, full.Header("etag"))
	}

	// What main answered 304 to: If-None-Match with the tag of the part.
	var buf [14]byte
	partTag := string(buf[:writeCRC32ETag(&buf, crc32.ChecksumIEEE([]byte("0123")), true)])
	inm := run832(t, mw, h, celeristest.WithHeader("range", "bytes=0-3"), celeristest.WithHeader("if-none-match", partTag))
	if inm.StatusCode != 206 || string(inm.Body) != "0123" {
		t.Errorf("ranged GET with If-None-Match %s (the part's hash): %d %q, want 206 \"0123\"", partTag, inm.StatusCode, inm.Body)
	}

	// A handler's own tag on a 206 is kept as it is.
	own := func(c *celeris.Context) error {
		c.SetHeader("etag", `"v1"`)
		c.SetHeader("content-range", "bytes 0-3/10")
		return c.Blob(206, "text/plain", []byte("0123"))
	}
	if r := run832(t, mw, own); r.StatusCode != 206 || r.Header("etag") != `"v1"` {
		t.Errorf("206 with the handler's own ETag: %d etag %q, want 206 \"v1\"", r.StatusCode, r.Header("etag"))
	}
}
