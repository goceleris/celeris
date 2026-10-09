package static

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/middleware/internal/testutil"
)

// Tests for celeris#846 items 1 and 4 on both static paths. With Compress, a
// request that accepts an encoding is answered with the pre-compressed .gz or
// .br variant, and that variant is the representation the client holds and
// resumes. Its validators (ETag, Last-Modified), its 304, its If-Range and its
// Content-Range are therefore the variant's own, not those of the original
// file it was built from.
//
// The corrupt-206 scenario: the variant is rebuilt (gzip writes its own
// timestamp, so the bytes change) and the original keeps its mtime. The client
// holds the first 8 bytes of the old variant and resumes with the
// Last-Modified it was given. Taken from the original, that date still matches
// and the client gets a 206 of the new variant appended to its old head.

var (
	origTime846 = time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	gz1Time846  = time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	gz2Time846  = time.Date(2026, 7, 1, 11, 30, 0, 0, time.UTC)
)

const (
	origSize846 = 100
	gz1Size846  = 40
	gz2Size846  = 56
)

// pc846 is a Compress fixture on one static path: app.js (100 bytes, mtime
// origTime846) with a gzip variant that can be rebuilt on its own.
type pc846 struct {
	mw       celeris.HandlerFunc
	orig     []byte
	gz1      []byte
	gz2      []byte
	origETag string
	rebuild  func() // replaces app.js.gz with gz2 at gz2Time846; app.js is untouched
}

