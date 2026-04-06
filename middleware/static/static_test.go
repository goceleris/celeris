package static

import (
	"fmt"
	"io"
	"io/fs"
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

func noopHandler(c *celeris.Context) error {
	return c.String(200, "fallthrough")
}

func setupTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	_ = os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello world"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>index</html>"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "style.css"), []byte("body{}"), 0o644)

	sub := filepath.Join(dir, "sub")
	_ = os.Mkdir(sub, 0o755)
	_ = os.WriteFile(filepath.Join(sub, "page.html"), []byte("<html>page</html>"), 0o644)
	_ = os.WriteFile(filepath.Join(sub, "index.html"), []byte("<html>sub index</html>"), 0o644)

	return dir
}

func TestServeOSFile(t *testing.T) {
	dir := setupTempDir(t)
	mw := New(Config{Root: dir})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/hello.txt")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if rec.BodyString() != "hello world" {
		t.Fatalf("body: got %q, want %q", rec.BodyString(), "hello world")
	}
}

func TestServeOSIndex(t *testing.T) {
	dir := setupTempDir(t)
	mw := New(Config{Root: dir})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if rec.BodyString() != "<html>index</html>" {
		t.Fatalf("body: got %q, want %q", rec.BodyString(), "<html>index</html>")
	}
}

func TestServeOSSubdirIndex(t *testing.T) {
	dir := setupTempDir(t)
	mw := New(Config{Root: dir})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/sub/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if rec.BodyString() != "<html>sub index</html>" {
		t.Fatalf("body: got %q, want %q", rec.BodyString(), "<html>sub index</html>")
	}
}

func TestNotFoundFallsThrough(t *testing.T) {
	dir := setupTempDir(t)
	mw := New(Config{Root: dir})
	chain := []celeris.HandlerFunc{mw, noopHandler}

	rec, err := testutil.RunChain(t, chain, "GET", "/nonexistent.txt")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if rec.BodyString() != "fallthrough" {
		t.Fatalf("body: got %q, want %q", rec.BodyString(), "fallthrough")
	}
}

func TestPostMethodFallsThrough(t *testing.T) {
	dir := setupTempDir(t)
	mw := New(Config{Root: dir})
	chain := []celeris.HandlerFunc{mw, noopHandler}

	rec, err := testutil.RunChain(t, chain, "POST", "/hello.txt")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if rec.BodyString() != "fallthrough" {
		t.Fatalf("body: got %q, want %q", rec.BodyString(), "fallthrough")
	}
}

func TestPrefix(t *testing.T) {
	dir := setupTempDir(t)
	mw := New(Config{Root: dir, Prefix: "/static"})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/static/hello.txt")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if rec.BodyString() != "hello world" {
		t.Fatalf("body: got %q, want %q", rec.BodyString(), "hello world")
	}
}

func TestPrefixSegmentBoundary(t *testing.T) {
	dir := setupTempDir(t)
	mw := New(Config{Root: dir, Prefix: "/api"})
	chain := []celeris.HandlerFunc{mw, noopHandler}

	// /api-docs should NOT match prefix /api
	rec, err := testutil.RunChain(t, chain, "GET", "/api-docs/hello.txt")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if rec.BodyString() != "fallthrough" {
		t.Fatalf("expected fallthrough for /api-docs, got %q", rec.BodyString())
	}

	// /api/hello.txt SHOULD match prefix /api
	rec2, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/api/hello.txt")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec2, 200)
	if rec2.BodyString() != "hello world" {
		t.Fatalf("body: got %q, want %q", rec2.BodyString(), "hello world")
	}
}

func TestPrefixExactMatch(t *testing.T) {
	dir := setupTempDir(t)
	mw := New(Config{Root: dir, Prefix: "/static"})

	// /static with no trailing path should serve index
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/static")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if rec.BodyString() != "<html>index</html>" {
		t.Fatalf("body: got %q, want %q", rec.BodyString(), "<html>index</html>")
	}
}

