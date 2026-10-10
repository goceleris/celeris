package static

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/middleware/internal/testutil"
)

// Tests for the review of celeris#846 (round 1): a request for a directory is
// answered with its index file, and with Compress the index file's own
// pre-compressed variant, on Root exactly as on fs.FS. The 304, If-Range and
// validators are the variant's, and nothing next to the served file (a sibling
// "<dir>.gz", a "<root>.gz" beside the root) takes part.

var (
	idxTime846    = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	idxGzTime846  = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	outsideTime   = time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)
	httpTime846   = func(t time.Time) string { return t.UTC().Format(http.TimeFormat) }
	idxHTML846    = []byte("<html>identity index of docs, uncompressed</html>")
	idxGz846      = []byte("GZ-INDEX-OF-DOCS")
	siblingGz846  = []byte("an archive of docs, which is no variant of anything served")
	outsideGz846  = []byte("outside the root: a file next to it, not part of the site")
	rootIdxHTML   = []byte("<html>home</html>")
	rootIdxGzData = []byte("GZ-HOME")
)

func writeAt846(t *testing.T, p string, data []byte, mt time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
}

func get846(t *testing.T, mw celeris.HandlerFunc, path string, hdr ...string) *celeristest.ResponseRecorder {
	t.Helper()
	opts := make([]celeristest.Option, 0, len(hdr)/2)
	for i := 0; i+1 < len(hdr); i += 2 {
		opts = append(opts, celeristest.WithHeader(hdr[i], hdr[i+1]))
	}
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", path, opts...)
	testutil.AssertNoError(t, err)
	return rec
}

// idxFixture846 is /docs/ (docs/index.html with a gzip variant) on one static
// path, with a sibling "docs.gz" that is no variant of anything served.
func idxFixture846(t *testing.T, kind string) celeris.HandlerFunc {
	t.Helper()
	return idxFixtureSibling846(t, kind, true)
}

// idxFixtureSibling846 is idxFixture846 with or without the sibling "docs.gz".
func idxFixtureSibling846(t *testing.T, kind string, sibling bool) celeris.HandlerFunc {
	t.Helper()
	switch kind {
	case "os":
		dir := t.TempDir()
		writeAt846(t, filepath.Join(dir, "docs", "index.html"), idxHTML846, idxTime846)
		writeAt846(t, filepath.Join(dir, "docs", "index.html.gz"), idxGz846, idxGzTime846)
		if sibling {
			writeAt846(t, filepath.Join(dir, "docs.gz"), siblingGz846, idxTime846)
		}
		return New(Config{Root: dir, Compress: true})
	case "fs":
		m := fstest.MapFS{
			"docs/index.html":    {Data: idxHTML846, ModTime: idxTime846},
			"docs/index.html.gz": {Data: idxGz846, ModTime: idxGzTime846},
		}
		if sibling {
			m["docs.gz"] = &fstest.MapFile{Data: siblingGz846, ModTime: idxTime846}
		}
		return New(Config{Compress: true, FS: m})
	}
	t.Fatal(kind)
	return nil
}

// TestIndexServesItsVariant846: GET /docs/ with Accept-Encoding gzip is the
// index's gzip variant with the variant's validators and the index's content
// type, on Root and on fs.FS alike.
func TestIndexServesItsVariant846(t *testing.T) {
	for _, kind := range []string{"os", "fs"} {
		for _, sibling := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "", true: "/sibling-docs.gz"}[sibling], func(t *testing.T) {
				rec := get846(t, idxFixtureSibling846(t, kind, sibling), "/docs/", "accept-encoding", "gzip")
				testutil.AssertStatus(t, rec, 200)
				testutil.AssertHeader(t, rec, "content-encoding", "gzip")
				testutil.AssertHeader(t, rec, "last-modified", httpTime846(idxGzTime846))
				testutil.AssertHeader(t, rec, "etag", weak846(idxGzTime846, len(idxGz846)))
				if ct := rec.Header("content-type"); !strings.HasPrefix(ct, "text/html") {
					t.Fatalf("content-type %q, want the index's text/html", ct)
				}
				if !bytes.Equal(rec.Body, idxGz846) {
					t.Fatalf("body %q, want the gzip variant %q", rec.Body, idxGz846)
				}
			})
		}
	}
}

