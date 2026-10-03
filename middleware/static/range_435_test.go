package static

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"testing/fstest"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/middleware/internal/testutil"
)

// Tests for celeris#435 on both static paths: a Root directory (served by
// Context.FileFromDir) and an fs.FS (served from the per-file cache).
//
// The corrupt-206 scenario: a client downloads part of a file, the file
// changes, and the client resumes with "Range: bytes=<what it has>-" and
// "If-Range: <the Last-Modified it was given>". The validator no longer
// matches, so RFC 9110 §13.1.5 says to send the whole new file as a 200.
// Before the fix If-Range was never read: the client got a 206 with the new
// file's bytes from its offset on and spliced them onto the old file's head.

// version435 builds a patterned body of n bytes whose every byte depends on
// seed, so two versions never share a byte at the same offset.
func version435(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'A' + (seed+byte(i))%26
	}
	return b
}

var (
	v1Time435 = time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	v2Time435 = time.Date(2026, 7, 1, 11, 30, 0, 0, time.UTC)
)

// resumeCheck435 runs the resume after the change and fails with the bytes
// the client would have ended up with.
func resumeCheck435(t *testing.T, mw celeris.HandlerFunc, v1, v2 []byte, have int, validator string) {
	t.Helper()
	// Guard against a vacuous fixture: the two versions differ in length
	// and at every offset the client holds, so a splice can never equal v2.
	if len(v1) == len(v2) || have >= len(v1) {
		t.Fatalf("fixture: v1 %d bytes, v2 %d bytes, have %d", len(v1), len(v2), have)
	}
	for i := range have {
		if v1[i] == v2[i] {
			t.Fatalf("fixture: v1 and v2 share byte %d", i)
		}
	}
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/dl.bin",
		celeristest.WithHeader("range", "bytes="+strconv.Itoa(have)+"-"),
		celeristest.WithHeader("if-range", validator))
	testutil.AssertNoError(t, err)
	if rec.StatusCode != 200 || !bytes.Equal(rec.Body, v2) {
		spliced := append(append([]byte(nil), v1[:have]...), rec.Body...)
		t.Fatalf("resume after the file changed: status %d content-range %q; want 200 with the whole new file (%d bytes).\n"+
			"the client would now hold %q (%d bytes), the new file is %q",
			rec.StatusCode, rec.Header("content-range"), len(v2), spliced, len(spliced), v2)
	}
	testutil.AssertNoHeader(t, rec, "content-range")
}

func TestResumeAfterChangeOS435(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "dl.bin")
	v1, v2 := version435(16, 0), version435(24, 13)
	if err := os.WriteFile(p, v1, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, v1Time435, v1Time435); err != nil {
		t.Fatal(err)
	}
	mw := New(Config{Root: dir})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/dl.bin")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	lm1 := rec.Header("last-modified")
	if lm1 == "" || !bytes.Equal(rec.Body, v1) {
		t.Fatalf("first download: last-modified %q, body %q", lm1, rec.Body)
	}

	// The file changes (new bytes, new length, a later mtime).
	if err := os.WriteFile(p, v2, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, v2Time435, v2Time435); err != nil {
		t.Fatal(err)
	}
	resumeCheck435(t, mw, v1, v2, 8, lm1)

	// The current validator still gets its range.
	lm2 := v2Time435.Format(http.TimeFormat)
	rec, err = testutil.RunMiddlewareWithMethod(t, mw, "GET", "/dl.bin",
		celeristest.WithHeader("range", "bytes=8-"), celeristest.WithHeader("if-range", lm2))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 206)
	testutil.AssertHeader(t, rec, "content-range", "bytes 8-23/24")
	if !bytes.Equal(rec.Body, v2[8:]) {
		t.Fatalf("matching If-Range: body %q, want %q", rec.Body, v2[8:])
	}
}

func TestResumeAfterChangeFS435(t *testing.T) {
	v1, v2 := version435(16, 0), version435(24, 13)
	fsys := fstest.MapFS{"dl.bin": {Data: v1, ModTime: v1Time435}}
	mw := New(Config{FS: fsys})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/dl.bin")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	lm1 := rec.Header("last-modified")
	if lm1 == "" || !bytes.Equal(rec.Body, v1) {
		t.Fatalf("first download: last-modified %q, body %q", lm1, rec.Body)
	}

	fsys["dl.bin"] = &fstest.MapFile{Data: v2, ModTime: v2Time435}
	resumeCheck435(t, mw, v1, v2, 8, lm1)

	lm2 := v2Time435.Format(http.TimeFormat)
	rec, err = testutil.RunMiddlewareWithMethod(t, mw, "GET", "/dl.bin",
		celeristest.WithHeader("range", "bytes=8-"), celeristest.WithHeader("if-range", lm2))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 206)
	testutil.AssertHeader(t, rec, "content-range", "bytes 8-23/24")
	if !bytes.Equal(rec.Body, v2[8:]) {
		t.Fatalf("matching If-Range: body %q, want %q", rec.Body, v2[8:])
	}
}