func newPC846(t *testing.T, kind string) *pc846 {
	t.Helper()
	f := &pc846{
		orig: version435(origSize846, 3),
		gz1:  version435(gz1Size846, 0),
		gz2:  version435(gz2Size846, 11),
	}
	// A fixture whose versions share a byte or a length would let a splice
	// compare equal to the new variant.
	if len(f.gz1) == len(f.gz2) || len(f.orig) == len(f.gz1) || len(f.orig) == len(f.gz2) {
		t.Fatal("fixture: lengths must differ")
	}
	for i := range f.gz1 {
		if f.gz1[i] == f.gz2[i] {
			t.Fatalf("fixture: gz1 and gz2 share byte %d", i)
		}
	}
	switch kind {
	case "os":
		dir := t.TempDir()
		write := func(name string, data []byte, mt time.Time) {
			p := filepath.Join(dir, name)
			if err := os.WriteFile(p, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(p, mt, mt); err != nil {
				t.Fatal(err)
			}
		}
		write("app.js", f.orig, origTime846)
		write("app.js.gz", f.gz1, gz1Time846)
		f.rebuild = func() { write("app.js.gz", f.gz2, gz2Time846) }
		f.origETag = weak846(origTime846, origSize846)
		f.mw = New(Config{Root: dir, Compress: true})
	case "fs":
		fsys := fstest.MapFS{
			"app.js":    {Data: f.orig, ModTime: origTime846},
			"app.js.gz": {Data: f.gz1, ModTime: gz1Time846},
		}
		f.rebuild = func() { fsys["app.js.gz"] = &fstest.MapFile{Data: f.gz2, ModTime: gz2Time846} }
		f.origETag = strongTag846(f.orig)
		f.mw = New(Config{FS: fsys, Compress: true})
	default:
		t.Fatal(kind)
	}
	return f
}

func (f *pc846) get(t *testing.T, hdr ...string) *celeristest.ResponseRecorder {
	t.Helper()
	opts := make([]celeristest.Option, 0, len(hdr)/2)
	for i := 0; i+1 < len(hdr); i += 2 {
		opts = append(opts, celeristest.WithHeader(hdr[i], hdr[i+1]))
	}
	rec, err := testutil.RunMiddlewareWithMethod(t, f.mw, "GET", "/app.js", opts...)
	testutil.AssertNoError(t, err)
	return rec
}

func weak846(mt time.Time, size int) string { return fmt.Sprintf(`W/"%x-%x"`, mt.Unix(), size) }

func each846(t *testing.T, fn func(t *testing.T, f *pc846)) {
	t.Helper()
	for _, kind := range []string{"os", "fs"} {
		t.Run(kind, func(t *testing.T) { fn(t, newPC846(t, kind)) })
	}
}

// TestPrecompressedCarriesItsOwnValidators846: the response for the variant
// has the variant's Last-Modified and ETag (its mtime and size), and the
// response for the identity representation still has the original's.
func TestPrecompressedCarriesItsOwnValidators846(t *testing.T) {
	each846(t, func(t *testing.T, f *pc846) {
		rec := f.get(t, "accept-encoding", "gzip")
		testutil.AssertStatus(t, rec, 200)
		testutil.AssertHeader(t, rec, "content-encoding", "gzip")
		testutil.AssertHeader(t, rec, "last-modified", gz1Time846.Format("Mon, 02 Jan 2006 15:04:05 GMT"))
		testutil.AssertHeader(t, rec, "etag", weak846(gz1Time846, gz1Size846))
		if !bytes.Equal(rec.Body, f.gz1) {
			t.Fatalf("variant body %q, want %q", rec.Body, f.gz1)
		}

		// The identity representation keeps the original's: its mtime and size on
		// Root, a hash of its bytes on fs.FS (celeris#846 item 2c).
		rec = f.get(t)
		testutil.AssertStatus(t, rec, 200)
		testutil.AssertNoHeader(t, rec, "content-encoding")
		testutil.AssertHeader(t, rec, "last-modified", origTime846.Format("Mon, 02 Jan 2006 15:04:05 GMT"))
		testutil.AssertHeader(t, rec, "etag", f.origETag)
	})
}

// TestPrecompressedContentType846: the variant is served with the original's
// content type, not the one its .gz extension maps to.
func TestPrecompressedContentType846(t *testing.T) {
	each846(t, func(t *testing.T, f *pc846) {
		plain := f.get(t)
		rec := f.get(t, "accept-encoding", "gzip")
		ct := plain.Header("content-type")
		if ct == "" || rec.Header("content-type") != ct {
			t.Fatalf("variant content-type %q, want the original's %q", rec.Header("content-type"), ct)
		}
	})
}

// TestPrecompressedResumeAfterRebuild846 is the #846 item 1 scenario: only the
// variant is rebuilt. A resume that carries the old variant's Last-Modified
// restarts with the whole new variant; one that carries the current
// Last-Modified gets its 206, cut from the variant.
func TestPrecompressedResumeAfterRebuild846(t *testing.T) {
	each846(t, func(t *testing.T, f *pc846) {
		first := f.get(t, "accept-encoding", "gzip")
		lm1 := first.Header("last-modified")
		if lm1 == "" {
			t.Fatal("no last-modified on the first download")
		}
		f.rebuild()

		rec := f.get(t, "accept-encoding", "gzip", "range", "bytes=8-", "if-range", lm1)
		if rec.StatusCode != 200 || !bytes.Equal(rec.Body, f.gz2) {
			spliced := append(append([]byte(nil), f.gz1[:8]...), rec.Body...)
			t.Fatalf("resume after only the variant was rebuilt: status %d content-range %q; want 200 with the whole new variant (%d bytes).\n"+
				"the client would now hold %q (%d bytes), the new variant is %q",
				rec.StatusCode, rec.Header("content-range"), len(f.gz2), spliced, len(spliced), f.gz2)
		}
		testutil.AssertNoHeader(t, rec, "content-range")
		testutil.AssertHeader(t, rec, "content-encoding", "gzip")

		lm2 := gz2Time846.Format("Mon, 02 Jan 2006 15:04:05 GMT")
		rec = f.get(t, "accept-encoding", "gzip", "range", "bytes=8-", "if-range", lm2)
		testutil.AssertStatus(t, rec, 206)
		testutil.AssertHeader(t, rec, "content-range", fmt.Sprintf("bytes 8-%d/%d", gz2Size846-1, gz2Size846))
		testutil.AssertHeader(t, rec, "content-encoding", "gzip")
		if !bytes.Equal(rec.Body, f.gz2[8:]) {
			t.Fatalf("matching If-Range: body %q, want %q", rec.Body, f.gz2[8:])
		}
	})
}

// TestPrecompressed304IsAboutTheVariant846: a client that holds the old
// variant is not told "not modified" once only the variant was rebuilt; one
// that holds the current variant is, and the 304 names Vary: Accept-Encoding
// and no Content-Encoding.
func TestPrecompressed304IsAboutTheVariant846(t *testing.T) {
	each846(t, func(t *testing.T, f *pc846) {
		first := f.get(t, "accept-encoding", "gzip")
		etag1, lm1 := first.Header("etag"), first.Header("last-modified")

		rec := f.get(t, "accept-encoding", "gzip", "if-none-match", etag1)
		testutil.AssertStatus(t, rec, 304)
		testutil.AssertHeader(t, rec, "etag", etag1)
		testutil.AssertHeader(t, rec, "vary", "Accept-Encoding")
		testutil.AssertNoHeader(t, rec, "content-encoding")
		if len(rec.Body) != 0 {
			t.Fatalf("304 with a %d byte body", len(rec.Body))
		}
		rec = f.get(t, "accept-encoding", "gzip", "if-modified-since", lm1)
		testutil.AssertStatus(t, rec, 304)

		f.rebuild()
		rec = f.get(t, "accept-encoding", "gzip", "if-none-match", etag1)
		if rec.StatusCode != 200 || !bytes.Equal(rec.Body, f.gz2) {
			t.Fatalf("If-None-Match of the old variant after a rebuild: status %d (%d bytes); want 200 with the new variant", rec.StatusCode, len(rec.Body))
		}
		rec = f.get(t, "accept-encoding", "gzip", "if-modified-since", lm1)
		if rec.StatusCode != 200 || !bytes.Equal(rec.Body, f.gz2) {
			t.Fatalf("If-Modified-Since of the old variant after a rebuild: status %d (%d bytes); want 200 with the new variant", rec.StatusCode, len(rec.Body))
		}
		rec = f.get(t, "accept-encoding", "gzip", "if-none-match", rec.Header("etag"))
		testutil.AssertStatus(t, rec, 304)
	})
}

// TestPrecompressedRange416And206846: a range is resolved against the
// variant's bytes. The variant (40 bytes) is shorter than the original (100),
// so a range that the original would satisfy is unsatisfiable on the variant,
// and Content-Range names the variant's size. An unsatisfiable range whose
// If-Range no longer holds is answered with the whole variant, not 416
// (RFC 9110 13.2.2).
func TestPrecompressedRange416And206846(t *testing.T) {
	each846(t, func(t *testing.T, f *pc846) {
		first := f.get(t, "accept-encoding", "gzip")
		lm1 := first.Header("last-modified")
		size := gz1Size846

		for _, tc := range []struct {
			rng, crange string
			want        []byte
		}{
			{"bytes=30-", fmt.Sprintf("bytes 30-%d/%d", size-1, size), f.gz1[30:]},
			{"bytes=0-99", fmt.Sprintf("bytes 0-%d/%d", size-1, size), f.gz1},
			{"bytes=-10", fmt.Sprintf("bytes %d-%d/%d", size-10, size-1, size), f.gz1[size-10:]},
		} {
			for _, ir := range []string{"", lm1} {
				rec := f.get(t, "accept-encoding", "gzip", "range", tc.rng, "if-range", ir)
				if rec.StatusCode != 206 || rec.Header("content-range") != tc.crange || !bytes.Equal(rec.Body, tc.want) {
					t.Fatalf("%s if-range %q: status %d content-range %q (%d bytes); want 206 %q (%d bytes)",
						tc.rng, ir, rec.StatusCode, rec.Header("content-range"), len(rec.Body), tc.crange, len(tc.want))
				}
				testutil.AssertHeader(t, rec, "content-encoding", "gzip")
			}
		}

		// 50 is inside the original and past the end of the variant.
		for _, ir := range []string{"", lm1} {
			rec := f.get(t, "accept-encoding", "gzip", "range", "bytes=50-", "if-range", ir)
			if rec.StatusCode != 416 || rec.Header("content-range") != fmt.Sprintf("bytes */%d", size) || len(rec.Body) != 0 {
				t.Fatalf("bytes=50- if-range %q: status %d content-range %q (%d bytes); want 416 %q, empty",
					ir, rec.StatusCode, rec.Header("content-range"), len(rec.Body), fmt.Sprintf("bytes */%d", size))
			}
			// An empty error body is not a gzip stream.
			testutil.AssertNoHeader(t, rec, "content-encoding")
		}

		// The 304 comes before the range.
		rec := f.get(t, "accept-encoding", "gzip", "if-none-match", first.Header("etag"), "range", "bytes=50-")
		testutil.AssertStatus(t, rec, 304)

		// If-Range of a variant that was since rebuilt: whole new variant,
		// whether the range is satisfiable or not.
		f.rebuild()
		for _, rng := range []string{"bytes=8-", "bytes=500-"} {
			rec := f.get(t, "accept-encoding", "gzip", "range", rng, "if-range", lm1)
			if rec.StatusCode != 200 || !bytes.Equal(rec.Body, f.gz2) || rec.Header("content-range") != "" {
				t.Fatalf("%s with the old variant's If-Range: status %d content-range %q (%d bytes); want 200 with the whole new variant (%d bytes)",
					rng, rec.StatusCode, rec.Header("content-range"), len(rec.Body), len(f.gz2))
			}
		}
		// Past the end of the new variant: 416 with its size.
		rec = f.get(t, "accept-encoding", "gzip", "range", "bytes=56-")
		testutil.AssertStatus(t, rec, 416)
		testutil.AssertHeader(t, rec, "content-range", fmt.Sprintf("bytes */%d", gz2Size846))
	})
}

// TestPrecompressedBrotliValidators846: with both variants present, the one
// served (br) supplies the validators.
func TestPrecompressedBrotliValidators846(t *testing.T) {
	brTime := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	br := version435(30, 5)
	gz := version435(40, 0)
	dir := t.TempDir()
	for name, v := range map[string]struct {
		data []byte
		mt   time.Time
	}{
		"app.js":    {version435(100, 3), origTime846},
		"app.js.gz": {gz, gz1Time846},
		"app.js.br": {br, brTime},
	} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, v.data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, v.mt, v.mt); err != nil {
			t.Fatal(err)
		}
	}
	mw := New(Config{Root: dir, Compress: true})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/app.js",
		celeristest.WithHeader("accept-encoding", "gzip, br"),
		celeristest.WithHeader("range", "bytes=20-"),
		celeristest.WithHeader("if-range", brTime.Format("Mon, 02 Jan 2006 15:04:05 GMT")))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 206)
	testutil.AssertHeader(t, rec, "content-encoding", "br")
	testutil.AssertHeader(t, rec, "content-range", "bytes 20-29/30")
	testutil.AssertHeader(t, rec, "etag", weak846(brTime, 30))
	if !bytes.Equal(rec.Body, br[20:]) {
		t.Fatalf("body %q, want %q", rec.Body, br[20:])
	}
}