// TestIndex304IsAboutTheVariant846: the 304 of a conditional request for
// /docs/ carries the validators of the variant the 200 carries and is decided
// against them, not against a sibling "docs.gz" or the identity index.
func TestIndex304IsAboutTheVariant846(t *testing.T) {
	for _, kind := range []string{"os", "fs"} {
		t.Run(kind, func(t *testing.T) {
			mw := idxFixture846(t, kind)
			first := get846(t, mw, "/docs/", "accept-encoding", "gzip")
			etag, lm := first.Header("etag"), first.Header("last-modified")

			rec := get846(t, mw, "/docs/", "accept-encoding", "gzip", "if-none-match", etag)
			testutil.AssertStatus(t, rec, 304)
			testutil.AssertHeader(t, rec, "etag", etag)
			testutil.AssertHeader(t, rec, "last-modified", lm)
			testutil.AssertHeader(t, rec, "vary", "Accept-Encoding")

			rec = get846(t, mw, "/docs/", "accept-encoding", "gzip", "if-none-match", "*")
			testutil.AssertStatus(t, rec, 304)
			testutil.AssertHeader(t, rec, "etag", etag)
			testutil.AssertHeader(t, rec, "last-modified", lm)

			// A client that holds the representation as of the sibling's date
			// (older than the variant) is out of date.
			rec = get846(t, mw, "/docs/", "accept-encoding", "gzip", "if-modified-since", httpTime846(idxTime846))
			if rec.StatusCode != 200 || !bytes.Equal(rec.Body, idxGz846) {
				t.Fatalf("If-Modified-Since the sibling's date: %d (last-modified %q etag %q); want 200 with the variant, the variant is newer",
					rec.StatusCode, rec.Header("last-modified"), rec.Header("etag"))
			}
			// The date of the variant itself holds.
			testutil.AssertStatus(t, get846(t, mw, "/docs/", "accept-encoding", "gzip", "if-modified-since", httpTime846(idxGzTime846)), 304)
			// The identity index's tag and the sibling's tag are other representations.
			for _, other := range []string{weak846(idxTime846, len(idxHTML846)), weak846(idxTime846, len(siblingGz846))} {
				rec = get846(t, mw, "/docs/", "accept-encoding", "gzip", "if-none-match", other)
				testutil.AssertStatus(t, rec, 200)
			}
		})
	}
}

// TestIndexRangeIsTheVariants846: a resume of /docs/ is cut from the variant
// and decided against the variant's validators, on both paths.
func TestIndexRangeIsTheVariants846(t *testing.T) {
	for _, kind := range []string{"os", "fs"} {
		t.Run(kind, func(t *testing.T) {
			mw := idxFixture846(t, kind)
			rec := get846(t, mw, "/docs/", "accept-encoding", "gzip", "range", "bytes=4-", "if-range", httpTime846(idxGzTime846))
			testutil.AssertStatus(t, rec, 206)
			if !bytes.Equal(rec.Body, idxGz846[4:]) {
				t.Fatalf("206 body %q, want %q", rec.Body, idxGz846[4:])
			}
			// The date of the sibling or of the identity index is not the variant's.
			rec = get846(t, mw, "/docs/", "accept-encoding", "gzip", "range", "bytes=4-", "if-range", httpTime846(idxTime846))
			if rec.StatusCode != 200 || !bytes.Equal(rec.Body, idxGz846) {
				t.Fatalf("If-Range with another representation's date: %d %q; want the whole variant", rec.StatusCode, rec.Body)
			}
		})
	}
}

