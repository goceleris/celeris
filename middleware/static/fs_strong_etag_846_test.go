package static

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"net/http"
	"testing"
	"testing/fstest"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/middleware/internal/testutil"
)

// Tests for celeris#846 item 2c: the ETag of a file served from an fs.FS is a
// strong validator, a hash of the bytes taken when the file is first read, so
// an If-Range ETag can resume safely where the mtime says nothing (build
// tools that normalise mtimes: ko, Nix, Bazel rules_oci, SOURCE_DATE_EPOCH).

// normTime846 is the mtime such tools give every file.
var normTime846 = time.Unix(1, 0).UTC()

func strongTag846(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf(`"%x"`, sum[:16])
}

func fsGet846(t *testing.T, mw celeris.HandlerFunc, hdr ...string) *celeristest.ResponseRecorder {
	t.Helper()
	opts := make([]celeristest.Option, 0, len(hdr)/2)
	for i := 0; i+1 < len(hdr); i += 2 {
		opts = append(opts, celeristest.WithHeader(hdr[i], hdr[i+1]))
	}
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/dl.bin", opts...)
	testutil.AssertNoError(t, err)
	return rec
}

// sameShape846 builds two versions of the same size, so mtime and size cannot
// tell them apart; they differ at every byte.
func sameShape846(t *testing.T) (v1, v2 []byte) {
	t.Helper()
	v1, v2 = version435(100, 0), version435(100, 13)
	for i := range v1 {
		if v1[i] == v2[i] {
			t.Fatalf("fixture: versions share byte %d", i)
		}
	}
	return v1, v2
}

func TestFSETagIsAContentHash846(t *testing.T) {
	v1, v2 := sameShape846(t)
	mw1 := New(Config{FS: fstest.MapFS{"dl.bin": {Data: v1, ModTime: normTime846}}})
	mw2 := New(Config{FS: fstest.MapFS{"dl.bin": {Data: v2, ModTime: normTime846}}})

	e1 := fsGet846(t, mw1).Header("etag")
	if e1 != strongTag846(v1) {
		t.Fatalf("etag %q, want the strong tag of the bytes %q", e1, strongTag846(v1))
	}
	// Cache fill and cache hit agree.
	if again := fsGet846(t, mw1).Header("etag"); again != e1 {
		t.Fatalf("second request etag %q, first %q", again, e1)
	}
	// A new instance (a restart, another replica) derives the same tag.
	if again := fsGet846(t, New(Config{FS: fstest.MapFS{"dl.bin": {Data: v1, ModTime: normTime846}}})).Header("etag"); again != e1 {
		t.Fatalf("new instance etag %q, first %q", again, e1)
	}
	// The other version has the same mtime and size, and another tag.
	rec2 := fsGet846(t, mw2)
	if rec2.Header("last-modified") != fsGet846(t, mw1).Header("last-modified") {
		t.Fatal("fixture: the versions' Last-Modified differ")
	}
	if e2 := rec2.Header("etag"); e2 != strongTag846(v2) || e2 == e1 {
		t.Fatalf("v2 etag %q, want %q (not v1's %q)", e2, strongTag846(v2), e1)
	}
}

// TestFSResumeAfterRedeploy846: the client's If-Range ETag resumes against
// the version it came from, and restarts against another version with the
// same mtime and size.
func TestFSResumeAfterRedeploy846(t *testing.T) {
	v1, v2 := sameShape846(t)
	mw1 := New(Config{FS: fstest.MapFS{"dl.bin": {Data: v1, ModTime: normTime846}}})
	mw2 := New(Config{FS: fstest.MapFS{"dl.bin": {Data: v2, ModTime: normTime846}}})

	first := fsGet846(t, mw1)
	etag1, lm1 := first.Header("etag"), first.Header("last-modified")

	// The same version: the range.
	rec := fsGet846(t, mw1, "range", "bytes=60-", "if-range", etag1)
	testutil.AssertStatus(t, rec, 206)
	testutil.AssertHeader(t, rec, "content-range", "bytes 60-99/100")
	if !bytes.Equal(rec.Body, v1[60:]) {
		t.Fatalf("body %q, want %q", rec.Body, v1[60:])
	}

	// The redeployed version: the whole file.
	rec = fsGet846(t, mw2, "range", "bytes=60-", "if-range", etag1)
	if rec.StatusCode != 200 || !bytes.Equal(rec.Body, v2) {
		spliced := append(append([]byte(nil), v1[:60]...), rec.Body...)
		t.Fatalf("resume after a redeploy: status %d content-range %q; want 200 with the whole new file.\n"+
			"the client would now hold %q, the new file is %q", rec.StatusCode, rec.Header("content-range"), spliced, v2)
	}

	// Fixture guard: this is what the mtime alone could not tell. The old
	// Last-Modified still matches the new version, which is why the ETag, not
	// the date, did the work above. A date If-Range is only as reliable as
	// the files' mtimes (celeris#846 item 2).
	if rec := fsGet846(t, mw2, "range", "bytes=60-", "if-range", lm1); rec.StatusCode != 206 {
		t.Fatalf("fixture: the old Last-Modified no longer matches the new version (status %d)", rec.StatusCode)
	}
}

func TestFSStrongETagConditional846(t *testing.T) {
	v1, _ := sameShape846(t)
	tag := strongTag846(v1)
	// The first request of a fresh instance is conditional: the tag is
	// computed from the bytes, which are read for it.
	for name, inm := range map[string]string{
		"strong": tag, "weak-form": "W/" + tag, "list": `"other", ` + tag, "star": "*",
	} {
		t.Run(name, func(t *testing.T) {
			mw := New(Config{FS: fstest.MapFS{"dl.bin": {Data: v1, ModTime: normTime846}}})
			rec := fsGet846(t, mw, "if-none-match", inm)
			testutil.AssertStatus(t, rec, 304)
			testutil.AssertHeader(t, rec, "etag", tag)
			if len(rec.Body) != 0 {
				t.Fatalf("304 with a %d byte body", len(rec.Body))
			}
			// The 304 filled the cache: a second one is the same.
			testutil.AssertStatus(t, fsGet846(t, mw, "if-none-match", inm), 304)
		})
	}
	mw := New(Config{FS: fstest.MapFS{"dl.bin": {Data: v1, ModTime: normTime846}}})
	rec := fsGet846(t, mw, "if-none-match", `"other"`)
	testutil.AssertStatus(t, rec, 200)
	if !bytes.Equal(rec.Body, v1) {
		t.Fatalf("body %q, want %q", rec.Body, v1)
	}
}

// TestFSETagFollowsTheFile846: when the file's mtime moves, the cache entry is
// replaced and the tag is the new bytes'.
func TestFSETagFollowsTheFile846(t *testing.T) {
	v1, v2 := sameShape846(t)
	fsys := fstest.MapFS{"dl.bin": {Data: v1, ModTime: v1Time435}}
	mw := New(Config{FS: fsys})
	testutil.AssertHeader(t, fsGet846(t, mw), "etag", strongTag846(v1))
	fsys["dl.bin"] = &fstest.MapFile{Data: v2, ModTime: v2Time435}
	rec := fsGet846(t, mw)
	testutil.AssertHeader(t, rec, "etag", strongTag846(v2))
	testutil.AssertHeader(t, rec, "last-modified", v2Time435.Format(http.TimeFormat))
}