// TestOSValidatorsDescribeTheServedFile846 is #846 item 4: the Root path used
// to stat the file, set the validators from that stat, and then let
// Context.File open the path again. A file replaced in between was served
// under the old file's validators. serveOSHook replaces the file in that
// window; the validators must describe the bytes that were sent.
func TestOSValidatorsDescribeTheServedFile846(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "dl.bin")
	v1, v2 := version435(16, 0), version435(24, 13)
	write := func(data []byte, mt time.Time) {
		tmp := p + ".new"
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(tmp, mt, mt); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, p); err != nil { // atomic replace
			t.Fatal(err)
		}
	}
	write(v1, v1Time435)
	mw := New(Config{Root: dir})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/dl.bin")
	testutil.AssertNoError(t, err)
	lm1 := rec.Header("last-modified")
	if lm1 == "" || !bytes.Equal(rec.Body, v1) {
		t.Fatalf("first download: last-modified %q, body %q", lm1, rec.Body)
	}

	// Guard against a vacuous hook: it must run, and once per request.
	calls := 0
	serveOSHook = func() {
		calls++
		write(v2, v2Time435)
	}
	t.Cleanup(func() { serveOSHook = nil })

	rec, err = testutil.RunMiddlewareWithMethod(t, mw, "GET", "/dl.bin",
		celeristest.WithHeader("range", "bytes=8-"), celeristest.WithHeader("if-range", lm1))
	testutil.AssertNoError(t, err)
	serveOSHook = nil
	if calls != 1 {
		t.Fatalf("the hook ran %d times, want 1: the fixture does not replace the file in the window", calls)
	}
	// Whatever bytes went out, the validators that went with them are theirs.
	lm, etag := rec.Header("last-modified"), rec.Header("etag")
	switch {
	case bytes.Equal(rec.Body, v2) && rec.StatusCode == 200:
		testutil.AssertHeader(t, rec, "last-modified", v2Time435.Format("Mon, 02 Jan 2006 15:04:05 GMT"))
		testutil.AssertHeader(t, rec, "etag", weak846(v2Time435, len(v2)))
	default:
		t.Fatalf("status %d content-range %q last-modified %q etag %q, body %q (%d bytes).\n"+
			"the file was replaced between the stat and the open; the resume must restart with the whole new file (200, %d bytes) under the new file's validators, got the client's old head spliced with %q",
			rec.StatusCode, rec.Header("content-range"), lm, etag, rec.Body, len(rec.Body), len(v2), rec.Body)
	}
}