// TestIndexRootAndFSAgree846: the two static paths answer the same directory
// request alike.
func TestIndexRootAndFSAgree846(t *testing.T) {
	for _, sibling := range []bool{false, true} {
		for _, hdr := range [][]string{
			{"accept-encoding", "gzip"},
			{"accept-encoding", "gzip", "if-none-match", "*"},
			{"accept-encoding", "gzip", "range", "bytes=2-5"},
			{"accept-encoding", "identity"},
			nil,
		} {
			o := get846(t, idxFixtureSibling846(t, "os", sibling), "/docs/", hdr...)
			f := get846(t, idxFixtureSibling846(t, "fs", sibling), "/docs/", hdr...)
			if o.StatusCode != f.StatusCode || o.Header("content-encoding") != f.Header("content-encoding") ||
				(o.Header("content-encoding") != "" && o.Header("etag") != f.Header("etag")) || !bytes.Equal(o.Body, f.Body) {
				t.Fatalf("sibling=%v %v: Root answers %d ce=%q etag=%q body=%q, fs.FS answers %d ce=%q etag=%q body=%q", sibling, hdr,
					o.StatusCode, o.Header("content-encoding"), o.Header("etag"), o.Body,
					f.StatusCode, f.Header("content-encoding"), f.Header("etag"), f.Body)
			}
		}
	}
}

// TestRootIndexStaysInRoot846: GET / with Compress stats nothing outside the
// root. A file named like the root plus ".gz" beside it is not a variant of
// the index: it is not served, errors nothing, and its mtime and size are in
// no response.
func TestRootIndexStaysInRoot846(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "www")
	writeAt846(t, filepath.Join(root, "index.html"), rootIdxHTML, idxTime846)
	writeAt846(t, filepath.Join(parent, "www.gz"), outsideGz846, outsideTime)
	mw := New(Config{Root: root, Compress: true})

	outsideTag := weak846(outsideTime, len(outsideGz846))
	for _, hdr := range [][]string{
		{"accept-encoding", "gzip"},
		{"accept-encoding", "gzip", "if-none-match", "*"},
		{"accept-encoding", "gzip", "if-modified-since", httpTime846(idxTime846)},
		{"accept-encoding", "gzip", "if-none-match", `"nomatch"`},
	} {
		rec := get846(t, mw, "/", hdr...)
		if rec.StatusCode != 200 && rec.StatusCode != 304 {
			t.Fatalf("GET / %v: %d; the index exists", hdr, rec.StatusCode)
		}
		for _, k := range []string{"etag", "last-modified"} {
			if v := rec.Header(k); v == outsideTag || v == httpTime846(outsideTime) {
				t.Fatalf("GET / %v: %s %q describes %s, a file outside the root", hdr, k, v, filepath.Join(parent, "www.gz"))
			}
		}
		testutil.AssertNoHeader(t, rec, "content-encoding")
		if rec.StatusCode == 200 && !bytes.Equal(rec.Body, rootIdxHTML) {
			t.Fatalf("GET / %v: body %q, want the index", hdr, rec.Body)
		}
	}

	// With the index's variant in the root, GET / gets it.
	writeAt846(t, filepath.Join(root, "index.html.gz"), rootIdxGzData, idxGzTime846)
	rec := get846(t, mw, "/", "accept-encoding", "gzip")
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertHeader(t, rec, "content-encoding", "gzip")
	testutil.AssertHeader(t, rec, "etag", weak846(idxGzTime846, len(rootIdxGzData)))
	if !bytes.Equal(rec.Body, rootIdxGzData) {
		t.Fatalf("GET / body %q, want the index's variant", rec.Body)
	}
}

