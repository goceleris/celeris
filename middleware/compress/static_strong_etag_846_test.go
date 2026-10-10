package compress

import (
	"bytes"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/middleware/static"
)

// TestCompressWeakensStaticStrongETag846: static serves an fs.FS file with a
// strong ETag (celeris#846), a hash of the identity bytes. compress in front
// of it encodes the 200 and so weakens that tag, as it does any strong tag; a
// weak tag never matches If-Range, so a client that resumes the compressed
// download is sent the whole file again and never a 206 cut from the identity
// bytes and appended to its compressed head. A client that holds the strong
// tag from an identity response still resumes that one.
func TestCompressWeakensStaticStrongETag846(t *testing.T) {
	content := []byte(strings.Repeat("0123456789", 1000))
	mod := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	mw := static.New(static.Config{FS: fstest.MapFS{"f.txt": {Data: content, ModTime: mod}}})
	run := func(opts ...celeristest.Option) *celeristest.ResponseRecorder {
		t.Helper()
		all := append([]celeristest.Option{celeristest.WithHandlers(New(), mw)}, opts...)
		c, rec := celeristest.NewContextT(t, "GET", "/f.txt", all...)
		if err := c.Next(); err != nil {
			t.Fatalf("Next: %v", err)
		}
		return rec
	}

	identity := run()
	strong := identity.Header("etag")
	if identity.StatusCode != 200 || strong == "" || strings.HasPrefix(strong, "W/") || identity.Header("content-encoding") != "" {
		t.Fatalf("identity GET: %d etag %q content-encoding %q; want 200 with a strong tag and no encoding",
			identity.StatusCode, strong, identity.Header("content-encoding"))
	}

	gz := run(celeristest.WithHeader("accept-encoding", "gzip"))
	if gz.StatusCode != 200 || gz.Header("content-encoding") != "gzip" || gz.Header("etag") != "W/"+strong {
		t.Fatalf("gzip GET: %d content-encoding %q etag %q; want 200 gzip with %q",
			gz.StatusCode, gz.Header("content-encoding"), gz.Header("etag"), "W/"+strong)
	}
	resume := run(celeristest.WithHeader("accept-encoding", "gzip"),
		celeristest.WithHeader("range", "bytes=100-"), celeristest.WithHeader("if-range", gz.Header("etag")))
	if resume.StatusCode != 200 || resume.Header("content-range") != "" || resume.Header("content-encoding") != "gzip" {
		t.Fatalf("resume of the compressed download: %d content-range %q content-encoding %q; want the whole file again as a 200 gzip",
			resume.StatusCode, resume.Header("content-range"), resume.Header("content-encoding"))
	}

	part := run(celeristest.WithHeader("range", "bytes=100-"), celeristest.WithHeader("if-range", strong))
	if part.StatusCode != 206 || !bytes.Equal(part.Body, content[100:]) || part.Header("content-encoding") != "" {
		t.Fatalf("resume of the identity download: %d content-encoding %q (%d bytes); want 206 of the identity bytes",
			part.StatusCode, part.Header("content-encoding"), len(part.Body))
	}
}