func TestBrowseXSS(t *testing.T) {
	dir := t.TempDir()
	// Create a file with a javascript: filename
	_ = os.WriteFile(filepath.Join(dir, "javascript:alert(1)"), []byte("xss"), 0o644)

	mw := New(Config{Root: dir, Browse: true})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	body := rec.BodyString()
	// The href MUST NOT start with a bare "javascript:" protocol.
	// The ./ prefix forces the browser to treat it as a relative path,
	// preventing protocol-scheme injection even though url.PathEscape
	// does not encode the colon.
	if strings.Contains(body, `href="javascript:`) {
		t.Fatal("XSS: href starts with javascript: protocol (missing ./ prefix)")
	}
	// The href should use ./ prefix + URL-encoded name.
	if !strings.Contains(body, `href="./javascript:alert%281%29"`) {
		t.Fatalf("expected ./ prefixed URL-encoded href, got body:\n%s", body)
	}
	// The display text should show the original filename.
	if !strings.Contains(body, "javascript:alert(1)") {
		t.Fatal("display text should contain the original filename")
	}
}

func TestBrowseDirectoryListing(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644)
	_ = os.Mkdir(filepath.Join(dir, "subdir"), 0o755)

	mw := New(Config{Root: dir, Browse: true})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	body := rec.BodyString()
	if !strings.Contains(body, "a.txt") {
		t.Fatal("listing should contain a.txt")
	}
	if !strings.Contains(body, "subdir/") {
		t.Fatal("listing should contain subdir/")
	}
}

func TestBrowseDisabledFallsThrough(t *testing.T) {
	dir := t.TempDir()
	// No index file, browse disabled
	mw := New(Config{Root: dir})
	chain := []celeris.HandlerFunc{mw, noopHandler}

	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if rec.BodyString() != "fallthrough" {
		t.Fatalf("expected fallthrough, got %q", rec.BodyString())
	}
}

func TestETagLastModified(t *testing.T) {
	dir := setupTempDir(t)
	mw := New(Config{Root: dir})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/hello.txt")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	lm := rec.Header("last-modified")
	if lm == "" {
		t.Fatal("expected last-modified header")
	}
	_, parseErr := http.ParseTime(lm)
	if parseErr != nil {
		t.Fatalf("invalid last-modified: %q: %v", lm, parseErr)
	}

	etag := rec.Header("etag")
	if etag == "" {
		t.Fatal("expected etag header")
	}
	if !strings.HasPrefix(etag, `W/"`) {
		t.Fatalf("expected weak etag, got %q", etag)
	}
}

func TestIfModifiedSince304(t *testing.T) {
	dir := setupTempDir(t)
	mw := New(Config{Root: dir})

	// First request to get Last-Modified.
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/hello.txt")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	lm := rec.Header("last-modified")

	// Conditional request with the same Last-Modified.
	rec, err = testutil.RunMiddlewareWithMethod(t, mw, "GET", "/hello.txt",
		celeristest.WithHeader("if-modified-since", lm))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 304)
}

func TestIfNoneMatch304(t *testing.T) {
	dir := setupTempDir(t)
	mw := New(Config{Root: dir})

	// First request to get ETag.
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/hello.txt")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	etag := rec.Header("etag")

	// Conditional request with the same ETag.
	rec, err = testutil.RunMiddlewareWithMethod(t, mw, "GET", "/hello.txt",
		celeristest.WithHeader("if-none-match", etag))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 304)
}

func TestServeFSFile(t *testing.T) {
	fsys := fstest.MapFS{
		"hello.txt": {Data: []byte("hello fs")},
	}
	mw := New(Config{FS: fsys})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/hello.txt")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if rec.BodyString() != "hello fs" {
		t.Fatalf("body: got %q, want %q", rec.BodyString(), "hello fs")
	}
}

func TestServeFSIndex(t *testing.T) {
	fsys := fstest.MapFS{
		"index.html": {Data: []byte("<html>fs index</html>")},
	}
	mw := New(Config{FS: fsys})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if rec.BodyString() != "<html>fs index</html>" {
		t.Fatalf("body: got %q, want %q", rec.BodyString(), "<html>fs index</html>")
	}
}

func TestRangeRequestFS(t *testing.T) {
	fsys := fstest.MapFS{
		"data.txt": {Data: []byte("0123456789abcdef")},
	}
	mw := New(Config{FS: fsys})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/data.txt",
		celeristest.WithHeader("range", "bytes=0-4"))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 206)
	if rec.BodyString() != "01234" {
		t.Fatalf("body: got %q, want %q", rec.BodyString(), "01234")
	}
	cr := rec.Header("content-range")
	if cr != "bytes 0-4/16" {
		t.Fatalf("content-range: got %q, want %q", cr, "bytes 0-4/16")
	}
}