// TestIndexFile304846: a request for a directory is answered with its index
// file, and its 304 is decided against the index file's validators, not the
// directory's.
func TestIndexFile304846(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "index.html")
	write := func(data []byte, mt time.Time) {
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	v1, v2 := version435(30, 0), version435(45, 7)
	write(v1, gz1Time846)
	mw := New(Config{Root: dir})
	get := func(hdr ...string) *celeristest.ResponseRecorder {
		opts := make([]celeristest.Option, 0, len(hdr)/2)
		for i := 0; i+1 < len(hdr); i += 2 {
			opts = append(opts, celeristest.WithHeader(hdr[i], hdr[i+1]))
		}
		rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/", opts...)
		testutil.AssertNoError(t, err)
		return rec
	}
	first := get()
	testutil.AssertHeader(t, first, "etag", weak846(gz1Time846, len(v1)))
	testutil.AssertStatus(t, get("if-none-match", first.Header("etag")), 304)
	write(v2, gz2Time846)
	rec := get("if-none-match", first.Header("etag"))
	if rec.StatusCode != 200 || !bytes.Equal(rec.Body, v2) {
		t.Fatalf("If-None-Match of the old index after it changed: status %d (%d bytes); want 200 with the new index", rec.StatusCode, len(rec.Body))
	}
}