// outsideFixture846 is a Root with app.js, a directory d and an index whose
// files (and the variant of app.js) are symlinks to a file outside the root,
// which FileFromDir refuses. The outside file has its own date and size.
func outsideFixture846(t *testing.T) (celeris.HandlerFunc, string, string) {
	t.Helper()
	outsideDir := t.TempDir()
	secret := filepath.Join(outsideDir, "secret.bin")
	writeAt846(t, secret, make([]byte, 4242), outsideTime)
	root := t.TempDir()
	writeAt846(t, filepath.Join(root, "app.js"), []byte("console.log(1)"), idxTime846)
	for name, target := range map[string]string{
		"app.js.gz":    secret, // the variant of a regular original
		"leak.txt":     secret, // the original itself
		"d/index.html": secret, // the index of a directory
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	return New(Config{Root: root, Compress: true}), weak846(outsideTime, 4242), httpTime846(outsideTime)
}

// TestRefusedPathLeaksNoValidators846: a path FileFromDir refuses (a symlink
// out of the root) must not leave the target's ETag and Last-Modified on the
// error response. They were set from the stat before the containment check,
// and a conditional request that did not hold carried them out on the 400.
func TestRefusedPathLeaksNoValidators846(t *testing.T) {
	mw, outsideTag, outsideLM := outsideFixture846(t)
	for _, tc := range []struct {
		path string
		hdr  []string
	}{
		{"/app.js", []string{"accept-encoding", "gzip", "if-none-match", `"x"`}},
		{"/app.js", []string{"accept-encoding", "gzip", "if-modified-since", httpTime846(outsideTime.Add(-time.Hour))}},
		{"/app.js", []string{"accept-encoding", "gzip"}},
		{"/leak.txt", []string{"if-none-match", `"x"`}},
		{"/leak.txt", nil},
		{"/d/", []string{"if-none-match", `"x"`}},
		{"/d/", nil},
	} {
		opts := []celeristest.Option{celeristest.WithHandlers(mw)}
		for i := 0; i+1 < len(tc.hdr); i += 2 {
			opts = append(opts, celeristest.WithHeader(tc.hdr[i], tc.hdr[i+1]))
		}
		c, _ := celeristest.NewContextT(t, "GET", tc.path, opts...)
		err := c.Next()
		if err == nil {
			t.Fatalf("GET %s %v: the escaping symlink was served (%d)", tc.path, tc.hdr, c.ResponseStatus())
		}
		for _, h := range c.ResponseHeaders() {
			if h[1] == outsideTag || h[1] == outsideLM {
				t.Errorf("GET %s %v: the error response carries %s: %s, a validator of a file outside the root", tc.path, tc.hdr, h[0], h[1])
			}
		}
	}
}

// TestOversizeVariantIsNotEncoded846: a pre-compressed variant over the size
// limit is refused with a 413 whose plain-text body is not gzip, so the error
// carries neither the variant's Content-Encoding nor its validators.
func TestOversizeVariantIsNotEncoded846(t *testing.T) {
	root := t.TempDir()
	writeAt846(t, filepath.Join(root, "app.js"), []byte("console.log(1)"), idxTime846)
	big := filepath.Join(root, "app.js.gz")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(101 << 20); err != nil { // sparse: over the 100 MB limit
		_ = f.Close()
		t.Fatal(err)
	}
	_ = f.Close()
	mw := New(Config{Root: root, Compress: true})
	c, _ := celeristest.NewContextT(t, "GET", "/app.js", celeristest.WithHandlers(mw), celeristest.WithHeader("accept-encoding", "gzip"))
	err = c.Next()
	var he *celeris.HTTPError
	if !errors.As(err, &he) || he.Code != 413 {
		t.Fatalf("err %v; want the 413 for a file over the limit", err)
	}
	for _, h := range c.ResponseHeaders() {
		switch h[0] {
		case "content-encoding", "etag", "last-modified":
			t.Errorf("the 413 carries %s: %s, which describe a file that is not sent", h[0], h[1])
		}
	}
}