func TestRangeRequestFSSuffix(t *testing.T) {
	fsys := fstest.MapFS{
		"data.txt": {Data: []byte("0123456789abcdef")},
	}
	mw := New(Config{FS: fsys})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/data.txt",
		celeristest.WithHeader("range", "bytes=-4"))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 206)
	if rec.BodyString() != "cdef" {
		t.Fatalf("body: got %q, want %q", rec.BodyString(), "cdef")
	}
}

func TestRangeRequestFSOpenEnd(t *testing.T) {
	fsys := fstest.MapFS{
		"data.txt": {Data: []byte("0123456789abcdef")},
	}
	mw := New(Config{FS: fsys})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/data.txt",
		celeristest.WithHeader("range", "bytes=12-"))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 206)
	if rec.BodyString() != "cdef" {
		t.Fatalf("body: got %q, want %q", rec.BodyString(), "cdef")
	}
}

func TestRangeRequestFSInvalid(t *testing.T) {
	fsys := fstest.MapFS{
		"data.txt": {Data: []byte("0123456789")},
	}
	mw := New(Config{FS: fsys})

	// Invalid range should return full content.
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/data.txt",
		celeristest.WithHeader("range", "bytes=50-100"))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if rec.BodyString() != "0123456789" {
		t.Fatalf("body: got %q, want %q", rec.BodyString(), "0123456789")
	}
}

func TestFSCacheHeadersNonZeroModTime(t *testing.T) {
	modTime := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	fsys := fstest.MapFS{
		"hello.txt": {
			Data:    []byte("hello"),
			ModTime: modTime,
		},
	}
	mw := New(Config{FS: fsys})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/hello.txt")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	lm := rec.Header("last-modified")
	if lm == "" {
		t.Fatal("expected last-modified header for non-zero ModTime")
	}
	etag := rec.Header("etag")
	if etag == "" {
		t.Fatal("expected etag header for non-zero ModTime")
	}
}

func TestFSCacheHeadersZeroModTime(t *testing.T) {
	fsys := fstest.MapFS{
		"hello.txt": {Data: []byte("hello")},
	}
	mw := New(Config{FS: fsys})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/hello.txt")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	lm := rec.Header("last-modified")
	if lm != "" {
		t.Fatalf("expected no last-modified for zero ModTime, got %q", lm)
	}
	etag := rec.Header("etag")
	if etag != "" {
		t.Fatalf("expected no etag for zero ModTime, got %q", etag)
	}
}

func TestFS304WithIfNoneMatch(t *testing.T) {
	modTime := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	fsys := fstest.MapFS{
		"hello.txt": {
			Data:    []byte("hello"),
			ModTime: modTime,
		},
	}
	mw := New(Config{FS: fsys})

	// Get ETag from first request.
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/hello.txt")
	testutil.AssertNoError(t, err)
	etag := rec.Header("etag")

	// Conditional request returns 304.
	rec, err = testutil.RunMiddlewareWithMethod(t, mw, "GET", "/hello.txt",
		celeristest.WithHeader("if-none-match", etag))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 304)
}

func TestSkipPaths(t *testing.T) {
	dir := setupTempDir(t)
	mw := New(Config{Root: dir, SkipPaths: []string{"/hello.txt"}})
	chain := []celeris.HandlerFunc{mw, noopHandler}

	rec, err := testutil.RunChain(t, chain, "GET", "/hello.txt")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if rec.BodyString() != "fallthrough" {
		t.Fatalf("expected fallthrough on skipped path, got %q", rec.BodyString())
	}
}

func TestSkipFunc(t *testing.T) {
	dir := setupTempDir(t)
	mw := New(Config{
		Root: dir,
		Skip: func(c *celeris.Context) bool {
			return c.Path() == "/hello.txt"
		},
	})
	chain := []celeris.HandlerFunc{mw, noopHandler}

	rec, err := testutil.RunChain(t, chain, "GET", "/hello.txt")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if rec.BodyString() != "fallthrough" {
		t.Fatalf("expected fallthrough on skipped path, got %q", rec.BodyString())
	}
}

func TestBrowseFSListing(t *testing.T) {
	fsys := fstest.MapFS{
		"dir/a.txt": {Data: []byte("a")},
		"dir/b.txt": {Data: []byte("b")},
	}
	mw := New(Config{FS: fsys, Browse: true})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/dir/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	body := rec.BodyString()
	if !strings.Contains(body, "a.txt") {
		t.Fatal("listing should contain a.txt")
	}
	if !strings.Contains(body, "b.txt") {
		t.Fatal("listing should contain b.txt")
	}
}