// TestIfRangeWeakETag435: static's ETag is weak (mtime-size), and If-Range
// uses the strong comparison, under which a weak tag never matches: the whole
// file is sent even though nothing changed (a client must not send a weak
// tag in If-Range at all, §13.1.5).
func TestIfRangeWeakETag435(t *testing.T) {
	v1 := version435(16, 0)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "dl.bin"), v1, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, mw := range map[string]celeris.HandlerFunc{
		"os": New(Config{Root: dir}),
		"fs": New(Config{FS: fstest.MapFS{"dl.bin": {Data: v1, ModTime: v1Time435}}}),
	} {
		t.Run(name, func(t *testing.T) {
			rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/dl.bin")
			testutil.AssertNoError(t, err)
			etag := rec.Header("etag")
			if len(etag) < 3 || etag[:2] != "W/" {
				t.Fatalf("static etag %q: this test assumes a weak tag", etag)
			}
			rec, err = testutil.RunMiddlewareWithMethod(t, mw, "GET", "/dl.bin",
				celeristest.WithHeader("range", "bytes=8-"), celeristest.WithHeader("if-range", etag))
			testutil.AssertNoError(t, err)
			testutil.AssertStatus(t, rec, 200)
			testutil.AssertNoHeader(t, rec, "content-range")
			if !bytes.Equal(rec.Body, v1) {
				t.Fatalf("body %q, want the whole file", rec.Body)
			}
		})
	}
}

// TestUnsatisfiableRange435: a range that starts at or past the end gets 416
// with "Content-Range: bytes */<size>" and no body, on both paths; a last-pos
// past the end is cut to the file.
func TestUnsatisfiableRange435(t *testing.T) {
	v1 := version435(16, 0)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "dl.bin"), v1, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, mw := range map[string]celeris.HandlerFunc{
		"os": New(Config{Root: dir}),
		"fs": New(Config{FS: fstest.MapFS{"dl.bin": {Data: v1, ModTime: v1Time435}}}),
	} {
		t.Run(name+"/416", func(t *testing.T) {
			for _, rng := range []string{"bytes=16-", "bytes=100-200", "bytes=-0"} {
				rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/dl.bin",
					celeristest.WithHeader("range", rng))
				testutil.AssertNoError(t, err)
				if rec.StatusCode != 416 || rec.Header("content-range") != "bytes */16" || len(rec.Body) != 0 {
					t.Fatalf("%s: status %d content-range %q body %d bytes; want 416, %q, empty",
						rng, rec.StatusCode, rec.Header("content-range"), len(rec.Body), "bytes */16")
				}
			}
		})
		t.Run(name+"/206-cut-to-file", func(t *testing.T) {
			rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/dl.bin",
				celeristest.WithHeader("range", "bytes=12-100"))
			testutil.AssertNoError(t, err)
			testutil.AssertStatus(t, rec, 206)
			testutil.AssertHeader(t, rec, "content-range", "bytes 12-15/16")
			if !bytes.Equal(rec.Body, v1[12:]) {
				t.Fatalf("body %q, want %q", rec.Body, v1[12:])
			}
		})
		t.Run(name+"/head-ignores-range", func(t *testing.T) {
			rec, err := testutil.RunMiddlewareWithMethod(t, mw, "HEAD", "/dl.bin",
				celeristest.WithHeader("range", "bytes=2-5"))
			testutil.AssertNoError(t, err)
			testutil.AssertStatus(t, rec, 200)
			testutil.AssertNoHeader(t, rec, "content-range")
		})
		t.Run(name+"/304-before-range", func(t *testing.T) {
			rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/dl.bin")
			testutil.AssertNoError(t, err)
			etag := rec.Header("etag")
			rec, err = testutil.RunMiddlewareWithMethod(t, mw, "GET", "/dl.bin",
				celeristest.WithHeader("if-none-match", etag), celeristest.WithHeader("range", "bytes=100-"))
			testutil.AssertNoError(t, err)
			testutil.AssertStatus(t, rec, 304)
		})
	}
}