func TestBrowseXSSFS(t *testing.T) {
	fsys := fstest.MapFS{
		"dir/javascript:alert(1)": {Data: []byte("xss")},
	}
	mw := New(Config{FS: fsys, Browse: true})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/dir/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	body := rec.BodyString()
	if strings.Contains(body, `href="javascript:`) {
		t.Fatal("XSS: href starts with javascript: protocol (missing ./ prefix)")
	}
	if !strings.Contains(body, `href="./javascript:alert%281%29"`) {
		t.Fatalf("expected ./ prefixed URL-encoded href in FS listing, got body:\n%s", body)
	}
}

func TestPathTraversalBrowse(t *testing.T) {
	dir := setupTempDir(t)
	mw := New(Config{Root: dir, Browse: true})
	chain := []celeris.HandlerFunc{mw, noopHandler}

	// Attempt directory traversal via ".." — must fall through, not list parent dirs.
	paths := []string{
		"/../",
		"/../../",
		"/sub/../../",
		"/sub/../../../etc/",
	}
	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			rec, err := testutil.RunChain(t, chain, "GET", p)
			testutil.AssertNoError(t, err)
			testutil.AssertStatus(t, rec, 200)
			if rec.BodyString() != "fallthrough" {
				t.Fatalf("path %q should fall through, got body: %s", p, rec.BodyString())
			}
		})
	}
}

func TestPathTraversalFile(t *testing.T) {
	dir := setupTempDir(t)
	mw := New(Config{Root: dir})
	chain := []celeris.HandlerFunc{mw, noopHandler}

	// Attempt file traversal via ".." — must fall through.
	rec, err := testutil.RunChain(t, chain, "GET", "/../../../etc/passwd")
	testutil.AssertNoError(t, err)
	// Should either fall through or return an error, never serve the file.
	if rec.BodyString() != "fallthrough" {
		body := rec.BodyString()
		if strings.Contains(body, "root:") {
			t.Fatal("path traversal: served /etc/passwd!")
		}
	}
}

func TestConfigPanicNoRootNoFS(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for empty config")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "Root or FS must be set") {
			t.Fatalf("unexpected panic: %v", r)
		}
	}()
	New(Config{})
}

func TestRenderListingParentLink(t *testing.T) {
	listing := renderListing("/sub/dir/", nil)
	if !strings.Contains(listing, `href="../"`) {
		t.Fatal("expected parent directory link")
	}

	listing = renderListing("/", nil)
	if strings.Contains(listing, `href="../"`) {
		t.Fatal("root path should not have parent link")
	}
}

func TestRenderListingHTMLEscape(t *testing.T) {
	entries := []fs.DirEntry{
		&fakeDirEntry{name: `<script>alert("xss")</script>.txt`},
	}
	listing := renderListing("/", entries)
	if strings.Contains(listing, `<script>`) {
		t.Fatal("display name must be HTML-escaped")
	}
	if !strings.Contains(listing, "&lt;script&gt;") {
		t.Fatal("expected HTML-escaped display name")
	}
}

func TestParseByteRange(t *testing.T) {
	tests := []struct {
		header string
		size   int64
		start  int64
		end    int64
		ok     bool
	}{
		{"bytes=0-4", 10, 0, 4, true},
		{"bytes=5-9", 10, 5, 9, true},
		{"bytes=-3", 10, 7, 9, true},
		{"bytes=7-", 10, 7, 9, true},
		{"bytes=0-0", 10, 0, 0, true},
		// Invalid ranges
		{"bytes=10-15", 10, 0, 0, false},
		{"bytes=-0", 10, 0, 0, false},
		{"bytes=5-3", 10, 0, 0, false},
		{"chars=0-4", 10, 0, 0, false},
		{"", 10, 0, 0, false},
		{"bytes=abc-def", 10, 0, 0, false},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s/%d", tt.header, tt.size), func(t *testing.T) {
			start, end, ok := parseByteRange(tt.header, tt.size)
			if ok != tt.ok {
				t.Fatalf("ok: got %v, want %v", ok, tt.ok)
			}
			if ok {
				if start != tt.start || end != tt.end {
					t.Fatalf("range: got %d-%d, want %d-%d", start, end, tt.start, tt.end)
				}
			}
		})
	}
}

func TestAcceptRangesHeaderFS(t *testing.T) {
	fsys := fstest.MapFS{
		"file.txt": {Data: []byte("content")},
	}
	mw := New(Config{FS: fsys})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/file.txt")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	ar := rec.Header("accept-ranges")
	if ar != "bytes" {
		t.Fatalf("accept-ranges: got %q, want %q", ar, "bytes")
	}
}

func TestHeadRequest(t *testing.T) {
	dir := setupTempDir(t)
	mw := New(Config{Root: dir})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "HEAD", "/hello.txt")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	etag := rec.Header("etag")
	if etag == "" {
		t.Fatal("expected etag header on HEAD request")
	}
	lm := rec.Header("last-modified")
	if lm == "" {
		t.Fatal("expected last-modified header on HEAD request")
	}
}

func TestSPAMode(t *testing.T) {
	dir := setupTempDir(t)
	mw := New(Config{Root: dir, SPA: true})

	// Non-existent file should serve index.html in SPA mode.
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/app/dashboard")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if rec.BodyString() != "<html>index</html>" {
		t.Fatalf("body: got %q, want %q", rec.BodyString(), "<html>index</html>")
	}
}

func TestSPAModeFS(t *testing.T) {
	fsys := fstest.MapFS{
		"index.html": {Data: []byte("<html>spa</html>")},
	}
	mw := New(Config{FS: fsys, SPA: true})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/app/dashboard")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if rec.BodyString() != "<html>spa</html>" {
		t.Fatalf("body: got %q, want %q", rec.BodyString(), "<html>spa</html>")
	}
}

func TestSPAModeDisabled(t *testing.T) {
	dir := setupTempDir(t)
	mw := New(Config{Root: dir})
	chain := []celeris.HandlerFunc{mw, noopHandler}

	rec, err := testutil.RunChain(t, chain, "GET", "/app/dashboard")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if rec.BodyString() != "fallthrough" {
		t.Fatalf("expected fallthrough with SPA disabled, got %q", rec.BodyString())
	}
}

func TestMaxAge(t *testing.T) {
	dir := setupTempDir(t)
	mw := New(Config{Root: dir, MaxAge: 24 * time.Hour})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/hello.txt")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	cc := rec.Header("cache-control")
	if cc != "public, max-age=86400" {
		t.Fatalf("cache-control: got %q, want %q", cc, "public, max-age=86400")
	}
}

func TestMaxAgeZero(t *testing.T) {
	dir := setupTempDir(t)
	mw := New(Config{Root: dir})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/hello.txt")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	cc := rec.Header("cache-control")
	if cc != "" {
		t.Fatalf("expected no cache-control header, got %q", cc)
	}
}

func TestHeadRequestFS(t *testing.T) {
	modTime := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	fsys := fstest.MapFS{
		"hello.txt": {
			Data:    []byte("hello fs"),
			ModTime: modTime,
		},
	}
	mw := New(Config{FS: fsys})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "HEAD", "/hello.txt")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	etag := rec.Header("etag")
	if etag == "" {
		t.Fatal("expected etag header on HEAD request")
	}
	lm := rec.Header("last-modified")
	if lm == "" {
		t.Fatal("expected last-modified header on HEAD request")
	}
}

func TestFS304WithIfModifiedSince(t *testing.T) {
	modTime := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	fsys := fstest.MapFS{
		"hello.txt": {
			Data:    []byte("hello"),
			ModTime: modTime,
		},
	}
	mw := New(Config{FS: fsys})

	// First request to get Last-Modified.
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/hello.txt")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	lm := rec.Header("last-modified")

	// Conditional request returns 304.
	rec, err = testutil.RunMiddlewareWithMethod(t, mw, "GET", "/hello.txt",
		celeristest.WithHeader("if-modified-since", lm))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 304)
}

func TestIfNoneMatchMultipleETags(t *testing.T) {
	dir := setupTempDir(t)
	mw := New(Config{Root: dir})

	// First request to get ETag.
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/hello.txt")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	etag := rec.Header("etag")

	// Send comma-separated list of ETags including the matching one.
	multiETags := fmt.Sprintf(`"nonexistent", %s, "other"`, etag)
	rec, err = testutil.RunMiddlewareWithMethod(t, mw, "GET", "/hello.txt",
		celeristest.WithHeader("if-none-match", multiETags))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 304)

	// Non-matching multi-ETags should return 200.
	rec, err = testutil.RunMiddlewareWithMethod(t, mw, "GET", "/hello.txt",
		celeristest.WithHeader("if-none-match", `"aaa", "bbb"`))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestPrefixValidation(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for prefix without leading /")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "Prefix must start with /") {
			t.Fatalf("unexpected panic: %v", r)
		}
	}()
	New(Config{Root: "/tmp", Prefix: "bad"})
}

// fakeDirEntry implements fs.DirEntry for testing.
type fakeDirEntry struct {
	name  string
	isDir bool
}

func (f *fakeDirEntry) Name() string               { return f.name }
func (f *fakeDirEntry) IsDir() bool                { return f.isDir }
func (f *fakeDirEntry) Type() fs.FileMode          { return 0 }
func (f *fakeDirEntry) Info() (fs.FileInfo, error) { return nil, nil }

// --- ReadSeeker fs.FS for testing ---

// seekerFS is an fs.FS whose files implement io.ReadSeeker.
type seekerFS struct {
	files map[string]*seekerFileData
}

type seekerFileData struct {
	data    []byte
	modTime time.Time
}

func (s *seekerFS) Open(name string) (fs.File, error) {
	fd, ok := s.files[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return &seekerFile{data: fd.data, modTime: fd.modTime, name: name}, nil
}

// seekerFile implements fs.File and io.ReadSeeker.
type seekerFile struct {
	data    []byte
	modTime time.Time
	name    string
	offset  int64
}

func (f *seekerFile) Read(p []byte) (int, error) {
	if f.offset >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.data[f.offset:])
	f.offset += int64(n)
	return n, nil
}

func (f *seekerFile) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = f.offset + offset
	case io.SeekEnd:
		abs = int64(len(f.data)) + offset
	default:
		return 0, fmt.Errorf("seekerFile.Seek: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("seekerFile.Seek: negative position")
	}
	f.offset = abs
	return abs, nil
}

func (f *seekerFile) Stat() (fs.FileInfo, error) {
	return &seekerFileInfo{name: f.name, size: int64(len(f.data)), modTime: f.modTime}, nil
}

func (f *seekerFile) Close() error { return nil }

type seekerFileInfo struct {
	name    string
	size    int64
	modTime time.Time
}

func (fi *seekerFileInfo) Name() string       { return fi.name }
func (fi *seekerFileInfo) Size() int64        { return fi.size }
func (fi *seekerFileInfo) Mode() fs.FileMode  { return 0o444 }
func (fi *seekerFileInfo) ModTime() time.Time { return fi.modTime }
func (fi *seekerFileInfo) IsDir() bool        { return false }
func (fi *seekerFileInfo) Sys() any           { return nil }

func TestServeFSReadSeeker(t *testing.T) {
	data := []byte("0123456789abcdef")
	fsys := &seekerFS{files: map[string]*seekerFileData{
		"data.txt": {data: data, modTime: time.Date(2025, 6, 15, 0, 0, 0, 0, time.UTC)},
	}}
	mw := New(Config{FS: fsys})

	// Full request.
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/data.txt")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if rec.BodyString() != "0123456789abcdef" {
		t.Fatalf("body: got %q, want %q", rec.BodyString(), "0123456789abcdef")
	}
	ar := rec.Header("accept-ranges")
	if ar != "bytes" {
		t.Fatalf("accept-ranges: got %q, want %q", ar, "bytes")
	}

	// Range request: only requested bytes should be served.
	rec, err = testutil.RunMiddlewareWithMethod(t, mw, "GET", "/data.txt",
		celeristest.WithHeader("range", "bytes=4-7"))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 206)
	if rec.BodyString() != "4567" {
		t.Fatalf("body: got %q, want %q", rec.BodyString(), "4567")
	}
	cr := rec.Header("content-range")
	if cr != "bytes 4-7/16" {
		t.Fatalf("content-range: got %q, want %q", cr, "bytes 4-7/16")
	}
}

func TestContentTypeSniffing(t *testing.T) {
	// PNG magic bytes: 8-byte signature.
	pngHeader := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	pngData := make([]byte, 64)
	copy(pngData, pngHeader)

	dir := t.TempDir()
	// File with no extension — content-type must be sniffed.
	_ = os.WriteFile(filepath.Join(dir, "image"), pngData, 0o644)

	mw := New(Config{Root: dir})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/image")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	// OS path uses c.FileFromDir which does its own content-type detection.
	// We just verify it serves successfully (OS-level sniffing is in the core).
	if rec.BodyString() != string(pngData) {
		t.Fatalf("body length: got %d, want %d", len(rec.BodyString()), len(pngData))
	}
}

func TestContentTypeSniffingFS(t *testing.T) {
	// PNG magic bytes.
	pngHeader := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	pngData := make([]byte, 64)
	copy(pngData, pngHeader)

	// fstest.MapFS does NOT implement ReadSeeker, tests the non-ReadSeeker path.
	fsys := fstest.MapFS{
		"image": {Data: pngData},
	}
	mw := New(Config{FS: fsys})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/image")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	ct := rec.Header("content-type")
	if !strings.Contains(ct, "image/png") {
		t.Fatalf("content-type: got %q, want image/png", ct)
	}
}

func TestContentTypeSniffingFSReadSeeker(t *testing.T) {
	// PNG magic bytes — test the ReadSeeker path.
	pngHeader := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	pngData := make([]byte, 64)
	copy(pngData, pngHeader)

	fsys := &seekerFS{files: map[string]*seekerFileData{
		"image": {data: pngData, modTime: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)},
	}}
	mw := New(Config{FS: fsys})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/image")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	ct := rec.Header("content-type")
	if !strings.Contains(ct, "image/png") {
		t.Fatalf("content-type: got %q, want image/png", ct)
	}
}

func TestPreCompressedBrotli(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "app.js"), []byte("console.log('hello')"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "app.js.br"), []byte("br-compressed-data"), 0o644)

	mw := New(Config{Root: dir, Compress: true})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/app.js",
		celeristest.WithHeader("accept-encoding", "gzip, br"))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	ce := rec.Header("content-encoding")
	if ce != "br" {
		t.Fatalf("content-encoding: got %q, want %q", ce, "br")
	}
	vary := rec.Header("vary")
	if !strings.Contains(vary, "Accept-Encoding") {
		t.Fatalf("vary: got %q, want it to contain Accept-Encoding", vary)
	}
	if rec.BodyString() != "br-compressed-data" {
		t.Fatalf("body: got %q, want %q", rec.BodyString(), "br-compressed-data")
	}
}

func TestPreCompressedGzip(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "app.js"), []byte("console.log('hello')"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "app.js.gz"), []byte("gzip-compressed-data"), 0o644)

	mw := New(Config{Root: dir, Compress: true})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/app.js",
		celeristest.WithHeader("accept-encoding", "gzip"))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	ce := rec.Header("content-encoding")
	if ce != "gzip" {
		t.Fatalf("content-encoding: got %q, want %q", ce, "gzip")
	}
	vary := rec.Header("vary")
	if !strings.Contains(vary, "Accept-Encoding") {
		t.Fatalf("vary: got %q, want it to contain Accept-Encoding", vary)
	}
	if rec.BodyString() != "gzip-compressed-data" {
		t.Fatalf("body: got %q, want %q", rec.BodyString(), "gzip-compressed-data")
	}
}

func TestPreCompressedNotAccepted(t *testing.T) {
	dir := t.TempDir()
	original := "console.log('hello')"
	_ = os.WriteFile(filepath.Join(dir, "app.js"), []byte(original), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "app.js.br"), []byte("br-compressed"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "app.js.gz"), []byte("gz-compressed"), 0o644)

	mw := New(Config{Root: dir, Compress: true})

	// No Accept-Encoding header — should serve the original file.
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/app.js")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	ce := rec.Header("content-encoding")
	if ce != "" {
		t.Fatalf("expected no content-encoding, got %q", ce)
	}
	if rec.BodyString() != original {
		t.Fatalf("body: got %q, want %q", rec.BodyString(), original)
	}
}

func TestPreCompressedMissing(t *testing.T) {
	dir := t.TempDir()
	original := "console.log('hello')"
	_ = os.WriteFile(filepath.Join(dir, "app.js"), []byte(original), 0o644)
	// No .br or .gz files exist.

	mw := New(Config{Root: dir, Compress: true})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/app.js",
		celeristest.WithHeader("accept-encoding", "gzip, br"))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	ce := rec.Header("content-encoding")
	if ce != "" {
		t.Fatalf("expected no content-encoding, got %q", ce)
	}
	if rec.BodyString() != original {
		t.Fatalf("body: got %q, want %q", rec.BodyString(), original)
	}
}
